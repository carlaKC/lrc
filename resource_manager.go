package lrc

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/queue"
)

// MaxMilliSatoshi is the maximum amount of millisatoshi that can possibly
// exist given 21 million bitcoin cap.
const MaxMilliSatoshi = 21_000_000 * 10_000_0000 * 1000

var (
	// ErrChannelNotFound is returned when we don't have a channel
	// tracked in our internal state.
	ErrChannelNotFound = errors.New("channel not found")
)

// Compile time check that ReputationManager implements the
// LocalReputationManager interface.
var _ LocalResourceManager = (*ResourceManager)(nil)

// ResourceManager tracks local reputation earned by incoming channels, and
// the thresholds required to earn endorsement on the outgoing channels
// required to implement resource bucketing for a node's channels.
type ResourceManager struct {
	// Parameters for reputation algorithm.
	params ManagerParams

	// incomingRevenue maps channel ids to decaying averages of the
	// revenue that individual channels have earned the local node as
	// incoming channels.

	// channelReputation tracks information required to track channel
	// reputation:
	// - The revenue that the channel has earned the local node forwarding
	//   *incoming* HTLCs.
	// - The incoming HTLCs that the channel has forwarded to the local
	//   node that have not yet resolved.
	channelReputation map[lnwire.ShortChannelID]reputationMonitor

	// channelRevenue tracks the routing revenue that channels have
	// earned the local node for both incoming and outgoing HTLCs.
	channelRevenue map[lnwire.ShortChannelID]revenueMonitor

	// resolutionPeriod is the period of time that is considered reasonable
	// for a htlc to resolve in.
	resolutionPeriod time.Duration

	// lookupReputation fetches previously persisted resolution values for
	// a channel.
	lookupReputation LookupReputation

	// lookupRevenue fetches previously persisted revenue values for
	// a channel.
	lookupRevenue LookupRevenue

	// newReputationMonitor creates a new reputation monitor, pulled
	// out for mocking purposes in tests.
	newReputationMonitor NewReputationMonitor

	// newRevenueMonitor creates a new revenue monitor, pull out for mocking
	// in tests.
	newRevenueMonitor NewRevenueMonitor

	clock clock.Clock

	log Logger

	// A single mutex guarding access to the manager.
	sync.Mutex
}

type ChannelFetcher func(lnwire.ShortChannelID) (*ChannelInfo, error)

// LookupReputation is the function signature for fetching a decaying average
// start value for the give channel's reputation. If not history is available
// it is expected to return nil.
type LookupReputation func(id lnwire.ShortChannelID) (*DecayingAverageStart,
	error)

// LookupReputation is the function signature for fetching a decaying average
// start value for the give channel's reputation. If not history is available
// it is expected to return nil.
type LookupRevenue func(id lnwire.ShortChannelID) (*DecayingAverageStart,
	error)

// NewReputationMonitor is a function signature for a constructor that creates
// a new reputation monitor.
type NewReputationMonitor func(start *DecayingAverageStart) reputationMonitor

// NewRevenueMonitor is a function signature for a constructor that creates
// a new revenue monitor.
type NewRevenueMonitor func(start *DecayingAverageStart,
	chanInfo *ChannelInfo) (revenueMonitor, error)

type ManagerParams struct {
	// RevenueWindow is the amount of time that we examine the revenue of
	// outgoing links over.
	RevenueWindow time.Duration

	// ReputationMultiplier is the multiplier on RevenueWindow that is
	// used to determine the longer period of time that incoming links
	// reputation is assessed over.
	ReputationMultiplier uint8

	// ProtectedPercentage is the percentage of liquidity and slots that
	// are reserved for high reputation, endorsed HTLCs.
	ProtectedPercentage uint64

	// ResolutionPeriod is the amount of time that we reasonably expect
	// HTLCs to complete within.
	ResolutionPeriod time.Duration

	// BlockTime is the expected block time.
	BlockTime time.Duration
}

// validate that we have sane parameters.
func (p *ManagerParams) validate() error {
	if p.ProtectedPercentage > 100 {
		return fmt.Errorf("Percentage: %v > 100", p.ProtectedPercentage)
	}

	if p.ResolutionPeriod == 0 {
		return errors.New("Resolution period must be > 0")
	}

	if p.BlockTime == 0 {
		return errors.New("Block time must be > 0")
	}

	return nil
}

// revenueWindow returns the period over which we examine revenue.
func (p *ManagerParams) reputationWindow() time.Duration {
	return p.RevenueWindow * time.Duration(
		p.ReputationMultiplier,
	)
}

