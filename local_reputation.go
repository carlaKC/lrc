package localreputation

import (
	"time"

	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lnwire"
)

// ReputationManager tracks local reputation earned by incoming channels, and
// the thresholds required to earn endorsement on the outgoing channels
// required.
type ReputationManager struct {
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

	// channelRevenue tracks the routing revenue that channels have
	// earned the local node for both incoming and outgoing HTLCs.
	channelRevenue map[lnwire.ShortChannelID]*decayingAverage

	// resolutionPeriod is the period of time that is considered reasonable
	// for a htlc to resolve in.
	resolutionPeriod time.Duration

	clock clock.Clock
}

// NewReputationManager creates a local reputation manager that will track
// channel revenue over the window provided, and incoming channel reputation
// over the window scaled by the multiplier.
func NewReputationManager(revenueWindow time.Duration,
	reputationMultiplier int, resolutionPeriod time.Duration,
	clock clock.Clock) *ReputationManager {

	return &ReputationManager{
		revenueWindow: revenueWindow,
		reputationWindow: revenueWindow * time.Duration(
			reputationMultiplier,
		),
		channelReputation: make(
			map[lnwire.ShortChannelID]*reputationTracker,
		),
		channelRevenue: make(
			map[lnwire.ShortChannelID]*decayingAverage,
		),
		resolutionPeriod: resolutionPeriod,
		clock:            clock,
	}
}

// getChannelRevenue looks up a channel's revenue record in the reputation
// manager, creating a new decaying average if one if not found. This function
// returns a pointer to the map entry which can be used to mutate its
// underlying value.
func (r *ReputationManager) getChannelRevenue(
	channel lnwire.ShortChannelID) *decayingAverage {

	if r.channelRevenue[channel] == nil {
		r.channelRevenue[channel] = newDecayingAverage(
			r.clock, r.revenueWindow,
		)
	}

	return r.channelRevenue[channel]
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
	outgoingChannel := r.getChannelRevenue(htlc.OutgoingChannel)
	outgoingRevenue := outgoingChannel.getValue()

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

type reputationTracker struct {
	// revenue tracks the bi-directional revenue that this channel has
	// earned the local node as the incoming edge for HTLC forwards.
	revenue *decayingAverage

	// inFlightHTLCs provides a map of in-flight HTLCs, keyed by htlc id.
	inFlightHTLCs map[int]*InFlightHTLC
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

	return (float64(htlc.ForwardingFee) *
		float64(htlc.CltvExpiryDelta) * 10 * 60) /
		resolutionPeriod.Seconds()
}
