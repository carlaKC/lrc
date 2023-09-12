package localreputation

import (
	"fmt"
	"math"
	"time"

	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lnwire"
)

// Compile time check that ReputationManager implements the
// LocalReputationManager interface.
var _ LocalReputationManager = (*ReputationManager)(nil)

// ReputationManager tracks local reputation earned by incoming channels, and
// the thresholds required to earn endorsement on the outgoing channels
// required.
type ReputationManager struct {
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

	// channelLookup provides the ability to look up our currently open
	// channels.
	channelLookup ChannelFetcher

	clock clock.Clock
}

type ChannelFetcher func(lnwire.ShortChannelID) (*ChannelInfo, error)

// NewReputationManager creates a local reputation manager that will track
// channel revenue over the window provided, and incoming channel reputation
// over the window scaled by the multiplier.
func NewReputationManager(revenueWindow time.Duration,
	reputationMultiplier int, resolutionPeriod time.Duration,
	clock clock.Clock, protectedPercentage uint64,
	getChannel ChannelFetcher) (*ReputationManager, error) {

	if protectedPercentage > 100 {
		return nil, fmt.Errorf("Percentage: %v > 100",
			protectedPercentage)
	}

	return &ReputationManager{
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
		clock:            clock,
	}, nil
}

// getTargetChannel looks up a channel's revenue record in the reputation
// manager, creating a new decaying average if one if not found. This function
// returns a pointer to the map entry which can be used to mutate its
// underlying value.
func (r *ReputationManager) getTargetChannel(
	channel lnwire.ShortChannelID) (*targetChannelTracker, error) {

	chanInfo, err := r.channelLookup(channel)
	if err != nil {
		return nil, err
	}

	if r.targetChannels[channel] == nil {
		r.targetChannels[channel] = newTargetChannelTracker(
			r.clock, r.revenueWindow, chanInfo, r.protectedPercentage,
		)
	}

	return r.targetChannels[channel], nil
}

// getChannelReputation looks up a channel's reputation tracker in the
// reputation manager, creating a new tracker if one is not found. This
// function returns a pointer to the map entry which can be used to mutate its
// underlying value.
func (r *ReputationManager) getChannelReputation(
	channel lnwire.ShortChannelID) *reputationTracker {

	if r.channelReputation[channel] == nil {
		r.channelReputation[channel] = &reputationTracker{
			revenue: newDecayingAverage(
				r.clock, r.reputationWindow,
			),
			inFlightHTLCs: make(map[int]*InFlightHTLC),
		}
	}

	return r.channelReputation[channel]
}

// SufficientReputation returns a boolean indicating whether the forwarding
// peer has sufficient reputation to forward the proposed htlc over the
// outgoing channel that they have requested.
func (r *ReputationManager) SufficientReputation(htlc *ProposedHTLC) bool {
	outgoingChannel, err := r.getTargetChannel(htlc.OutgoingChannel)
	if err != nil {
		return false
	}

	outgoingRevenue := outgoingChannel.revenue.getValue()

	incomingChannel := r.getChannelReputation(htlc.IncomingChannel)
	incomingRevenue := incomingChannel.revenue.getValue()

	// Get the in flight risk for the incoming channel.
	inFlightRisk := incomingChannel.inFlightHTLCRisk(
		htlc.IncomingChannel, r.resolutionPeriod,
	)

	// We include the proposed HTLC in our in-flight risk as well, as this
	// is the risk we're taking on.
	inFlightRisk += outstandingRisk(htlc, r.resolutionPeriod)

	// The incoming channel has sufficient reputation if:
	// incoming_channel_revenue - in_flight_risk >= outgoing_link_revenue
	return incomingRevenue > outgoingRevenue+inFlightRisk
}

// ForwardHTLC updates the reputation manager's state to reflect that a HTLC
// has been forwarded (and is now in-flight on the outgoing channel).
func (r *ReputationManager) ForwardHTLC(htlc *ProposedHTLC) {
	r.getChannelReputation(htlc.IncomingChannel).addInFlight(htlc, false)
}

// ResolveHTLC updates the reputation manager's state to reflect the
// resolution
func (r *ReputationManager) ResolveHTLC(htlc *ResolvedHLTC) {
	// Fetch the in flight HTLC from the incoming channel and add its
	// effective fees to the incoming channel's reputation.
	incomingChannel := r.getChannelReputation(htlc.IncomingChannel)
	inFlight, ok := incomingChannel.inFlightHTLCs[htlc.IncomingIndex]
	if !ok {
		return
	}

	delete(incomingChannel.inFlightHTLCs, inFlight.IncomingIndex)
	effectiveFees := r.effectiveFees(inFlight, htlc.Success)
	incomingChannel.revenue.add(effectiveFees)

	// Add the fees for the forward to the outgoing channel _if_ the
	// HTLC was successful.
	outgoingChannel, err := r.getTargetChannel(htlc.OutgoingChannel)
	if err != nil {
		// Note: we don't expect an error here because we expect the
		// outgoing channel to have already been created so we can
		// panic.
		panic(fmt.Sprintf("Outgoing channel: %v not found on resolve",
			htlc.OutgoingChannel))
	}

	if htlc.Success {
		outgoingChannel.revenue.add(float64(inFlight.ForwardingFee()))
	}
}

func (r *ReputationManager) effectiveFees(htlc *InFlightHTLC,
	success bool) float64 {

	resolutionTime := r.clock.Now().Sub(htlc.TimestampAdded).Seconds()
	resolutionSeconds := r.resolutionPeriod.Seconds()
	fee := float64(htlc.ForwardingFee())

	opportunityCost := math.Ceil(
		(resolutionTime-resolutionSeconds)/resolutionSeconds,
	) * fee

	switch {
	// Successful, endorsed HTLC.
	case htlc.IncomingEndorsed && success:
		return fee - opportunityCost

		// Failed, endorsed HTLC.
	case htlc.IncomingEndorsed:
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
	outgoingEndorsed bool) {

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

// outstandingRisk calculates the outstanding risk of in-flight HLTCs.
func outstandingRisk(htlc *ProposedHTLC,
	resolutionPeriod time.Duration) float64 {

	return (float64(htlc.ForwardingFee()) *
		float64(htlc.CltvExpiryDelta) * 10 * 60) /
		resolutionPeriod.Seconds()
}