// NewResourceManager creates a local reputation manager that will track
// channel revenue over the window provided, and incoming channel reputation
// over the window scaled by the multiplier.
func NewResourceManager(params ManagerParams, clock clock.Clock,
	lookupReputation LookupReputation,
	lookupRevenue LookupRevenue, log Logger) (*ResourceManager, error) {

	if err := params.validate(); err != nil {
		return nil, err
	}
	return &ResourceManager{
		params: params,
		channelReputation: make(
			map[lnwire.ShortChannelID]reputationMonitor,
		),
		channelRevenue: make(
			map[lnwire.ShortChannelID]revenueMonitor,
		),
		resolutionPeriod: params.ResolutionPeriod,
		lookupReputation: lookupReputation,
		lookupRevenue:    lookupRevenue,
		newReputationMonitor: func(start *DecayingAverageStart) reputationMonitor {
			return newReputationTracker(
				clock, params, log, start,
			)
		},
		newRevenueMonitor: func(start *DecayingAverageStart,
			chanInfo *ChannelInfo) (revenueMonitor, error) {

			return newRevenueTracker(
				clock, params, chanInfo, log, start,
			)

		},
		clock: clock,
		log:   log,
	}, nil
}

// getTargetChannel looks up a channel's revenue record in the reputation
// manager, creating a new decaying average if one if not found. This function
// returns a pointer to the map entry which can be used to mutate its
// underlying value.
func (r *ResourceManager) getTargetChannel(channel lnwire.ShortChannelID,
	chanInfo *ChannelInfo) (revenueMonitor, error) {

	if r.channelRevenue[channel] == nil {
		revenue, err := r.lookupRevenue(channel)
		if err != nil {
			return nil, err
		}

		r.channelRevenue[channel], err = r.newRevenueMonitor(
			revenue, chanInfo,
		)
		if err != nil {
			return nil, err
		}

		r.log.Infof("Added new revenue channel: %v with start: %v",
			channel.ToUint64(), revenue)
	}

	return r.channelRevenue[channel], nil
}

// getChannelReputation looks up a channel's reputation tracker in the
// reputation manager, creating a new tracker if one is not found. This
// function returns a pointer to the map entry which can be used to mutate its
// underlying value.
func (r *ResourceManager) getChannelReputation(
	channel lnwire.ShortChannelID) (reputationMonitor, error) {

	if r.channelReputation[channel] == nil {
		startValue, err := r.lookupReputation(channel)
		if err != nil {
			return nil, err
		}

		r.channelReputation[channel] = r.newReputationMonitor(
			startValue,
		)

		r.log.Infof("Added new channel reputation: %v with start: %v",
			channel.ToUint64(), startValue)
	}

	return r.channelReputation[channel], nil
}

type htlcIdxTimestamp struct {
	ts  time.Time
	idx int
}

// Less is used to order PriorityQueueItem's by their release time such that
// items with the older release time are at the top of the queue.
//
// NOTE: Part of the queue.PriorityQueueItem interface.
func (r *htlcIdxTimestamp) Less(other queue.PriorityQueueItem) bool {
	return r.ts.Before(other.(*htlcIdxTimestamp).ts)
}

// ForwardHTLC returns a boolean indicating whether the HTLC proposed is
// allowed to proceed based on its reputation, endorsement and resources
// available on the outgoing channel. If this function returns true, the HTLC
// has been added to internal state and must be cleared out using ResolveHTLC.
// If it returns false, it assumes that the HTLC will be failed back and does
// not expect any further resolution notification.
func (r *ResourceManager) ForwardHTLC(htlc *ProposedHTLC,
	chanInInfo, chanOutInfo *ChannelInfo) (*ForwardDecision, error) {

	// Validate the HTLC amount. When LND intercepts, it hasn't yet
	// checked anything about the HTLC so this value could be manipulated.
	if htlc.OutgoingAmount > MaxMilliSatoshi {
		return nil, ErrAmtOverflow
	}

	r.Lock()
	defer r.Unlock()

	// First get the incoming/outgoing pair's reputation.
	incomingChannelRep, err := r.getChannelReputation(htlc.IncomingChannel)
	if err != nil {
		return nil, err
	}

	outgoingChannelRev, err := r.getTargetChannel(
		htlc.OutgoingChannel, chanOutInfo,
	)
	if err != nil {
		return nil, err
	}

	// Next, get the outgoing/incoming pair's reputation.
	outgoingChannelRep, err := r.getChannelReputation(htlc.OutgoingChannel)
	if err != nil {
		return nil, err
	}

	incomingChannelRev, err := r.getTargetChannel(
		htlc.IncomingChannel, chanInInfo,
	)
	if err != nil {
		return nil, err
	}

	reputation := ReputationCheck{
		IncomingDirection: ChannelPair{
			IncomingChannel: incomingChannelRep.Reputation(true),
			OutgoingRevenue: outgoingChannelRev.Revenue(),
		},
		OutgoingDirection: ChannelPair{
			IncomingChannel: outgoingChannelRep.Reputation(false),
			OutgoingRevenue: incomingChannelRev.Revenue(),
		},
		HTLCRisk: outstandingRisk(
			float64(r.params.BlockTime), htlc,
			r.params.ResolutionPeriod,
		),
	}

	// addInFlight is a closure that will add our HTLC to both the incoming
	// and outgoing channel's in flight htlcs. Even if it's actually failed
	// before we forward on to the outgoing link, we expected to receive a
	// resolution for it, and will appropriately clear it out then. This
	// happens near-instantly for our local failures.
	addInFlight := func(forwardOutcome ForwardOutcome) error {
		if err := incomingChannelRep.AddIncomingInFlight(
			htlc, forwardOutcome,
		); err != nil {
			return err
		}

		if err := outgoingChannelRep.AddOutgoingInFlight(htlc); err != nil {
			return err
		}
		return nil
	}

	// If the outgoing peer does not have
	outgoingRep := reputation.OutgoingDirection.SufficientReputation(
		reputation.HTLCRisk,
	)
	if htlc.IncomingEndorsed == EndorsementTrue && !outgoingRep {
		err = addInFlight(ForwardOutcomeOutgoingUnkonwn)
		if err != nil {
			return nil, err
		}

		return &ForwardDecision{
			ReputationCheck: reputation,
			ForwardOutcome:  ForwardOutcomeOutgoingUnkonwn,
		}, nil
	}

	// Get a forwarding decision from the outgoing channel, considering
	// the reputation of the incoming channel.
	forwardOutcome := outgoingChannelRev.AddInFlight(
		htlc, reputation.SufficientReputation(),
	)
	if err := addInFlight(forwardOutcome); err != nil {
		return nil, err
	}

	return &ForwardDecision{
		ReputationCheck: reputation,
		ForwardOutcome:  forwardOutcome,
	}, nil
}

