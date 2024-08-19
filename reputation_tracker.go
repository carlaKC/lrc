package lrc

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lnwire"
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
		outgoingInFlight: make(map[lnwire.ShortChannelID]map[int]float64),
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

	// outgoingInFlight provides a map of in-flight HTLCs that use the
	// channel as the outgoing link. These HTLCs are keyed by incoming
	// channel and index, because we don't yet have a unique identifier for
	// the outgoing htlc.
	outgoingInFlight map[lnwire.ShortChannelID]map[int]float64

	// blockTime is the expected time to find a block, surfaced to account
	// for simulation scenarios where this isn't 10 minutes.
	blockTime float64

	// resolutionPeriod is the amount of time that we reasonably expect
	// a htlc to resolve in.
	resolutionPeriod time.Duration

	log Logger
}

func (r *reputationTracker) Reputation(incoming bool) Reputation {
	return Reputation{
		IncomingRevenue: r.revenue.getValue(),
		InFlightRisk:    r.inFlightHTLCRisk(incoming),
	}
}

// AddIncomingInFlight updates the incoming channel's view to include a new in
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

// AddOutgoingInFlight updates the outgoing channel's view to include the in
// flight htlc.
func (r *reputationTracker) AddOutgoingInFlight(htlc *ProposedHTLC) error {
	incomingChanHTLCs, ok := r.outgoingInFlight[htlc.IncomingChannel]
	if !ok {
		r.outgoingInFlight[htlc.IncomingChannel] = make(
			map[int]float64,
		)
	}

	// Sanity check that HTLC is not already checked.
	if _, ok := incomingChanHTLCs[htlc.IncomingIndex]; ok {
		return fmt.Errorf("AddOutgoinInFlight: %w: %v",
			ErrDuplicateIndex, htlc.IncomingIndex)
	}

	// For outgoing HTLCs, we only need to track our in flight risk.
	r.outgoingInFlight[htlc.IncomingChannel][htlc.IncomingIndex] = htlc.inFlightRisk(
		r.blockTime, r.resolutionPeriod,
	)

	return nil
}

// ResolveIncoming removes a htlc from the reputation tracker's state,
// returning an error if it is not found, and updates the link's reputation
// accordingly. It will also return the original in flight htlc when
// successfully removed.
func (r *reputationTracker) ResolveIncoming(htlc *ResolvedHTLC) (*InFlightHTLC,
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

	r.log.Infof("Adding effective fees to incoming channel: %v: %v",
		htlc.IncomingChannel.ToUint64(), effectiveFees)

	r.revenue.add(effectiveFees)

	return inFlight, nil
}

// ResolveOutgoing removes a htlc from the reputation tracker's state,
// returning an error if it is not found.
func (r *reputationTracker) ResolveOutgoing(incoming lnwire.ShortChannelID,
	index int, effectiveFees float64) error {

	inFlightChan, ok := r.outgoingInFlight[incoming]
	if !ok {
		return fmt.Errorf("Outgoing channel lookup %w: %v(%v)",
			ErrResolutionNotFound, incoming, index)
	}

	if _, ok := inFlightChan[index]; !ok {
		return fmt.Errorf("Outgoing index lookup: %w: %v(%v)",
			ErrResolutionNotFound, incoming, index)
	}

	delete(inFlightChan, index)

	// We also add the effective fees to the outgoing channel's revenue, to
	// hold it accountable for the htlc's behavior.
	r.log.Infof("Adding effective fees to outgoing channel: %v",
		effectiveFees)

	r.revenue.add(effectiveFees)

	// If there's nothing left, we can delete the channel map as well.
	if len(inFlightChan) == 0 {
		delete(r.outgoingInFlight, incoming)
		return nil
	}

	r.outgoingInFlight[incoming] = inFlightChan

	return nil
}

// inFlightHTLCRisk returns the total outstanding risk of the incoming
// in-flight HTLCs from a specific channel.
func (r *reputationTracker) inFlightHTLCRisk(incoming bool) float64 {
	var inFlightRisk float64
	if !incoming {
		for _, incomingChan := range r.outgoingInFlight {
			for _, htlc := range incomingChan {
				inFlightRisk += htlc
			}
		}

		return inFlightRisk
	}

	for _, htlc := range r.incomingInFlight {
		inFlightRisk += htlc.inFlightRisk(
			r.blockTime, r.resolutionPeriod,
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
