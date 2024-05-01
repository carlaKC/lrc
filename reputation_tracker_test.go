package lrc

import (
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// TestReputationTracker tests tracking of reputation scores for nodes that
// are forwarding us htlcs. Since we have testing for our decaying average
// elsewhere, we fix our time here so we don't need to worry about decaying
// averages.
func TestReputationTracker(t *testing.T) {
	clock := clock.NewTestClock(testTime)
	tracker := newReputationTracker(
		clock, time.Hour, time.Second*90, 10.0, &TestLogger{},
	)
	require.Len(t, tracker.inFlightHTLCs, 0)

	totalReputation := 0

	// First, we'll test the impact of unendorsed HTLCs. We should have
	// no impact on our revenue at all, except for an increase when we
	// successfully resolve within our resolution period.

	// Unendorsed - slow failure, no reputation impact
	assertHtlcLifecycle(
		t, tracker, 0, false, false, time.Hour,
	)
	require.EqualValues(t, totalReputation, tracker.revenue.getValue())

	// Unendorsed - fast failure, no reputation impact
	assertHtlcLifecycle(
		t, tracker, 1, false, false, time.Second,
	)
	require.EqualValues(t, totalReputation, tracker.revenue.getValue())

	// Unendorsed - slow success, no reputation impact
	assertHtlcLifecycle(
		t, tracker, 2, false, true, time.Hour,
	)
	require.EqualValues(t, totalReputation, tracker.revenue.getValue())

	// Unendorsed - fast success, reputation increases by fee
	totalReputation += mockProposedFee
	assertHtlcLifecycle(
		t, tracker, 3, false, true, time.Second*30,
	)
	require.EqualValues(t, totalReputation, tracker.revenue.getValue())

	// Endorsed, fast failure - no reputation impact
	assertHtlcLifecycle(
		t, tracker, 4, true, false, time.Second,
	)
	require.EqualValues(t, totalReputation, tracker.revenue.getValue())

	// Endorsed, slow failure - reputation decreases by fee (for one period)
	totalReputation -= mockProposedFee
	assertHtlcLifecycle(
		t, tracker, 5, true, false, time.Second*90+1,
	)
	require.EqualValues(t, totalReputation, tracker.revenue.getValue())

	// Endorsed, fast success - reputation increases by fee
	totalReputation += mockProposedFee
	assertHtlcLifecycle(
		t, tracker, 6, true, true, time.Second,
	)
	require.EqualValues(t, totalReputation, tracker.revenue.getValue())

	// Endorsed, slow success (one period) - net zero impact
	assertHtlcLifecycle(
		t, tracker, 7, true, true, time.Second*90+1,
	)
	require.EqualValues(t, totalReputation, tracker.revenue.getValue())

	// Endorsed, slow failure (multiple periods) - negative reputation
	totalReputation -= mockProposedFee * 4
	assertHtlcLifecycle(
		t, tracker, 8, true, false, time.Second*90*5,
	)
	require.EqualValues(t, totalReputation, tracker.revenue.getValue())
}

func assertHtlcLifecycle(t *testing.T, tracker *reputationTracker, idx int,
	incomingEndorsed, settle bool, resolveTime time.Duration) {

	// Note, we're just setting the outgoing endorsed to whatever our
	// incoming endorsed is - we're not testing reputation here.
	htlc0 := mockProposedHtlc(100, 200, idx, incomingEndorsed)
	err := tracker.AddInFlight(htlc0, NewEndorsementSignal(incomingEndorsed))
	require.NoError(t, err)
	require.Len(t, tracker.inFlightHTLCs, 1)

	res0 := resolutionForProposed(htlc0, settle, testTime.Add(resolveTime))
	_, err = tracker.ResolveInFlight(res0)
	require.NoError(t, err)
	require.Len(t, tracker.inFlightHTLCs, 0)
}

// TestReputationTrackerErrs tests the error cases for a reputation tracker.
func TestReputationTrackerErrs(t *testing.T) {
	clock := clock.NewTestClock(testTime)
	tracker := newReputationTracker(
		clock, time.Hour, time.Second*90, 10.0, &TestLogger{},
	)

	htlc0 := mockProposedHtlc(100, 200, 0, true)
	err := tracker.AddInFlight(htlc0, NewEndorsementSignal(true))
	require.NoError(t, err)

	// Assert that we error on duplicate htlcs.
	err = tracker.AddInFlight(htlc0, NewEndorsementSignal(true))
	require.ErrorIs(t, err, ErrDuplicateIndex)

	htlc1 := mockProposedHtlc(100, 200, 1, true)
	res1 := resolutionForProposed(htlc1, true, testTime)
	_, err = tracker.ResolveInFlight(res1)
	require.ErrorIs(t, err, ErrResolutionNotFound)
}

// TestEffectiveFees tests calculation of effective fees for HTLCs.
func TestEffectiveFees(t *testing.T) {
	tests := []struct {
		name         string
		holdTime     time.Duration
		endorsed     bool
		success      bool
		effectiveFee float64
	}{
		{
			name:         "endoresd, fast success",
			holdTime:     time.Second,
			endorsed:     true,
			success:      true,
			effectiveFee: 1,
		},
		{
			name:     "endorsed, slow success",
			holdTime: time.Minute * 5,
			endorsed: true,
			success:  true,
			// Held for 4 periods, but we get the fee.
			effectiveFee: -3,
		},
		{
			name:         "endorsed, fast failure",
			holdTime:     time.Second,
			endorsed:     true,
			success:      false,
			effectiveFee: 0,
		},
		{
			name:     "endorsed, slow failure",
			holdTime: time.Minute * 5,
			endorsed: true,
			success:  false,
			// Held for 4 periods.
			effectiveFee: -4,
		},
		{
			name:         "unendorsed, fast success",
			holdTime:     time.Second,
			endorsed:     false,
			success:      true,
			effectiveFee: 1,
		},
		{
			name:         "unendorsed, slow success",
			holdTime:     time.Hour,
			endorsed:     false,
			success:      true,
			effectiveFee: 0,
		},
		{
			name:         "unendorsed, slow failure",
			holdTime:     time.Hour,
			endorsed:     false,
			success:      false,
			effectiveFee: 0,
		},
		{
			name:         "unendorsed, fast failure",
			holdTime:     time.Second,
			endorsed:     false,
			success:      false,
			effectiveFee: 0,
		},
	}

	for _, testCase := range tests {
		htlc := &InFlightHTLC{
			TimestampAdded: testTime,
			ProposedHTLC: &ProposedHTLC{
				IncomingEndorsed: NewEndorsementSignal(
					testCase.endorsed,
				),
				// Make fee = 1 for easy calc.
				IncomingAmount: 2,
				OutgoingAmount: 1,
			},
		}
		settled := htlc.TimestampAdded.Add(testCase.holdTime)

		actual := effectiveFees(
			time.Minute, settled, htlc, testCase.success,
		)
		require.Equal(t, testCase.effectiveFee, actual)
	}
}
