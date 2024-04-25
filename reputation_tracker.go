package lrc

import (
	"time"
)

type reputationTracker struct {
	// revenue tracks the bi-directional revenue that this channel has
	// earned the local node as the incoming edge for HTLC forwards.
	revenue *decayingAverage

	// inFlightHTLCs provides a map of in-flight HTLCs, keyed by htlc id.
	inFlightHTLCs map[int]*InFlightHTLC

	// blockTime is the expected time to find a block, surfaced to account
	// for simulation scenarios where this isn't 10 minutes.
	blockTime float64

	// resolutionPeriod is the amount of time that we reasonably expect
	// a htlc to resolve in.
	resolutionPeriod time.Duration
}

func (r *reputationTracker) IncomingReputation() IncomingReputation {
	return IncomingReputation{
		IncomingRevenue: r.revenue.getValue(),
		InFlightRisk:    r.inFlightHTLCRisk(),
	}
}

// addInFlight updates the outgoing channel's view to include a new in flight
// HTLC.
func (r *reputationTracker) addInFlight(htlc *ProposedHTLC,
	outgoingEndorsed Endorsement) {

	inFlightHTLC := &InFlightHTLC{
		TimestampAdded:   r.revenue.clock.Now(),
		ProposedHTLC:     htlc,
		OutgoingEndorsed: outgoingEndorsed,
	}

	// Sanity check whether the HTLC is already present.
	if _, ok := r.inFlightHTLCs[htlc.IncomingIndex]; ok {
		return
	}

	r.inFlightHTLCs[htlc.IncomingIndex] = inFlightHTLC
}

// inFlightHTLCRisk returns the total outstanding risk of the incoming
// in-flight HTLCs from a specific channel.
func (r *reputationTracker) inFlightHTLCRisk() float64 {
	var inFlightRisk float64
	for _, htlc := range r.inFlightHTLCs {
		inFlightRisk += outstandingRisk(
			r.blockTime, htlc.ProposedHTLC, r.resolutionPeriod,
		)
	}

	return inFlightRisk
}

// outstandingRisk calculates the outstanding risk of in-flight HTLCs.
func outstandingRisk(blockTime float64, htlc *ProposedHTLC,
	resolutionPeriod time.Duration) float64 {

	return (float64(htlc.ForwardingFee()) *
		float64(htlc.CltvExpiryDelta) * blockTime * 60) /
		resolutionPeriod.Seconds()
}
