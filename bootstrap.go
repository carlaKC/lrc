package lrc

import (
	"fmt"
	"sort"

	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lnwire"
)

type ChannelBootstrap struct {
	// Reputation is the historical reputation that a channel accrued as
	// the incoming link in forwards.
	Reputation *DecayingAverageStart

	// Revenue is the historical revenue that the channel accrued as the
	// outgoing link in forwards.
	Revenue *DecayingAverageStart
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
		// Sanity check that we only have forwards where we're the
		// incoming channel.
		if h.InFlightHTLC.IncomingChannel != scid {
			return nil, fmt.Errorf("reputation history for: "+
				"%v contains forward that does not belong "+
				"to channel (%v -> %v)", scid,
				h.InFlightHTLC.IncomingChannel,
				h.Resolution.OutgoingChannel)
		}

		effectiveFees := effectiveFees(
			params.ResolutionPeriod,
			h.Resolution.TimestampSettled, &h.InFlightHTLC,
			h.Resolution.Success,
		)

		if reputationAvg == nil {
			reputationAvg = newDecayingAverage(
				clock, params.reputationWindow(),
				&DecayingAverageStart{
					h.Resolution.TimestampSettled,
					effectiveFees,
				},
			)
		} else {
			reputationAvg.addAtTime(
				effectiveFees,
				h.Resolution.TimestampSettled,
			)
		}

	}

	return &DecayingAverageStart{
		Value:      reputationAvg.getValue(),
		LastUpdate: reputationAvg.lastUpdate,
	}, nil
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
		if h.InFlightHTLC.OutgoingChannel != scid {
			return nil, fmt.Errorf("revenue history for: "+
				"%v contains forward that does not belong "+
				"to channel (%v -> %v)", scid,
				h.InFlightHTLC.IncomingChannel,
				h.Resolution.OutgoingChannel)
		}

		if revenueAvg == nil {
			revenueAvg = newDecayingAverage(
				clock, params.RevenueWindow,
				&DecayingAverageStart{
					h.Resolution.TimestampSettled,
					float64(h.ForwardingFee()),
				},
			)
		} else {
			revenueAvg.addAtTime(
				float64(h.ForwardingFee()),
				h.Resolution.TimestampSettled,
			)
		}
	}

	return &DecayingAverageStart{
		Value:      revenueAvg.getValue(),
		LastUpdate: revenueAvg.lastUpdate,
	}, nil
}
