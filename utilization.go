package lrc

import (
	"time"

	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lnwire"
)

type channelUtilization struct {
	slotUtilization *decayingAverage

	liquidityUtilization *decayingAverage

	slotCount float64

	capacityMsat float64
}

func newChannelUtilization(clock clock.Clock, period time.Duration,
	slotStart, capacityStart *DecayingAverageStart,
	info ChannelInfo) channelUtilization {

	return channelUtilization{
		slotUtilization: newDecayingAverage(
			clock, period, slotStart,
		),
		liquidityUtilization: newDecayingAverage(
			clock, period, capacityStart,
		),
		slotCount:    float64(info.InFlightHTLC),
		capacityMsat: float64(info.InFlightLiquidity),
	}
}

func (c channelUtilization) addHtlc(amount lnwire.MilliSatoshi) {
	c.slotUtilization.add(1)
	c.liquidityUtilization.add(float64(amount))
}

// manUtilization returns the largest resource utilization ratio for the
// channel's resource - either the portion of liquidity or slots that are
// commonly used.
func (c channelUtilization) maxUtilization() float64 {
	// Get our average utilization of slots on the channel. Set this value
	// to at least one to represent the slot that the HTLC will consume.
	minSlots := c.slotUtilization.getValue()
	if minSlots < 1 {
		minSlots = 1
	}

	slotsUtilization := minSlots / c.slotCount
	liquidityUtilization := c.liquidityUtilization.getValue() / c.capacityMsat

	if slotsUtilization > liquidityUtilization {
		return slotsUtilization
	}

	return liquidityUtilization
}
