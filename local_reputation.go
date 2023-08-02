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

	clock clock.Clock
}

// NewReputationManager creates a local reputation manager that will track
// channel revenue over the window provided, and incoming channel reputation
// over the window scaled by the multiplier.
func NewReputationManager(revenueWindow time.Duration,
	reputationMultiplier int, clock clock.Clock) *ReputationManager {

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
		clock: clock,
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

type reputationTracker struct {
	// revenue tracks the bi-directional revenue that this channel has
	// earned the local node as the incoming edge for HTLC forwards.
	revenue *decayingAverage

	// inFlightHTLCs provides a map of in-flight HTLCs, keyed by htlc id.
	inFlightHTLCs map[int]*InFlightHTLC
}
