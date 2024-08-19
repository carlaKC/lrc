package lrc

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/lightningnetwork/lnd/clock"
)

var (
	// ErrResolutionNotFound is returned when we get a resolution for a
	// HTLC that is not found in our in flight set.
	ErrResolutionNotFound = errors.New("resolved htlc not found")

	// ErrDuplicateIndex is returned when an incoming htlc index is
	// duplicated.
	ErrDuplicateIndex = errors.New("htlc index duplicated")
)

// Compile time check that reputationTracker implements the reputationMonitor
// interface.
var _ reputationMonitor = (*reputationTracker)(nil)

func newReputationTracker(clock clock.Clock, params ManagerParams,
	log Logger, startValue *DecayingAverageStart) *reputationTracker {

	return &reputationTracker{
		revenue: newDecayingAverage(
			clock, params.reputationWindow(), startValue,
		),
		incomingInFlight: make(map[int]*InFlightHTLC),
		blockTime:        float64(params.BlockTime),
		resolutionPeriod: params.ResolutionPeriod,
		log:              log,
	}
}

type reputationTracker struct {
	// revenue tracks the bi-directional revenue that this channel has
	// earned the local node as the incoming edge for HTLC forwards.
	revenue *decayingAverage

	// incomingInFlight provides a map of in-flight HTLCs, keyed by htlc id
	// on the incoming link.
	incomingInFlight map[int]*InFlightHTLC

	// blockTime is the expected time to find a block, surfaced to account
	// for simulation scenarios where this isn't 10 minutes.
	blockTime float64

	// resolutionPeriod is the amount of time that we reasonably expect
	// a htlc to resolve in.
	resolutionPeriod time.Duration

	log Logger
}

func (r *reputationTracker) IncomingReputation() IncomingReputation {
	return IncomingReputation{
		IncomingRevenue: r.revenue.getValue(),
		InFlightRisk:    r.inFlightHTLCRisk(),
	}
}

// AddIncomingInFlight updates the outgoing channel's view to include a new in
// flight HTLC.
func (r *reputationTracker) AddIncomingInFlight(htlc *ProposedHTLC,
	outgoingDecision ForwardOutcome) error {

	inFlightHTLC := &InFlightHTLC{
		TimestampAdded:   r.revenue.clock.Now(),
		ProposedHTLC:     htlc,
		OutgoingDecision: outgoingDecision,
	}

	// Sanity check whether the HTLC is already present.
	if _, ok := r.incomingInFlight[htlc.IncomingIndex]; ok {
		return fmt.Errorf("%w: %v", ErrDuplicateIndex,
			htlc.IncomingIndex)
	}

	r.incomingInFlight[htlc.IncomingIndex] = inFlightHTLC

	return nil
}

// ResolveInFlight removes a htlc from the reputation tracker's state,
// returning an error if it is not found, and updates the link's reputation
// accordingly. It will also return the original in flight htlc when
// successfully removed.
func (r *reputationTracker) ResolveInFlight(htlc *ResolvedHTLC) (*InFlightHTLC,
	error) {

	inFlight, ok := r.incomingInFlight[htlc.IncomingIndex]
	if !ok {
		return nil, fmt.Errorf("%w: %v(%v) -> %v(%v)", ErrResolutionNotFound,
			htlc.IncomingChannel.ToUint64(), htlc.IncomingIndex,
			htlc.OutgoingChannel.ToUint64(), htlc.OutgoingIndex)
	}

	delete(r.incomingInFlight, inFlight.IncomingIndex)

	effectiveFees := effectiveFees(
		r.resolutionPeriod, htlc.TimestampSettled, inFlight,
		htlc.Success,
	)

	r.log.Infof("Adding effective fees to channel: %v: %v",
		htlc.IncomingChannel.ToUint64(), effectiveFees)

	r.revenue.add(effectiveFees)

	return inFlight, nil
}

// inFlightHTLCRisk returns the total outstanding risk of the incoming
// in-flight HTLCs from a specific channel.
func (r *reputationTracker) inFlightHTLCRisk() float64 {
	var inFlightRisk float64
	for _, htlc := range r.incomingInFlight {
		// Only endorsed HTLCs count towards our in flight risk.
		if htlc.IncomingEndorsed != EndorsementTrue {
			continue
		}

		inFlightRisk += outstandingRisk(
			r.blockTime, htlc.ProposedHTLC, r.resolutionPeriod,
		)
	}

	return inFlightRisk
}

func effectiveFees(resolutionPeriod time.Duration, timestampSettled time.Time,
	htlc *InFlightHTLC, success bool) float64 {

	resolutionTime := timestampSettled.Sub(htlc.TimestampAdded).Seconds()
	resolutionSeconds := resolutionPeriod.Seconds()
	fee := float64(htlc.ForwardingFee())

	opportunityCost := math.Ceil(
		(resolutionTime-resolutionSeconds)/resolutionSeconds,
	) * fee

	switch {
	// Successful, endorsed HTLC.
	case htlc.IncomingEndorsed == EndorsementTrue && success:
		return fee - opportunityCost

	// Failed, endorsed HTLC.
	case htlc.IncomingEndorsed == EndorsementTrue:
		return -1 * opportunityCost

	// Successful, unendorsed HTLC.
	case success:
		if resolutionTime <= resolutionPeriod.Seconds() {
			return fee
		}

		return 0

	// Failed, unendorsed HTLC.
	default:
		return 0
	}
}

// outstandingRisk calculates the outstanding risk of in-flight HTLCs.
func outstandingRisk(blockTime float64, htlc *ProposedHTLC,
	resolutionPeriod time.Duration) float64 {

	return (float64(htlc.ForwardingFee()) *
		float64(htlc.CltvExpiryDelta) * blockTime * 60) /
		resolutionPeriod.Seconds()
}
