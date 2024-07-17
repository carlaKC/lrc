package lrc

import (
	"fmt"
	"sort"

	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lnwire"
)

func addReputationHtlc(clock clock.Clock, scid lnwire.ShortChannelID,
	htlc *ForwardedHTLC, params ManagerParams,
	reputationAvg *decayingAverage) (*decayingAverage, error) {

	// Sanity check that we only have forwards where we're the
	// incoming channel.
	if htlc.InFlightHTLC.IncomingChannel != scid {
		return nil, fmt.Errorf("reputation history for: "+
			"%v contains forward that does not belong "+
			"to channel (%v -> %v)", scid,
			htlc.InFlightHTLC.IncomingChannel,
			htlc.Resolution.OutgoingChannel)
	}

	effectiveFees := effectiveFees(
		params.ResolutionPeriod,
		htlc.Resolution.TimestampSettled, &htlc.InFlightHTLC,
		htlc.Resolution.Success,
	)

	if reputationAvg == nil {
		reputationAvg = newDecayingAverage(
			clock, params.reputationWindow(),
			&DecayingAverageStart{
				htlc.Resolution.TimestampSettled,
				effectiveFees,
			},
		)
	} else {
		reputationAvg.addAtTime(
			effectiveFees,
			htlc.Resolution.TimestampSettled,
		)
	}

	return reputationAvg, nil
}

// BootstrapReputation processes a set of forwards where we are the incoming
// link and returns a start value for our reputation decaying average (or nil
// if there is no history).
func BootstrapReputation(scid lnwire.ShortChannelID, params ManagerParams,
	history []*ForwardedHTLC, clock clock.Clock) (*DecayingAverageStart,
	error) {

	// Zero history is a valid input, we just return no values.
	if len(history) == 0 {
		return nil, nil
	}

	// We sort by resolved timestamp so that we can replay the values for
	// our decaying average.
	sort.Slice(history, func(i, j int) bool {
		return history[i].Resolution.TimestampSettled.Before(
			history[j].Resolution.TimestampSettled,
		)
	})

	var reputationAvg *decayingAverage

	for _, h := range history {
		var err error
		reputationAvg, err = addReputationHtlc(
			clock, scid, h, params, reputationAvg,
		)
		if err != nil {
			return nil, err
		}
	}

	return &DecayingAverageStart{
		Value:      reputationAvg.getValue(),
		LastUpdate: reputationAvg.lastUpdate,
	}, nil
}

func addRevenueHtlc(clock clock.Clock, scid lnwire.ShortChannelID,
	params ManagerParams, htlc *ForwardedHTLC,
	revenueAvg *decayingAverage) (*decayingAverage, error) {

	if htlc.InFlightHTLC.OutgoingChannel != scid &&
		htlc.InFlightHTLC.IncomingChannel != scid {

		return nil, fmt.Errorf("revenue history for: "+
			"%v contains forward that does not belong "+
			"to channel (%v -> %v)", scid,
			htlc.InFlightHTLC.IncomingChannel,
			htlc.Resolution.OutgoingChannel)
	}

	if revenueAvg == nil {
		revenueAvg = newDecayingAverage(
			clock, params.RevenueWindow,
			&DecayingAverageStart{
				htlc.Resolution.TimestampSettled,
				float64(htlc.ForwardingFee()),
			},
		)
	} else {
		revenueAvg.addAtTime(
			float64(htlc.ForwardingFee()),
			htlc.Resolution.TimestampSettled,
		)
	}

	return revenueAvg, nil
}

// BootstrapRevenue processes a set of forwards where we are the outgoing link
// and returns a start value for our revenue decaying average (or nil if there
// is no history).
func BootstrapRevenue(scid lnwire.ShortChannelID, params ManagerParams,
	history []*ForwardedHTLC, clock clock.Clock) (*DecayingAverageStart,
	error) {

	// Zero history is a valid input, we just return no values.
	if len(history) == 0 {
		return nil, nil
	}

	// We sort by resolved timestamp so that we can replay the values for
	// our decaying average.
	sort.Slice(history, func(i, j int) bool {
		return history[i].Resolution.TimestampSettled.Before(
			history[j].Resolution.TimestampSettled,
		)
	})

	var revenueAvg *decayingAverage

	for _, h := range history {
		var err error
		revenueAvg, err = addRevenueHtlc(
			clock, scid, params, h, revenueAvg,
		)
		if err != nil {
			return nil, err
		}
	}

	return &DecayingAverageStart{
		Value:      revenueAvg.getValue(),
		LastUpdate: revenueAvg.lastUpdate,
	}, nil
}

type NetworkForward struct {
	NodeAlias string
	*ForwardedHTLC
}

type ChannelBootstrap struct {
	Incoming map[lnwire.ShortChannelID]*decayingAverage
	Outgoing map[lnwire.ShortChannelID]*decayingAverage
}

func newChannels() *ChannelBootstrap {
	return &ChannelBootstrap{
		Incoming: make(map[lnwire.ShortChannelID]*decayingAverage),
		Outgoing: make(map[lnwire.ShortChannelID]*decayingAverage),
	}
}

// BootstrapNetwork reads a list of forwards belonging to a network of nodes
// and calculates pairwise reputation for all channels in the network.
func BootstrapNetwork(params ManagerParams, history []*NetworkForward,
	clock clock.Clock) (map[string]*ChannelBootstrap, error) {

	if len(history) == 0 {
		return nil, nil
	}

	nodes := make(map[string]*ChannelBootstrap)

	for _, h := range history {
		var err error

		channels, ok := nodes[h.NodeAlias]
		if !ok {
			channels = newChannels()
		}

		incoming, _ := channels.Incoming[h.IncomingChannel]
		channels.Incoming[h.IncomingChannel], err = addReputationHtlc(
			clock, h.IncomingChannel, h.ForwardedHTLC, params,
			incoming,
		)
		if err != nil {
			return nil, err
		}

		outgoing, _ := channels.Outgoing[h.OutgoingChannel]
		channels.Outgoing[h.OutgoingChannel], err = addRevenueHtlc(
			clock, h.OutgoingChannel, params, h.ForwardedHTLC,
			outgoing,
		)
		if err != nil {
			return nil, err
		}

		nodes[h.NodeAlias] = channels
	}

	return nodes, nil
}
