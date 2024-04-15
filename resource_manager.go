package lrc

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/queue"
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
	channelReputation map[lnwire.ShortChannelID]*reputationTracker

	// targetChannels tracks the routing revenue that channels have
	// earned the local node for both incoming and outgoing HTLCs.
	targetChannels map[lnwire.ShortChannelID]*targetChannelTracker

	// resolutionPeriod is the period of time that is considered reasonable
	// for a htlc to resolve in.
	resolutionPeriod time.Duration

	// channelHistory is a closure that's used to grab the historical
	// forwards for a channel when we encounter it.
	channelHistory ChannelHistory

	clock clock.Clock

	log Logger

	// A single mutex guarding access to the manager.
	sync.Mutex
}

type ChannelFetcher func(lnwire.ShortChannelID) (*ChannelInfo, error)

// ChannelHistory is a closure type used to obtain the historical forwards for
// a channel. If incomingOnly is true, then it'll filter for forwards where
// the channel was the incoming nodes. Otherwise, it'll return forwards where
// the channel was either the incoming or the outgoing link.
type ChannelHistory func(id lnwire.ShortChannelID,
	incomingOnly bool) ([]*ForwardedHTLC, error)

// NewReputationManager creates a local reputation manager that will track
// channel revenue over the window provided, and incoming channel reputation
// over the window scaled by the multiplier.
func NewReputationManager(revenueWindow time.Duration,
	reputationMultiplier int, resolutionPeriod time.Duration,
	clock clock.Clock, channelHistory ChannelHistory,
	protectedPercentage uint64, log Logger) (*ResourceManager, error) {

	if protectedPercentage > 100 {
		return nil, fmt.Errorf("Percentage: %v > 100",
			protectedPercentage)
	}

	return &ResourceManager{
		protectedPercentage: protectedPercentage,
		revenueWindow:       revenueWindow,
		reputationWindow: revenueWindow * time.Duration(
			reputationMultiplier,
		),
		channelReputation: make(
			map[lnwire.ShortChannelID]*reputationTracker,
		),
		targetChannels: make(
			map[lnwire.ShortChannelID]*targetChannelTracker,
		),
		resolutionPeriod: resolutionPeriod,
		channelHistory:   channelHistory,
		clock:            clock,
		log:              log,
	}, nil
}

// getTargetChannel looks up a channel's revenue record in the reputation
// manager, creating a new decaying average if one if not found. This function
// returns a pointer to the map entry which can be used to mutate its
// underlying value.
func (r *ResourceManager) getTargetChannel(channel lnwire.ShortChannelID,
	chanInfo *ChannelInfo) (*targetChannelTracker, error) {

	if r.targetChannels[channel] == nil {
		var err error
		r.targetChannels[channel], err = r.newTargetChannel(
			channel, chanInfo,
		)
		if err != nil {
			return nil, err
		}
	}

	return r.targetChannels[channel], nil
}

// newTargetChannel creates a new target channel tracker for the short channel
// id provided.
func (r *ResourceManager) newTargetChannel(id lnwire.ShortChannelID,
	chanInfo *ChannelInfo) (*targetChannelTracker, error) {

	targetChannel := newTargetChannelTracker(
		r.clock, r.revenueWindow, chanInfo,
		r.protectedPercentage,
	)

	// When adding a target channel, we want to account for all of its
	// forwards so we pull full history for the channel.
	history, err := r.channelHistory(id, false)
	if err != nil {
		return nil, err
	}

	r.log.Infof("Adding new target channel: %v with: %v historical "+
		"records (in flight count: %v, capacity: %v)",
		id, len(history), chanInfo.InFlightHTLC,
		chanInfo.InFlightLiquidity)

	// Add the bi-directional revenue for our forwards to the fresh tracker.
	// We sort by resolved timestamp so that we can reply values for our
	// decaying average.
	sort.Slice(history, func(i, j int) bool {
		return history[i].Resolution.TimestampSettled.Before(
			history[j].Resolution.TimestampSettled,
		)
	})

	for _, h := range history {
		if !(h.InFlightHTLC.IncomingChannel == id ||
			h.InFlightHTLC.OutgoingChannel == id) {

			return nil, fmt.Errorf("forwarding history for: "+
				"%v contains forward htat does not belong "+
				"to channel (%v -> %v)", id,
				h.InFlightHTLC.IncomingChannel,
				h.Resolution.OutgoingChannel)
		}

		targetChannel.revenue.addAtTime(
			float64(h.InFlightHTLC.ForwardingFee()),
			h.Resolution.TimestampSettled,
		)
	}

	return targetChannel, nil
}

// getChannelReputation looks up a channel's reputation tracker in the
// reputation manager, creating a new tracker if one is not found. This
// function returns a pointer to the map entry which can be used to mutate its
// underlying value.
func (r *ResourceManager) getChannelReputation(
	channel lnwire.ShortChannelID) (*reputationTracker, error) {

	if r.channelReputation[channel] == nil {
		var err error
		r.channelReputation[channel], err = r.newChannelReputation(
			channel,
		)
		if err != nil {
			return nil, err
		}
	}

	return r.channelReputation[channel], nil
}

