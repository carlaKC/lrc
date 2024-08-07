package lrc

import (
	"testing"

	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// TestTargetChannelTracker tests basic functionality of our target tracker,
// mocking our bucketing functionality as it's tested elsewhere.
func TestTargetChannelTracker(t *testing.T) {

	testClock := clock.NewTestClock(testTime)
	tracker, err := newTargetChannelTracker(
		testClock, testParams, &ChannelInfo{}, &TestLogger{}, nil,
	)
	require.NoError(t, err)

	// Overwrite our bucketer with a mock bucket so we don't need to worry
	// about the actual operation of the bucket when we're just testing
	// the tracker.
	mockBucket := &MockBucket{}
	defer mockBucket.AssertExpectations(t)
	tracker.resourceBuckets = mockBucket

	// Add an in-flight htlc that's endorsed with sufficient reputation.
	incoming := IncomingReputation{
		IncomingRevenue: 1000,
		InFlightRisk:    50,
	}
	htlc0 := &ProposedHTLC{
		IncomingEndorsed: EndorsementTrue,
		IncomingAmount:   100,
		OutgoingAmount:   10,
	}
	mockBucket.Mock.On("addHTLC", true, htlc0.OutgoingAmount).Return(true).Once()

	decision := tracker.AddInFlight(incoming, htlc0)
	require.Equal(t, ForwardOutcomeEndorsed, decision.ForwardOutcome)

	// Resolve the htlc and assert that revenue is increased.
	mockBucket.Mock.On("removeHTLC", true, htlc0.OutgoingAmount).Return().Once()
	err = tracker.ResolveInFlight(
		&ResolvedHTLC{Success: true},
		&InFlightHTLC{
			ProposedHTLC:     htlc0,
			OutgoingDecision: ForwardOutcomeEndorsed,
		},
	)
	require.NoError(t, err)
	require.EqualValues(t, 90, tracker.revenue.getValue())

	// Add a htlc that has sufficient reputation but is not endorsed, and
	// set the mock such that we still have space for it.
	htlc1 := &ProposedHTLC{
		IncomingEndorsed: EndorsementFalse,
		IncomingAmount:   100,
		OutgoingAmount:   20,
	}
	mockBucket.Mock.On("addHTLC", false, htlc1.OutgoingAmount).Return(true).Once()

	decision = tracker.AddInFlight(incoming, htlc1)
	require.Equal(t, ForwardOutcomeUnendorsed, decision.ForwardOutcome)

	// Resolve the htlc unsuccessfully and assert that revenue is
	// unchanged.
	mockBucket.Mock.On("removeHTLC", false, htlc1.OutgoingAmount).Return().Once()
	err = tracker.ResolveInFlight(
		&ResolvedHTLC{Success: false},
		&InFlightHTLC{
			ProposedHTLC:     htlc1,
			OutgoingDecision: ForwardOutcomeUnendorsed,
		},
	)
	require.NoError(t, err)
	require.EqualValues(t, 90, tracker.revenue.getValue())

	// Next, add a htlc that is endorsed but does not have sufficient
	// reputation (due to large in flight) and set our bucket to indicate
	// that it does not have space for general htlcs.
	incoming.InFlightRisk = incoming.IncomingRevenue * 2
	htlc2 := &ProposedHTLC{
		IncomingEndorsed: EndorsementTrue,
		IncomingAmount:   100,
		OutgoingAmount:   30,
	}
	mockBucket.Mock.On("addHTLC", false, htlc2.OutgoingAmount).Return(false).Once()

	decision = tracker.AddInFlight(incoming, htlc2)
	require.Equal(t, ForwardOutcomeNoResources, decision.ForwardOutcome)

	// Test that htlcs that were not assigned resources are rejected.
	err = tracker.ResolveInFlight(
		&ResolvedHTLC{}, &InFlightHTLC{
			OutgoingDecision: ForwardOutcomeNoResources,
		},
	)
	require.ErrorIs(t, err, ErrResolvedNoResources)
}