// ResolveHTLC updates the reputation manager's state to reflect the
// resolution. If the incoming channel or the in flight HTLC are not found
// this operation is a no-op.
// TODO: figure out whether we should allow this API to be called and then the
// corresponding forward is not found (depends on replay logic).
func (r *ResourceManager) ResolveHTLC(htlc *ResolvedHTLC) (*InFlightHTLC,
	error) {

	r.Lock()
	defer r.Unlock()

	// Fetch the in flight HTLC from the incoming channel and add its
	// effective fees to the incoming channel's reputation.
	incomingChannel := r.channelReputation[htlc.IncomingChannel]
	if incomingChannel == nil {
		return nil, fmt.Errorf("Incoming success=%v %w: %v(%v) -> %v(%v)",
			htlc.Success, ErrChannelNotFound,
			htlc.IncomingChannel.ToUint64(),
			htlc.IncomingIndex, htlc.OutgoingChannel.ToUint64(),
			htlc.OutgoingIndex,
		)
	}

	// Resolve the HTLC on both the incoming and outgoing channel's in
	// flight trackers. We know that both must be present because we added
	// them on interception of the HTLC.
	inFlight, err := incomingChannel.ResolveIncoming(htlc)
	if err != nil {
		return nil, err
	}

	effectiveFees := effectiveFees(
		r.resolutionPeriod, htlc.TimestampSettled, inFlight,
		htlc.Success,
	)

	outgoingChannelRep := r.channelReputation[htlc.OutgoingChannel]
	if err := outgoingChannelRep.ResolveOutgoing(
		htlc.IncomingChannel, htlc.IncomingIndex, effectiveFees,
	); err != nil {
		return nil, err
	}

	// If the htlc was not assigned any outgoing resources, then it would
	// not have been allocated any resources on our outgoing link (it is
	// expected to have been failed back), so we can exit here.
	if inFlight.OutgoingDecision == ForwardOutcomeNoResources ||
		inFlight.OutgoingDecision == ForwardOutcomeOutgoingUnkonwn {

		return inFlight, nil
	}

	// It's possible that after we intercepted the HTLC it was forwarded
	// over another channel (non-strict forwarding). This is only an issue
	// when we're using an external interceptor (when built into a solution,
	// we'd know which channel we used).
	if inFlight.OutgoingChannel != htlc.OutgoingChannel {
		r.log.Debugf("Non-strict forwarding: %v used instead of %v",
			htlc.OutgoingChannel, inFlight.OutgoingChannel)
	}

	// Update state on the outgoing channel as well, likewise if we can't
	// find the channel we're receiving a resolution that we didn't catch
	// on the add. We use the outgoing channel specified by the in-flight
	// HTLC, as that's where we added the in-flight HTLC.
	outgoingChannel := r.channelRevenue[inFlight.OutgoingChannel]
	if outgoingChannel == nil {
		return nil, fmt.Errorf("Outgoing success=%v %w: %v(%v) -> %v(%v)",
			htlc.Success, ErrChannelNotFound,
			htlc.IncomingChannel.ToUint64(),
			htlc.IncomingIndex,
			htlc.OutgoingChannel.ToUint64(),
			htlc.OutgoingIndex)
	}
	outgoingChannel.ResolveInFlight(htlc, inFlight)

	return inFlight, nil
}
