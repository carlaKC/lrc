package lrc

import (
	"fmt"
	"sort"

	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lnwire"
)

type ChannelBoostrap struct {
	Scid                 lnwire.ShortChannelID
	incomingReputation   *decayingAverage
	outgoingReputation   *decayingAverage
	bidirectionalRevenue *decayingAverage
}

func (c *ChannelBoostrap) ToChannelHistory() *ChannelHistory {
	channelHistory := &ChannelHistory{}

	if c.incomingReputation != nil {
		channelHistory.IncomingReputation = &DecayingAverageStart{
			Value:      c.incomingReputation.value,
			LastUpdate: c.incomingReputation.lastUpdate,
		}
	}

	if c.outgoingReputation != nil {
		channelHistory.OutgoingReputation = &DecayingAverageStart{
			Value:      c.outgoingReputation.value,
			LastUpdate: c.outgoingReputation.lastUpdate,
		}
	}

	if c.bidirectionalRevenue != nil {
		channelHistory.Revenue = &DecayingAverageStart{
			Value:      c.bidirectionalRevenue.value,
			LastUpdate: c.bidirectionalRevenue.lastUpdate,
		}
	}

	return channelHistory
}

func (c *ChannelBoostrap) AddHTLC(clock clock.Clock, htlc *ForwardedHTLC,
	params ManagerParams) error {
	switch {
	// If the HTLC was forwarded to us, add to incoming reputation
	// and bidirectional revenue.
	case htlc.IncomingChannel == c.Scid:
		if err := c.addReputationHtlc(
			clock, htlc, params, true,
		); err != nil {
			return err
		}

	// If the HTLC was forwarded out through us, add to outgoing
	// reputation and bidirectional revenue.
	case htlc.OutgoingChannel == c.Scid:
		if err := c.addReputationHtlc(
			clock, htlc, params, false,
		); err != nil {
			return err
		}

	default:
		return fmt.Errorf("history for: "+
			"%v contains forward that does not belong "+
			"to channel (%v -> %v)", c.Scid,
			htlc.InFlightHTLC.IncomingChannel,
			htlc.Resolution.OutgoingChannel)
	}

	return c.addRevenueHtlc(clock, params, htlc)
}

func (c *ChannelBoostrap) addReputationHtlc(clock clock.Clock,
	htlc *ForwardedHTLC, params ManagerParams, incoming bool,
) error {

	effectiveFees := effectiveFees(
		params.ResolutionPeriod,
		htlc.Resolution.TimestampSettled, &htlc.InFlightHTLC,
		htlc.Resolution.Success,
	)
	forwardScid := htlc.IncomingChannel
	if !incoming {
		forwardScid = htlc.OutgoingChannel
	}

	// Sanity check scid.
	if forwardScid != c.Scid {
		return fmt.Errorf("bootstrap for channel: %v reputation "+
			"direction: %v does not match htlc scid: %v "+
			"(%v -> %v)", c.Scid, incoming, forwardScid,
			htlc.IncomingChannel, htlc.OutgoingChannel,
		)
	}

	add := func(avg *decayingAverage) *decayingAverage {
		if avg == nil {
			avg = newDecayingAverage(
				clock, params.reputationWindow(),
				&DecayingAverageStart{
					htlc.Resolution.TimestampSettled,
					effectiveFees,
				},
			)
		} else {
			avg.addAtTime(
				effectiveFees,
				htlc.Resolution.TimestampSettled,
			)
		}

		return avg
	}

	if incoming {
		c.incomingReputation = add(c.incomingReputation)
	} else {
		c.outgoingReputation = add(c.outgoingReputation)
	}

	return nil
}

func (c *ChannelBoostrap) addRevenueHtlc(clock clock.Clock,
	params ManagerParams, htlc *ForwardedHTLC) error {

	if htlc.InFlightHTLC.OutgoingChannel != c.Scid &&
		htlc.InFlightHTLC.IncomingChannel != c.Scid {

		return fmt.Errorf("revenue history for: "+
			"%v contains forward that does not belong "+
			"to channel (%v -> %v)", c.Scid,
			htlc.InFlightHTLC.IncomingChannel,
			htlc.Resolution.OutgoingChannel)
	}

	if c.bidirectionalRevenue == nil {
		c.bidirectionalRevenue = newDecayingAverage(
			clock, params.RevenueWindow,
			&DecayingAverageStart{
				htlc.Resolution.TimestampSettled,
				float64(htlc.ForwardingFee()),
			},
		)
	} else {
		c.bidirectionalRevenue.addAtTime(
			float64(htlc.ForwardingFee()),
			htlc.Resolution.TimestampSettled,
		)
	}

	return nil
}

// BootstrapHistory processes a set of htlc forwards for a channel and builds
// up its reputation history.
func BootstrapHistory(scid lnwire.ShortChannelID, params ManagerParams,
	history []*ForwardedHTLC, clock clock.Clock) (*ChannelHistory,
	error) {

	// Zero history is a valid input, we just return no values.
	if len(history) == 0 {
		return &ChannelHistory{}, nil
	}

	// We sort by resolved timestamp so that we can replay the values for
	// our decaying average.
	sort.Slice(history, func(i, j int) bool {
		return history[i].Resolution.TimestampSettled.Before(
			history[j].Resolution.TimestampSettled,
		)
	})

	channel := ChannelBoostrap{
		Scid: scid,
	}

	for _, h := range history {
		if err := channel.AddHTLC(clock, h, params); err != nil {
			return nil, err
		}
	}

	return channel.ToChannelHistory(), nil
}
