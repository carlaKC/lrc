package lrc

import (
	"math"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

var testTime = time.Date(2024, time.January, 5, 0, 0, 0, 0, time.UTC)

func setupTest(period time.Duration) (*clock.TestClock, *decayingAverage) {
	testClock := clock.NewTestClock(testTime)

	return testClock, newDecayingAverage(testClock, period, nil)
}

// TestDecayingAverage tests basic operation of our decaying average
// calculations.
func TestDecayingAverage(t *testing.T) {
	period := time.Hour * 10
	testClock, decayingAverage := setupTest(period)

	// Assert that we begin with a zero value and timestamp.
	require.EqualValues(t, time.Time{}, decayingAverage.lastUpdate)
	require.EqualValues(t, 0, decayingAverage.value)

	// Once we access our value, assert that timestamp is updated.
	require.EqualValues(t, 0, decayingAverage.getValue())
	require.EqualValues(t, testTime, decayingAverage.lastUpdate)

	// Add a value to the decaying average and assert that the average
	// has the same value, because our mock clock's time is the same as
	// the last update time of the average.
	value := 10.0
	decayingAverage.add(value)
	require.EqualValues(t, value, decayingAverage.getValue())

	// Advance our clock and assert that our value has been discounted by
	// time.
	testClock.SetTime(testTime.Add(time.Hour))
	require.Less(t, decayingAverage.getValue(), value)
}

// TestDecayingAverageValues tests updating of decaying average values against
// independently calculated expected values.
func TestDecayingAverageValues(t *testing.T) {
	period := time.Second * 100
	testClock, decayingAverage := setupTest(period)

	// Progress to non-zero time
	decayingAverage.add(1000)
	require.EqualValues(t, 1000, decayingAverage.getValue())

	// Progress time a few times and assert we hit our (rounded) expected
	// values.
	testClock.SetTime(testTime.Add(time.Second * 25))
	require.EqualValues(t, 707.11,
		math.Round(decayingAverage.getValue()*100)/100)

	testClock.SetTime(testTime.Add(time.Second * 28))
	require.EqualValues(t, 678.30,
		math.Round(decayingAverage.getValue()*100)/100)

	decayingAverage.add(2300)
	require.EqualValues(t, 2978.30,
		math.Round(decayingAverage.getValue()*100)/100)

	testClock.SetTime(testTime.Add(time.Second * 78))
	require.EqualValues(t, 1489.15,
		math.Round(decayingAverage.getValue()*100)/100)
}
