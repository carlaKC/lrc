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
	// protectedPercentage is the percentage of liquidity and slots that
	// are reserved for endorsed HTLCs from peers with sufficient
	// reputation.
	//
	// Note: this percentage could be different for liquidity and slots,
	// but is set to one value for simplicity here.
	protectedPercentage uint64

	// reputationWindow is the period of time over which decaying averages
	// of reputation revenue for incoming channels are calculated.
	reputationWindow time.Duration

	// revenueWindow in the period of time over which decaying averages of
	// routing revenue for requested outgoing channels are calculated.
	revenueWindow time.Duration

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

	// targetChannels tracks the routing revenue that channels have
	// earned the local node for both incoming and outgoing HTLCs.
	targetChannels map[lnwire.ShortChannelID]targetMonitor

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

	// newTargetMonitor creates a new target monitor, pull out for mocking
	// in tests.
	newTargetMonitor NewTargetMonitor

	clock clock.Clock

	log Logger

	// Expected time for blocks, included to allow simulation with faster
	// block time.
	blockTime float64

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

// NewTargetMonitor is a function signature for a constructor that creates
// a new target channel revenue monitor.
type NewTargetMonitor func(start *DecayingAverageStart,
	chanInfo *ChannelInfo) (targetMonitor, error)

// NewResourceManager creates a local reputation manager that will track
// channel revenue over the window provided, and incoming channel reputation
// over the window scaled by the multiplier.
func NewResourceManager(revenueWindow time.Duration,
	reputationMultiplier int, resolutionPeriod time.Duration,
	clock clock.Clock, lookupReputation LookupReputation,
	lookupRevenue LookupRevenue, protectedPercentage uint64,
	log Logger, blockTime float64) (*ResourceManager, error) {

	if protectedPercentage > 100 {
		return nil, fmt.Errorf("Percentage: %v > 100",
			protectedPercentage)
	}

	reputationWindow := revenueWindow * time.Duration(
		reputationMultiplier,
	)

	return &ResourceManager{
		protectedPercentage: protectedPercentage,
		revenueWindow:       revenueWindow,
		reputationWindow:    reputationWindow,
		channelReputation: make(
			map[lnwire.ShortChannelID]reputationMonitor,
		),
		targetChannels: make(
			map[lnwire.ShortChannelID]targetMonitor,
		),
		resolutionPeriod: resolutionPeriod,
		lookupReputation: lookupReputation,
		lookupRevenue:    lookupRevenue,
		newReputationMonitor: func(start *DecayingAverageStart) reputationMonitor {
			return newReputationTracker(
				clock, reputationWindow, resolutionPeriod,
				blockTime, log, start,
			)
		},
		newTargetMonitor: func(start *DecayingAverageStart,
			chanInfo *ChannelInfo) (targetMonitor, error) {

			return newTargetChannelTracker(
				clock, revenueWindow, chanInfo,
				protectedPercentage,
				blockTime, resolutionPeriod, log, start,
			)

		},
		clock:     clock,
		blockTime: blockTime,
		log:       log,
	}, nil
}

// getTargetChannel looks up a channel's revenue record in the reputation
// manager, creating a new decaying average if one if not found. This function
// returns a pointer to the map entry which can be used to mutate its
// underlying value.
func (r *ResourceManager) getTargetChannel(channel lnwire.ShortChannelID,
	chanInfo *ChannelInfo) (targetMonitor, error) {

	if r.targetChannels[channel] == nil {

		revenue, err := r.lookupRevenue(channel)
		if err != nil {
			return nil, err
		}

		r.targetChannels[channel], err = r.newTargetMonitor(
			revenue, chanInfo,
		)
		if err != nil {
			return nil, err
		}
	}

	return r.targetChannels[channel], nil
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
	}

	return r.channelReputation[channel], nil
}

// sufficientReputation returns a reputation check that is used to determine
// whether the forwarding peer has sufficient reputation to forward the
// proposed htlc over the outgoing channel that they have requested.
func (r *ResourceManager) sufficientReputation(htlc *ProposedHTLC,
	outgoingChannelRevenue float64) (*ReputationCheck, error) {

	incomingChannel, err := r.getChannelReputation(htlc.IncomingChannel)
	if err != nil {
		return nil, err
	}

	return &ReputationCheck{
		IncomingReputation: incomingChannel.IncomingReputation(),
		OutgoingRevenue:    outgoingChannelRevenue,
		HTLCRisk: outstandingRisk(
			r.blockTime, htlc, r.resolutionPeriod,
		),
	}, nil
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
	chanOutInfo *ChannelInfo) (*ForwardDecision, error) {

	// Validate the HTLC amount. When LND intercepts, it hasn't yet
	// checked anything about the HTLC so this value could be manipulated.
	if htlc.OutgoingAmount > MaxMilliSatoshi {
		return nil, ErrAmtOverflow
	}

	r.Lock()
	defer r.Unlock()

	incomingChannel, err := r.getChannelReputation(htlc.IncomingChannel)
	if err != nil {
		return nil, err
	}

	outgoingChannel, err := r.getTargetChannel(
		htlc.OutgoingChannel, chanOutInfo,
	)
	if err != nil {
		return nil, err
	}

	// Get a forwarding decision from the outgoing channel, considering
	// the reputation of the incoming channel.
	forwardDecision := outgoingChannel.AddInFlight(
		incomingChannel.IncomingReputation(), htlc,
	)

	// If we have no resources for this htlc, no further action.
	if forwardDecision.ForwardOutcome == ForwardOutcomeNoResources {
		return &forwardDecision, nil
	}

	// If we do proceed with the forward, then add it to our incoming
	// link, tracking our outgoing endorsement status.
	if err := incomingChannel.AddInFlight(
		htlc, NewEndorsementSignal(
			forwardDecision.ForwardOutcome ==
				ForwardOutcomeEndorsed,
		),
	); err != nil {
		return nil, err
	}

	return &forwardDecision, nil
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
		return nil, fmt.Errorf("%w: incoming %v",
			ErrChannelNotFound, htlc.IncomingChannel.ToUint64())
	}

	// Resolve the HTLC on the incoming channel. If it's not found, it's
	// possible that we only started tracking after the HTLC was forwarded
	// so we log the event and return without error.
	inFlight, err := incomingChannel.ResolveInFlight(htlc)
	if err != nil {
		return nil, err
	}

	// Update state on the outgoing channel as well, likewise if we can't
	// find the channel we're receiving a resolution that we didn't catch
	// on the add.
	outgoingChannel := r.targetChannels[htlc.OutgoingChannel]
	if outgoingChannel == nil {
		return nil, fmt.Errorf("%w: outgoing: %v",
			ErrChannelNotFound, htlc.OutgoingChannel.ToUint64())
	}
	outgoingChannel.ResolveInFlight(htlc, inFlight)

	return inFlight, nil
}
