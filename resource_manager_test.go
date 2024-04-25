package lrc

import (
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// setup creates a resource manager for testing with some sane values set.
// If a channel history function is not provided, we stub it out with an
// empty impl.
func setup(t *testing.T, chanHist ChannelHistory,
	buckets ResourceBucketConstructor) (*clock.TestClock,
	*ResourceManager) {

	testClock := clock.NewTestClock(testTime)

	if chanHist == nil {
		chanHist = func(id lnwire.ShortChannelID,
			incomingOnly bool) ([]*ForwardedHTLC, error) {

			return nil, nil
		}
	}

	if buckets == nil {
		buckets = func(totalLiquidity lnwire.MilliSatoshi,
			totalSlots, protectedPercentage uint64) (
			resourceBucketer, error) {

			return newBucketResourceManager(
				totalLiquidity, totalSlots,
				protectedPercentage,
			)
		}

	}

	r, err := NewReputationManager(
		time.Hour, 10, time.Second*90, testClock, chanHist, 50,
		&TestLogger{}, 10,
	)
	require.NoError(t, err)

	return testClock, r
}

func mockBucketConstructor(m *MockBucket) func(totalLiquidity lnwire.MilliSatoshi,
	totalSlots, protectedPercentage uint64) (
	resourceBucketer, error) {

	return func(totalLiquidity lnwire.MilliSatoshi,
		totalSlots, protectedPercentage uint64) (
		resourceBucketer, error) {

		return m, nil
	}
}

// TestResourceManager tests resource manager's handling of HTLCs
func TestResourceManager(t *testing.T) {
	// Create a mock resource bucket and
	targetBucket := &MockBucket{}
	defer targetBucket.AssertExpectations(t)

	// We don't care about a history bootstrap function here.
	_, mgr := setup(t, nil, mockBucketConstructor(targetBucket))

	htlc0 := mockProposedHtlc(100, 200, 0, true)
	chanOutInfo := &ChannelInfo{
		InFlightHTLC:      483,
		InFlightLiquidity: 100000,
	}

	// Start with a HTLC that overflows.
	htlc0.OutgoingAmount = MaxMilliSatoshi + 1
	_, err := mgr.ForwardHTLC(htlc0, chanOutInfo)
	require.ErrorIs(t, err, ErrAmtOverflow)

        // Add a HTLC that's endorsed, but doesn't yet have reputation.
	htlc1 := mockProposedHtlc(200, 100, 0, true)
	f, err := mgr.ForwardHTLC(htlc1, chanOutInfo)
	require.NoError(t, err)
	require.Equal(t, ForwardOutcomeUnendorsed, f.ForwardOutcome)
}
