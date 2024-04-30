package lrc

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestResourceBucketing tests functionality of resource buckets.
func TestResourceBucketing(t *testing.T) {
	_, err := newBucketResourceManager(0, 500, 0)
	require.ErrorIs(t, err, ErrProtocolLimit)

	_, err = newBucketResourceManager(0, 0, 101)
	require.ErrorIs(t, err, ErrProtectedPercentage)
}
