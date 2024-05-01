package lrc

import (
	"time"

	"github.com/lightningnetwork/lnd/clock"
)

// Compile time check that targetChannelTracker implements the targetMonitor
// interface.
var _ targetMonitor = (*targetChannelTracker)(nil)

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

	log Logger
}

func newTargetChannelTracker(clock clock.Clock, revenueWindow time.Duration,
	channel *ChannelInfo, protectedPortion uint64, blockTime float64,
	resolutionPeriod time.Duration, log Logger,
	startValue *DecayingAverageStart) (*targetChannelTracker, error) {

	bucket, err := newBucketResourceManager(
		channel.InFlightLiquidity, channel.InFlightHTLC,
		protectedPortion,
	)
	if err != nil {
		return nil, err
	}

	return &targetChannelTracker{
		revenue: newDecayingAverage(
			clock, revenueWindow, startValue,
		),
		resourceBuckets:  bucket,
		blockTime:        blockTime,
		resolutionPeriod: resolutionPeriod,
		log:              log,
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

// ResolveInFlight removes a htlc from our internal state, crediting the fees
// to our channel if it was successful.
func (t *targetChannelTracker) ResolveInFlight(htlc *ResolvedHTLC,
	inFlight *InFlightHTLC) {

	// Add the fees for the forward to the outgoing channel _if_ the
	// HTLC was successful.
	if htlc.Success {
		t.log.Infof("HTLC successful: adding fees to channel: %v: %v",
			htlc.OutgoingChannel.ToUint64(),
			inFlight.ForwardingFee())

		t.revenue.add(float64(inFlight.ForwardingFee()))
	}

	// Clear out the resources in our resource bucket regardless of outcome.
	t.resourceBuckets.removeHTLC(
		inFlight.OutgoingEndorsed == EndorsementTrue,
		inFlight.OutgoingAmount,
	)
}
