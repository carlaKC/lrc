package lrc

import (
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

var testTime = time.Date(2024, time.January, 5, 0, 0, 0, 0, time.UTC)

func setupTest(period time.Duration) (*clock.TestClock, *decayingAverage) {
	testClock := clock.NewTestClock(testTime)

	return testClock, newDecayingAverage(testClock, period)
}

func TestDecayingAverage(t *testing.T) {
	period := time.Hour * 10
	testClock, decayingAverage := setupTest(period)

	// Assert that we begin with a zero value.
	require.EqualValues(t, 0, decayingAverage.getValue())

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
