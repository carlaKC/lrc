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

	// resolutionPeriod is the period of time that is considered reasonable
	// for a htlc to resolve in.
	resolutionPeriod time.Duration

	// lookupHistory fetches the historical reputation and revenue values
	// for a channel.
	lookupHistory GetHistoryFunc

	// newReputationMonitor creates a new reputation monitor, pulled
	// out for mocking purposes in tests.
	newReputationMonitor NewReputationMonitor

	clock clock.Clock

	log Logger

	// A single mutex guarding access to the manager.
	sync.Mutex
}

type ChannelFetcher func(lnwire.ShortChannelID) (*ChannelInfo, error)

// GetHistoryFunc is the function signature for fetching the forwarding history
// of a channel.
type GetHistoryFunc func(id lnwire.ShortChannelID) (*ChannelHistory, error)

// NewReputationMonitor is a function signature for a constructor that creates
// a new reputation monitor.
type NewReputationMonitor func(channel lnwire.ShortChannelID,
	info ChannelInfo) (reputationMonitor, error)

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
	channelHistory GetHistoryFunc, log Logger) (*ResourceManager, error) {

	if err := params.validate(); err != nil {
		return nil, err
	}
	return &ResourceManager{
		params: params,
		channelReputation: make(
			map[lnwire.ShortChannelID]reputationMonitor,
		),
		resolutionPeriod: params.ResolutionPeriod,
		lookupHistory:    channelHistory,
		newReputationMonitor: func(channel lnwire.ShortChannelID,
			info ChannelInfo) (reputationMonitor, error) {

			history, err := channelHistory(channel)
			if err != nil {
				return nil, err
			}

			log.Infof("Added new channel reputation: %v with "+
				"history: %v", channel, history)

			return newReputationTracker(
				clock, params, log, info, history,
			)
		},

		clock: clock,
		log:   log,
	}, nil
}

// getChannelReputation looks up a channel's reputation tracker in the
// reputation manager, creating a new tracker if one is not found. This
// function returns a pointer to the map entry which can be used to mutate its
// underlying value.
func (r *ResourceManager) getChannelReputation(
	channel lnwire.ShortChannelID, info ChannelInfo) (reputationMonitor, error) {

	if r.channelReputation[channel] == nil {
		var err error
		r.channelReputation[channel], err = r.newReputationMonitor(
			channel, info,
		)
		if err != nil {
			return nil, err
		}
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
	chanInInfo, chanOutInfo ChannelInfo) (*ForwardDecision, error) {

	// Validate the HTLC amount. When LND intercepts, it hasn't yet
	// checked anything about the HTLC so this value could be manipulated.
	if htlc.OutgoingAmount > MaxMilliSatoshi {
		return nil, ErrAmtOverflow
	}

	r.Lock()
	defer r.Unlock()

	// First get the incoming/outgoing pair's reputation.
	incomingChannelRep, err := r.getChannelReputation(htlc.IncomingChannel, chanInInfo)
	if err != nil {
		return nil, err
	}

	// Next, get the outgoing/incoming pair's reputation.
	outgoingChannelRep, err := r.getChannelReputation(htlc.OutgoingChannel, chanOutInfo)
	if err != nil {
		return nil, err
	}

	reputation := ReputationCheck{
		IncomingChannel: incomingChannelRep.Reputation(htlc, true),
		OutgoingChannel: outgoingChannelRep.Reputation(htlc, false),
	}

	outcome := outgoingChannelRep.MayAddOutgoing(
		reputation, htlc.OutgoingAmount,
		htlc.IncomingEndorsed == EndorsementTrue,
	)

	// Always add the HTLC to our incoming in flight tracking. It's already
	// locked in on our incoming link when we intercept it, and we'll want
	// to resolve it when we get a notification that it's been removed.
	err = incomingChannelRep.AddIncomingInFlight(htlc, outcome)
	if err != nil {
		return nil, err
	}

	// If we're not supposed to forward the HTLC, we won't add it to our
	// outgoing link.
	if outcome.NoForward() {
		return &ForwardDecision{
			ReputationCheck: reputation,
			ForwardOutcome:  outcome,
		}, nil
	}

	// Otherwise, track the HTLC on the outgoing link and return this
	// outcome.
	if err := outgoingChannelRep.AddOutgoingInFlight(
		htlc, outcome == ForwardOutcomeEndorsed,
	); err != nil {
		return nil, err
	}

	return &ForwardDecision{
		ReputationCheck: reputation,
		ForwardOutcome:  outcome,
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
			htlc.IncomingChannel,
			htlc.IncomingIndex, htlc.OutgoingChannel,
			htlc.OutgoingIndex,
		)
	}

	// We always add the HTLC to the incoming channel, so we resolve it
	// on our incoming link.
	inFlight, err := incomingChannel.ResolveIncoming(htlc)
	if err != nil {
		return nil, err
	}

	// If we didn't forward the HTLC, then we don't need to take any action
	// on the outgoing link.
	if inFlight.OutgoingDecision.NoForward() {
		return inFlight, nil
	}

	outgoingChannelRep := r.channelReputation[htlc.OutgoingChannel]
	if err := outgoingChannelRep.ResolveOutgoing(
		inFlight, htlc,
	); err != nil {
		return nil, err
	}

	// It's possible that after we intercepted the HTLC it was forwarded
	// over another channel (non-strict forwarding). This is only an issue
	// when we're using an external interceptor (when built into a solution,
	// we'd know which channel we used).
	if inFlight.OutgoingChannel != htlc.OutgoingChannel {
		r.log.Debugf("Non-strict forwarding: %v used instead of %v",
			htlc.OutgoingChannel, inFlight.OutgoingChannel)
	}

	return inFlight, nil
}
