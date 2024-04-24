package lrc

import (
	"testing"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestResourceBucketing tests functionality of resource buckets.
func TestResourceBucketing(t *testing.T) {
	_, err := newBucketResourceManager(0, 500, 0)
	require.ErrorIs(t, err, ErrProtocolLimit)

	_, err = newBucketResourceManager(0, 0, 101)
	require.ErrorIs(t, err, ErrProtectedPercentage)

	// Create a bucket of size 100 which protects 50% of its resources.
	bucketSize := lnwire.MilliSatoshi(100)
	bucket, err := newBucketResourceManager(bucketSize, 4, 50)
	require.NoError(t, err)

	// Any protected HTLC should be accepted.
	require.True(t, bucket.addHTLC(true, 100))

	// Assert that we can add an unprotected htlc that takes all the
	// liquidity.
	require.True(t, bucket.addHTLC(false, 50))

	// We then cannot add another unprotected HTLC, but will happily add
	// a protected one.
	require.False(t, bucket.addHTLC(false, 1))
	require.True(t, bucket.addHTLC(true, 1))

	// Remove the HTLC that was taking up all our general liquidity.
	bucket.removeHTLC(false, 50)

	// Now add htlcs to take up all our general slots, but not liquidity
	// and fail when we hit the limit.
	require.True(t, bucket.addHTLC(false, 10))
	require.True(t, bucket.addHTLC(false, 10))
	require.False(t, bucket.addHTLC(false, 10))

	// Remove one of our htlcs and assert that we still handle liquidity
	// constraints correctly.
	bucket.removeHTLC(false, 10)
	require.False(t, bucket.addHTLC(false, 50))
}