// newChannelReputation creates a new channel reputation tracker for the
// short channel id provided.
func (r *ResourceManager) newChannelReputation(
	channel lnwire.ShortChannelID) (*reputationTracker, error) {

	reputationTracker := &reputationTracker{
		revenue: newDecayingAverage(
			r.clock, r.reputationWindow,
		),
		inFlightHTLCs: make(map[int]*InFlightHTLC),
	}

	// When adding a reputation tracker, we only want to account for the
	// incoming HTLCs that contributed to our revenue so we filter our
	// historical query by incomingOnly.
	history, err := r.channelHistory(channel, true)
	if err != nil {
		return nil, err
	}

	// Add the bi-directional revenue for our forwards to the fresh
	// tracker. We sort by resolved timestamp so that we can replay values
	// for our decaying average.
	sort.Slice(history, func(i, j int) bool {
		return history[i].Resolution.TimestampSettled.Before(
			history[j].Resolution.TimestampSettled,
		)
	})

	r.log.Infof("Adding new reputation tracker: %v with: %v historical "+
		"records", channel, len(history))

	for _, h := range history {
		if !(h.InFlightHTLC.IncomingChannel == channel ||
			h.InFlightHTLC.OutgoingChannel == channel) {

			return nil, fmt.Errorf("forwarding history for: "+
				"%v contains forward htat does not belong "+
				"to channel (%v -> %v)", channel,
				h.InFlightHTLC.IncomingChannel,
				h.Resolution.OutgoingChannel)
		}

		effectiveFees := r.effectiveFees(
			h.Resolution.TimestampSettled, &h.InFlightHTLC,
			h.Resolution.Success,
		)
		reputationTracker.revenue.addAtTime(
			effectiveFees, h.Resolution.TimestampSettled,
		)

	}

	return reputationTracker, nil
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

	incomingRevenue := incomingChannel.revenue.getValue()

	// Get the in flight risk for the incoming channel.
	inFlightRisk := incomingChannel.inFlightHTLCRisk(
		htlc.IncomingChannel, r.resolutionPeriod,
	)

	return &ReputationCheck{
		IncomingRevenue: incomingRevenue,
		OutgoingRevenue: outgoingChannelRevenue,
		InFlightRisk:    inFlightRisk,
		HTLCRisk:        outstandingRisk(htlc, r.resolutionPeriod),
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

	r.Lock()
	defer r.Unlock()

	outgoingChannel, err := r.getTargetChannel(
		htlc.OutgoingChannel, chanOutInfo,
	)
	if err != nil {
		return nil, err
	}

	// First, check whether the HTLC qualifies for protected resources.
	reputation, err := r.sufficientReputation(
		htlc, outgoingChannel.revenue.getValue(),
	)
	if err != nil {
		return nil, err
	}
	sufficientRep := reputation.SufficientReputation()

	// The HTLC has access to protected spots if it has sufficient
	// reputation *and* the incoming htlc was endorsed.
	htlcProtected := sufficientRep &&
		htlc.IncomingEndorsed == EndorsementTrue

	// Next, check whether there is space for the HTLC in the assigned
	// bucket on the outgoing channel. If there is no space, we return
	// false indicating that there are no available resources for the HTLC.
	canForward := outgoingChannel.resourceBuckets.addHTLC(
		htlcProtected, htlc.OutgoingAmount,
	)
	if !canForward {
		return &ForwardDecision{
			ReputationCheck: *reputation,
			ForwardOutcome:  ForwardOutcomeNoResources,
		}, nil
	}

	// If there is space for the HTLC, we've accounted for it in our
	// resource bucketing so we go ahead and add it to the in-flight
	// HTLCs on the incoming channel, returning true indicating that
	// we're happy for the HTLC to proceed.
	incomingCHannel, err := r.getChannelReputation(htlc.IncomingChannel)
	if err != nil {
		return nil, err
	}
	incomingCHannel.addInFlight(
		htlc, NewEndorsementSignal(htlcProtected),
	)

	if htlcProtected {
		return &ForwardDecision{
			ReputationCheck: *reputation,
			ForwardOutcome:  ForwardOutcomeEndorsed,
		}, nil
	}

	return &ForwardDecision{
		ReputationCheck: *reputation,
		ForwardOutcome:  ForwardOutcomeUnendorsed,
	}, nil
}

// ResolveHTLC updates the reputation manager's state to reflect the
// resolution. If the incoming channel or the in flight HTLC are not found
// this operation is a no-op.
// TODO: figure out whether we should allow this API to be called and then the
// corresponding forward is not found (depends on replay logic).
func (r *ResourceManager) ResolveHTLC(htlc *ResolvedHTLC) *InFlightHTLC {
	r.Lock()
	defer r.Unlock()

	// Fetch the in flight HTLC from the incoming channel and add its
	// effective fees to the incoming channel's reputation.
	incomingChannel := r.channelReputation[htlc.IncomingChannel]
	if incomingChannel == nil {
		r.log.Infof("Incoming channel: %v not found for resolve",
			htlc.IncomingChannel.ToUint64())

		return nil
	}

	inFlight, ok := incomingChannel.inFlightHTLCs[htlc.IncomingIndex]
	if !ok {
		r.log.Infof("In flight HTLC: %v/%v not found for resolve",
			htlc.IncomingChannel.ToUint64(), htlc.IncomingIndex)

		return nil
	}

	delete(incomingChannel.inFlightHTLCs, inFlight.IncomingIndex)
	effectiveFees := r.effectiveFees(
		htlc.TimestampSettled, inFlight, htlc.Success,
	)
	incomingChannel.revenue.add(effectiveFees)

	// Add the fees for the forward to the outgoing channel _if_ the
	// HTLC was successful.
	outgoingChannel := r.targetChannels[htlc.OutgoingChannel]
	if outgoingChannel == nil {
		r.log.Infof("Outgoing channel: %v not found for removal",
			htlc.OutgoingChannel.ToUint64())

		return nil
	}

	if htlc.Success {
		outgoingChannel.revenue.add(float64(inFlight.ForwardingFee()))
	}

	// Clear out the resources in our resource bucket regardless of outcome.
	outgoingChannel.resourceBuckets.removeHTLC(
		inFlight.OutgoingEndorsed == EndorsementTrue,
		inFlight.OutgoingAmount,
	)

	return inFlight
}

func (r *ResourceManager) effectiveFees(timestampSettled time.Time,
	htlc *InFlightHTLC, success bool) float64 {

	resolutionTime := timestampSettled.Sub(htlc.TimestampAdded).Seconds()
	resolutionSeconds := r.resolutionPeriod.Seconds()
	fee := float64(htlc.ForwardingFee())

	opportunityCost := math.Ceil(
		(resolutionTime-resolutionSeconds)/resolutionSeconds,
	) * fee

	switch {
	// Successful, endorsed HTLC.
	case htlc.IncomingEndorsed == EndorsementTrue && success:
		return fee - opportunityCost

		// Failed, endorsed HTLC.
	case htlc.IncomingEndorsed == EndorsementTrue:
		return -1 * opportunityCost

	// Successful, unendorsed HTLC.
	case success:
		if resolutionTime <= r.resolutionPeriod.Seconds() {
			return fee
		}

		return 0

	// Failed, unendorsed HTLC.
	default:
		return 0
	}
}

// targetChannelTracker is used to track the revenue and resources of channels
// that are requested as the outgoing link of a forward.
type targetChannelTracker struct {
	revenue *decayingAverage

	resourceBuckets resourceBucketer
}

func newTargetChannelTracker(clock clock.Clock, revenueWindow time.Duration,
	channel *ChannelInfo, protectedPortion uint64) *targetChannelTracker {

	return &targetChannelTracker{
		revenue: newDecayingAverage(clock, revenueWindow),
		resourceBuckets: newBucketResourceManager(
			channel.InFlightLiquidity, channel.InFlightHTLC,
			protectedPortion,
		),
	}
}

type reputationTracker struct {
	// revenue tracks the bi-directional revenue that this channel has
	// earned the local node as the incoming edge for HTLC forwards.
	revenue *decayingAverage

	// inFlightHTLCs provides a map of in-flight HTLCs, keyed by htlc id.
	inFlightHTLCs map[int]*InFlightHTLC
}

// addInFlight updates the outgoing channel's view to include a new in flight
// HTLC.
func (r *reputationTracker) addInFlight(htlc *ProposedHTLC,
	outgoingEndorsed Endorsement) {

	inFlightHTLC := &InFlightHTLC{
		TimestampAdded:   r.revenue.clock.Now(),
		ProposedHTLC:     htlc,
		OutgoingEndorsed: outgoingEndorsed,
	}

	// Sanity check whether the HTLC is already present.
	if _, ok := r.inFlightHTLCs[htlc.IncomingIndex]; ok {
		return
	}

	r.inFlightHTLCs[htlc.IncomingIndex] = inFlightHTLC
}

// inFlightHTLCRisk returns the total outstanding risk of the incoming
// in-flight HTLCs from a specific channel.
func (r *reputationTracker) inFlightHTLCRisk(
	incomingChannel lnwire.ShortChannelID,
	resolutionPeriod time.Duration) float64 {

	var inFlightRisk float64
	for _, htlc := range r.inFlightHTLCs {
		inFlightRisk += outstandingRisk(
			htlc.ProposedHTLC, resolutionPeriod,
		)
	}

	return inFlightRisk
}

// outstandingRisk calculates the outstanding risk of in-flight HTLCs.
func outstandingRisk(htlc *ProposedHTLC,
	resolutionPeriod time.Duration) float64 {

	return (float64(htlc.ForwardingFee()) *
		float64(htlc.CltvExpiryDelta) * 10 * 60) /
		resolutionPeriod.Seconds()
}
