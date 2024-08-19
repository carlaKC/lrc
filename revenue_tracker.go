package lrc

import (
	"errors"
	"fmt"
	"time"

	"github.com/lightningnetwork/lnd/clock"
)

var (
	// ErrResolvedNoResources is returned if we try to resolve a htlc on
	// the outgoing link that was not supposed to be allocated resources
	// in the first place.
	ErrResolvedNoResources = errors.New("resolved htlc on outgoing link " +
		"that should not have been assigned resources")
)

// Compile time check that revenueMonitor implements the revenueMonitor
// interface.
var _ revenueMonitor = (*revenueTracker)(nil)

// revenueTracker is used to track the revenue and resources of channels
// that are requested as the outgoing link of a forward.
type revenueTracker struct {
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

func newRevenueTracker(clock clock.Clock, params ManagerParams,
	channel *ChannelInfo, log Logger,
	startValue *DecayingAverageStart) (*revenueTracker, error) {

	bucket, err := newBucketResourceManager(
		channel.InFlightLiquidity, channel.InFlightHTLC,
		params.ProtectedPercentage,
	)
	if err != nil {
		return nil, err
	}

	return &revenueTracker{
		revenue: newDecayingAverage(
			clock, params.RevenueWindow, startValue,
		),
		resourceBuckets:  bucket,
		blockTime:        float64(params.BlockTime),
		resolutionPeriod: params.ResolutionPeriod,
		log:              log,
	}, nil
}

// AddInFlight adds a proposed htlc to our outgoing channel, considering the
// reputation of the incoming link. This function will return a forwarding
// decision with the details of the reputation decision and the action that
// was taken for this HTLC considering our resources and its endorsement.
func (t *revenueTracker) AddInFlight(incomingReputation IncomingReputation,
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
func (t *revenueTracker) ResolveInFlight(htlc *ResolvedHTLC,
	inFlight *InFlightHTLC) error {

	if inFlight.OutgoingDecision == ForwardOutcomeNoResources {
		return fmt.Errorf("%w: %v(%v) -> %v(%v)",
			ErrResolvedNoResources, htlc.IncomingChannel.ToUint64(),
			htlc.IncomingIndex, htlc.OutgoingChannel.ToUint64(),
			htlc.OutgoingIndex)
	}

	// Add the fees for the forward to the outgoing channel _if_ the
	// HTLC was successful.
	if htlc.Success {
		t.log.Infof("HTLC successful (%v(%v) -> %v(%v)): adding fees "+
			"to channel: %v msat", htlc.IncomingChannel.ToUint64(),
			htlc.IncomingIndex, htlc.OutgoingChannel.ToUint64(),
			htlc.OutgoingIndex, inFlight.ForwardingFee())

		t.revenue.add(float64(inFlight.ForwardingFee()))
	}

	// Clear out the resources in our resource bucket regardless of outcome.
	t.resourceBuckets.removeHTLC(
		inFlight.OutgoingDecision == ForwardOutcomeEndorsed,
		inFlight.OutgoingAmount,
	)

	return nil
}
