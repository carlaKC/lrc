package lrc

import (
	"errors"
	"fmt"

	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// ErrProtocolLimit is returned when we exceed the protocol defined
	// limit of htlc slots.
	ErrProtocolLimit = errors.New("slot count exceeds protocol limit of " +
		"483")

	// ErrProtectedPercentage is returned when a protected percentage
	// is invalid.
	ErrProtectedPercentage = errors.New("protected percentage must be " +
		"<= 100")
)

// Compile time check that ReputationManager implements the
// LocalReputationManager interface.
var _ resourceBucketer = (*bucketResourceManager)(nil)

// bucketResourceManager
type bucketResourceManager struct {
	generalLiquidity lnwire.MilliSatoshi
	generalSlots     uint64

	inFlightLiquidity lnwire.MilliSatoshi
	inFlightSlots     uint64
}

// newBucketResourceManager creates a resource manager that reserves a
// percentage of resources for HTLCs that are protected.
func newBucketResourceManager(totalLiquidity lnwire.MilliSatoshi,
	totalSlots, protectedPercentage uint64) (*bucketResourceManager,
	error) {

	if totalSlots > 483 {
		return nil, fmt.Errorf("%w: with %v slots", ErrProtocolLimit,
			totalSlots)
	}

	if protectedPercentage > 100 {
		return nil, fmt.Errorf("%w: with %v percentage",
			ErrProtectedPercentage, protectedPercentage)
	}

	protectedLiquidity := (uint64(totalLiquidity) * protectedPercentage) / 100
	protectedSlots := (totalSlots * protectedPercentage) / 100

	return &bucketResourceManager{
		generalLiquidity: totalLiquidity - lnwire.MilliSatoshi(
			protectedLiquidity,
		),
		generalSlots: totalSlots - protectedSlots,
	}, nil
}

// addHTLC adds a HTLC to the resource manager's internal state, returning a
// boolean indicating whether the HTLC should be forwarded based on bucketing
// concerns. Protected HTLCs are always forwarded, as they are not restricted,
// and general HTLCs are forwarded if there is sufficient liquidity and slots.
func (r *bucketResourceManager) addHTLC(protected bool,
	amount lnwire.MilliSatoshi) bool {

	if protected {
		return true
	}

	if r.inFlightLiquidity+amount > r.generalLiquidity {
		return false
	}

	if r.inFlightSlots+1 > r.generalSlots {
		return false
	}

	r.inFlightLiquidity += amount
	r.inFlightSlots++

	return true
}

// removeHTLC removes a HTLC from the bucket resource manager's internal state.
func (r *bucketResourceManager) removeHTLC(protected bool,
	amount lnwire.MilliSatoshi) {

	if protected {
		return
	}

	// TODO: more graceful error handling.
	if r.inFlightLiquidity < amount {
		panic(fmt.Sprintf("In flight: %v less than amount being "+
			"resolved: %v", r.inFlightLiquidity, amount))
	}

	if r.inFlightSlots == 0 {
		panic("Resolved HTLC with no slots in flight")
	}

	r.inFlightLiquidity -= amount
	r.inFlightSlots--
}
