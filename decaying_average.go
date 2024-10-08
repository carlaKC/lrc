package lrc

import (
	"fmt"
	"math"
	"time"

	"github.com/lightningnetwork/lnd/clock"
)

// DecayingAverageStart represents an optional start value to create a
// decaying average with.
type DecayingAverageStart struct {
	LastUpdate time.Time
	Value      float64
}

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
// a starting value of 0 if no start value is provided and bootstrapping from
// existing values otherwise.
func newDecayingAverage(clock clock.Clock, period time.Duration,
	startValue *DecayingAverageStart) *decayingAverage {

	lastUpdate := time.Time{}
	value := 0.0

	if startValue != nil {
		lastUpdate = startValue.LastUpdate
		value = startValue.Value
	}

	return &decayingAverage{
		lastUpdate: lastUpdate,
		value:      value,
		decayRate:  calcualteDecayRate(period),
		clock:      clock,
	}
}

// update modifies the decaying average's value to reflect its value at the
// current time. This function *must* be used when reading and writing the
// average's value.
func (d *decayingAverage) update(updateTime time.Time) {
	var (
		lastUpdateDiff = updateTime.Sub(d.lastUpdate).Seconds()
	)

	// If no time has passed, we don't need to apply an update.
	if lastUpdateDiff == 0 {
		return
	}

	d.value = d.value * math.Pow(d.decayRate, lastUpdateDiff)
	d.lastUpdate = updateTime
}

// getValue updates the decaying average to the present and returns its
// current value.
func (d *decayingAverage) getValue() float64 {
	d.update(d.clock.Now())
	return d.value
}

// add updates the decaying average to the present and adds the value provided.
func (d *decayingAverage) add(value float64) {
	d.addAtTime(value, d.clock.Now())
}

// addAtTime adds a value to the decaying average at a specific timestamp. This
// should be called with the current time to apply current updates, and may
// be called with times in the past to bootstrap previous values. If
// bootstrapping, values must be provided in chronological order (we can't
// add a value at a timestamp that is before our last update).
func (d *decayingAverage) addAtTime(value float64, ts time.Time) error {
	if !d.lastUpdate.IsZero() && ts.Before(d.lastUpdate) {
		return fmt.Errorf("updated decaying average at: %v with "+
			"value: %v after last update time of: %v",
			ts, value, d.lastUpdate)
	}

	d.update(ts)
	d.value += value
	d.lastUpdate = ts

	return nil
}

// DebugValue returns the value of the decaying average without updating its
// timestamp. This should *not* be used outside of debugging contexts.
func (d *decayingAverage) DebugValue() float64 {
	return d.value
}
