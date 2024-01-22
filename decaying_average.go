package lrc

import (
	"math"
	"time"

	"github.com/lightningnetwork/lnd/clock"
)

type decayingAverage struct {
	lastUpdate time.Time
	value      float64
	decayRate  float64
	clock      clock.Clock
}

// calcualteDecayRate determines the decay rate required for the period over
// which a decaying average is calculated.
func calcualteDecayRate(period time.Duration) float64 {
	return math.Pow(0.5, 2/period.Seconds())
}

// newDecayingAverage creates a decaying average over the period provided, with
// a starting value of 0.
func newDecayingAverage(clock clock.Clock,
	period time.Duration) *decayingAverage {

	return &decayingAverage{
		lastUpdate: clock.Now(),
		value:      0,
		decayRate:  calcualteDecayRate(period),
		clock:      clock,
	}
}

// update modifies the decaying average's value to reflect its value at the
// current time. This function *must* be used when reading and writing the
// average's value.
func (d *decayingAverage) update() {
	var (
		now            = d.clock.Now()
		lastUpdateDiff = now.Sub(d.lastUpdate).Seconds()
	)

	// If no time has passed, we don't need to apply an update.
	if lastUpdateDiff == 0 {
		return
	}

	d.value = d.value * math.Pow(d.decayRate, lastUpdateDiff)
}

// getValue updates the decaying average to the present and returns its
// current value.
func (d *decayingAverage) getValue() float64 {
	d.update()

	return d.value
}

// add updates the decaying average to the present and adds the value provided.
func (d *decayingAverage) add(value float64) {
	d.update()
	d.value += value
	d.lastUpdate = d.clock.Now()
}
