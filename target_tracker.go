package lrc

import (
	"time"

	"github.com/lightningnetwork/lnd/clock"
)

// targetChannelTracker is used to track the revenue and resources of channels
// that are requested as the outgoing link of a forward.
type targetChannelTracker struct {
	revenue *decayingAverage

	// blockTime is the expected time to find a block, surfaced to account
	// for simulation scenarios where this isn't 10 minutes.
	blockTime float64

	// resolutionPeriod is the amount of time that we reasonably expect
	// a htlc to resolve in.
	resolutionPeriod time.Duration

	resourceBuckets resourceBucketer
}

func newTargetChannelTracker(clock clock.Clock, revenueWindow time.Duration,
	channel *ChannelInfo, protectedPortion uint64,
	newBucket ResourceBucketConstructor, blockTime float64,
	resolutionPeriod time.Duration) (*targetChannelTracker, error) {

	bucket, err := newBucket(
		channel.InFlightLiquidity, channel.InFlightHTLC,
		protectedPortion,
	)
	if err != nil {
		return nil, err
	}

	return &targetChannelTracker{
		revenue:          newDecayingAverage(clock, revenueWindow),
		resourceBuckets:  bucket,
		blockTime:        blockTime,
		resolutionPeriod: resolutionPeriod,
	}, nil
}

// AddInFlight adds a proposed htlc to our outgoing channel, considering the
// reputation of the incoming link. This function will return a forwarding
// decision with the details of the reputation decision and the action that
// was taken for this HTLC considering our resources and its endorsement.
func (t *targetChannelTracker) AddInFlight(incomingReputation IncomingReputation,
	htlc *ProposedHTLC) ForwardDecision {

	// First, assess reputation of the incoming link to decide whether
	// the HTLC should be granted access to protected slots.
	reputationCheck := ReputationCheck{
		IncomingReputation: incomingReputation,
		OutgoingRevenue:    t.revenue.getValue(),
		HTLCRisk: outstandingRisk(
			t.blockTime, htlc, t.resolutionPeriod,
		),
	}

	htlcProtected := reputationCheck.SufficientReputation() &&
		htlc.IncomingEndorsed == EndorsementTrue

		// Try to add the htlc to our bucket, if we can't accommodate it
		// return early.
	canForward := t.resourceBuckets.addHTLC(
		htlcProtected, htlc.OutgoingAmount,
	)

	var outcome ForwardOutcome
	switch {
	case !canForward:
		outcome = ForwardOutcomeNoResources

	case htlcProtected:
		outcome = ForwardOutcomeEndorsed

	default:
		outcome = ForwardOutcomeUnendorsed
	}

	return ForwardDecision{
		ReputationCheck: reputationCheck,
		ForwardOutcome:  outcome,
	}
}
