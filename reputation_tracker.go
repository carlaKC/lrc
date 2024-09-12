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

	// ErrProtocolLimit is returned when we exceed the protocol defined
	// limit of htlc slots.
	ErrProtocolLimit = errors.New("slot count exceeds protocol limit of " +
		"483")

	// ErrProtectedPercentage is returned when a protected percentage
	// is invalid.
	ErrProtectedPercentage = errors.New("protected percentage must be " +
		"<= 100")
)

// Compile time check that reputationTracker implements the reputationMonitor
// interface.
var _ reputationMonitor = (*reputationTracker)(nil)

func newReputationTracker(clock clock.Clock, params ManagerParams,
	log Logger, info ChannelInfo, history *ChannelHistory) (
	*reputationTracker, error) {

	if info.InFlightHTLC > 483 {
		return nil, fmt.Errorf("%w: with %v slots", ErrProtocolLimit,
			info.InFlightHTLC)
	}

	if params.ProtectedPercentage > 100 {
		return nil, fmt.Errorf("%w: with %v percentage",
			ErrProtectedPercentage, params.ProtectedPercentage)
	}

	generalLiquidity := info.InFlightLiquidity - lnwire.MilliSatoshi(
		(uint64(info.InFlightLiquidity)*params.ProtectedPercentage)/100,
	)

	generalSlots := info.InFlightHTLC - (info.InFlightHTLC*params.ProtectedPercentage)/100

	return &reputationTracker{
		bidirectionalRevenue: newDecayingAverage(
			clock, params.RevenueWindow, history.Revenue,
		),
		incomingReputation: newDecayingAverage(
			clock, params.reputationWindow(), history.IncomingReputation,
		),
		outgoingReputation: newDecayingAverage(
			clock, params.reputationWindow(), history.OutgoingReputation,
		),
		incomingInFlight: make(map[int]*InFlightHTLC),
		outgoingInFlight: make(map[lnwire.ShortChannelID]map[int]outgoingHTLC),
		// TODO: add ability to bootstrap utlization values.
		incomingUtilization: newChannelUtilization(
			clock, params.RevenueWindow, nil, nil, info,
		),
		outgoingUtilization: newChannelUtilization(
			clock, params.RevenueWindow, nil, nil, info,
		),
		blockTime:        float64(params.BlockTime),
		generalLiquidity: generalLiquidity,
		generalSlots:     int(generalSlots),
		resolutionPeriod: params.ResolutionPeriod,
		log:              log,
	}, nil
}

// reputationTracker is responsible for tracking the traffic of a channel to
// build a local view of it's history as a forwarding partner, and implements
// resource bucketing for the channel's outgoing resources. We'll be tracking
// various different values used by our reputation scheme:
//   - incomingReputation: used when the channel forwards us an incoming HTLC,
//     and compared to the outgoing revenue of the target channel to decide
//     whether the node has good reputation for incoming HTLCs. This reputation
//     is used to decide whether to grant the HTLC access to protected resources
//     on the outgoing link.
//   - outgoingReputation: used when the channel has been requested as the
//     outgoing link for a forward to decide whether to forward endorsed HTLCs.
//   - bidirectionalRevenue: the revenue that the link has earned us, as both an
//     incoming and outgoing link. This value is used to determine whether other
//     channels have sufficient reputation to forward htlcs over this channel.
//
// To keep track of resource bucketing and in-flight HTLCs, we track both
// incoming and outgoing in-flight HTLCs on the channel against our allocated
// general resources.
type reputationTracker struct {
	// bidirectionalRevenue tracks the revenue that the channel has earned
	// as both the incoming and outgoing link.
	bidirectionalRevenue *decayingAverage

	// incomingReputation tracks the revenue that the channel has earned us
	// as the incoming link.
	incomingReputation *decayingAverage

	// outgoingReputation tracks the revenue that the channel has earned us
	// as the outgoing link.
	outgoingReputation *decayingAverage

	// incomingInFlight provides a map of in-flight HTLCs, keyed by htlc id
	// on the incoming link.
	incomingInFlight map[int]*InFlightHTLC

	// outgoingInFlight provides a map of in-flight HTLCs that use the
	// channel as the outgoing link. These HTLCs are keyed by incoming
	// channel and index, because we don't yet have a unique identifier for
	// the outgoing htlc.
	outgoingInFlight map[lnwire.ShortChannelID]map[int]outgoingHTLC

	// incomingUtilization tracks utilization of the channel as an incoming
	// link.
	incomingUtilization channelUtilization

	// outgoingUtilization tracks utilization of the channel as an outgoing
	// link.
	outgoingUtilization channelUtilization

	// blockTime is the expected time to find a block, surfaced to account
	// for simulation scenarios where this isn't 10 minutes.
	blockTime float64

	// resolutionPeriod is the amount of time that we reasonably expect
	// a htlc to resolve in.
	resolutionPeriod time.Duration

	// generalLiquidity is the amount of liquidity available in the channel
	// on our side (outgoing direction).
	generalLiquidity lnwire.MilliSatoshi

	// generalSlots is the number of slots available in the channel on our
	// side (outgoing direction).
	generalSlots int

	log Logger
}

// outgoingHTLC tracks the minimal amount of information that we need on hand
// for an outgoing HTLC to keep track of our resource buckets.
type outgoingHTLC struct {
	amount    lnwire.MilliSatoshi
	risk      float64
	protected bool
}

// Reputation returns the reputation score for a link as either an incoming or
// outgoing source of traffic.
func (r *reputationTracker) Reputation(htlc *ProposedHTLC,
	incoming bool) Reputation {

	rep := Reputation{
		Revenue:           r.bidirectionalRevenue.getValue(),
		Reputation:        r.incomingReputation.getValue(),
		InFlightRisk:      r.inFlightHTLCRisk(incoming),
		HTLCRisk:          r.htlcRisk(htlc, incoming),
		UtilizationFactor: r.outgoingUtilization.maxUtilization(),
	}

	if !incoming {
		rep.Reputation = r.outgoingReputation.getValue()
		rep.UtilizationFactor = r.incomingUtilization.maxUtilization()
	}

	return rep
}

// MayAddOutgoing examines the htlc's reputation and our available resources
// to make a forwarding decision.
func (r *reputationTracker) MayAddOutgoing(reputation ReputationCheck,
	amt lnwire.MilliSatoshi, incomingEndorsed bool) ForwardOutcome {

	// We can always accommodate HTLCs that are endorsed and have sufficient
	// reputation.
	if reputation.SufficientReputation() && incomingEndorsed {
		return ForwardOutcomeEndorsed
	}

	// If the outgoing channel does not have reputation and the incoming
	// htlc is endorsed, we don't risk forwarding a htlc to an unknown
	// entity.
	if !reputation.OutgoingReputation() && incomingEndorsed {
		return ForwardOutcomeOutgoingUnkonwn
	}

	// Otherwise, we're looking to add the HTLC to our general resources.
	// Check whether we can accommodate it based on our current set of in
	// flight htlcs.
	var (
		generalSlotsOccupied      int
		generalLiquidityOccuplied lnwire.MilliSatoshi
	)

	for _, channel := range r.outgoingInFlight {
		for _, htlc := range channel {
			// We're only counting occupancy of our general bucket.
			if htlc.protected {
				continue
			}

			generalSlotsOccupied++
			generalLiquidityOccuplied += htlc.amount
		}
	}

	if generalSlotsOccupied+1 > r.generalSlots {
		return ForwardOutcomeNoResources
	}

	if generalLiquidityOccuplied+amt > r.generalLiquidity {
		return ForwardOutcomeNoResources
	}

	return ForwardOutcomeUnendorsed
}

// AddIncomingInFlight updates the incoming channel's view to include a new in
// flight HTLC.
func (r *reputationTracker) AddIncomingInFlight(htlc *ProposedHTLC,
	outgoingDecision ForwardOutcome) error {

	inFlightHTLC := &InFlightHTLC{
		TimestampAdded:   r.incomingReputation.clock.Now(),
		ProposedHTLC:     htlc,
		OutgoingDecision: outgoingDecision,
	}

	// Sanity check whether the HTLC is already present.
	if _, ok := r.incomingInFlight[htlc.IncomingIndex]; ok {
		return fmt.Errorf("%w: %v", ErrDuplicateIndex,
			htlc.IncomingIndex)
	}

	r.incomingUtilization.addHtlc(htlc.IncomingAmount)
	r.incomingInFlight[htlc.IncomingIndex] = inFlightHTLC

	return nil
}

// AddOutgoingInFlight updates the outgoing channel's view to include the in
// flight htlc.
func (r *reputationTracker) AddOutgoingInFlight(htlc *ProposedHTLC,
	protectedResources bool) error {

	incomingChanHTLCs, ok := r.outgoingInFlight[htlc.IncomingChannel]
	if !ok {
		r.outgoingInFlight[htlc.IncomingChannel] = make(
			map[int]outgoingHTLC,
		)
	}

	// Sanity check that HTLC is not already checked.
	if _, ok := incomingChanHTLCs[htlc.IncomingIndex]; ok {
		return fmt.Errorf("AddOutgoinInFlight: %w: %v",
			ErrDuplicateIndex, htlc.IncomingIndex)
	}

	r.outgoingUtilization.addHtlc(htlc.OutgoingAmount)
	r.outgoingInFlight[htlc.IncomingChannel][htlc.IncomingIndex] = outgoingHTLC{
		amount:    htlc.OutgoingAmount,
		protected: protectedResources,
		// We track this risk here so that we don't have to store htlc
		// fee and expiry values in outgoing.
		risk: r.calcInFlightRisk(htlc, false),
	}

	return nil
}

// addBidirectionalRevenue adds a htlc to the bidirectional revenue tracker's
// running total if it was successful.
func (r *reputationTracker) updateRevenue(incoming bool, inFlight *InFlightHTLC,
	htlc *ResolvedHTLC) {

	effectiveFees := effectiveFees(
		r.resolutionPeriod, htlc.TimestampSettled, inFlight,
		htlc.Success,
	)

	// First, update either the incoming or outgoing direction with the
	// effective fees for the HTLC.
	if incoming {
		r.log.Infof("Adding effective fees: %v to incoming channel: %v: %v",
			effectiveFees, htlc.IncomingChannel, effectiveFees)

		r.incomingReputation.add(effectiveFees)
	} else {
		r.log.Infof("Adding effective fees: %v to outgoing channel: %v: %v",
			effectiveFees, htlc.OutgoingChannel, effectiveFees)

		r.outgoingReputation.add(effectiveFees)
	}

	if !htlc.Success {
		return
	}

	r.log.Infof("HTLC successful (%v(%v) -> %v(%v)): adding fees "+
		"to channel: %v msat", htlc.IncomingChannel,
		htlc.IncomingIndex, htlc.OutgoingChannel,
		htlc.OutgoingIndex, inFlight.ForwardingFee())

	r.bidirectionalRevenue.add(float64(inFlight.ForwardingFee()))
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
			htlc.IncomingChannel, htlc.IncomingIndex,
			htlc.OutgoingChannel, htlc.OutgoingIndex)
	}

	delete(r.incomingInFlight, inFlight.IncomingIndex)

	// Add the htlc to both our incoming and bidirectional totals.
	r.updateRevenue(true, inFlight, htlc)

	return inFlight, nil
}

// ResolveOutgoing removes a htlc from the reputation tracker's state,
// returning an error if it is not found.
func (r *reputationTracker) ResolveOutgoing(htlc *InFlightHTLC,
	resolution *ResolvedHTLC) error {

	inFlightChan, ok := r.outgoingInFlight[htlc.IncomingChannel]
	if !ok {
		return fmt.Errorf("Outgoing channel lookup %w: %v(%v)",
			ErrResolutionNotFound, htlc.IncomingChannel,
			htlc.IncomingIndex)
	}

	if _, ok := inFlightChan[htlc.IncomingIndex]; !ok {
		return fmt.Errorf("Outgoing index lookup: %w: %v(%v)",
			ErrResolutionNotFound, htlc.IncomingChannel,
			htlc.IncomingIndex)
	}

	delete(inFlightChan, htlc.IncomingIndex)

	// We also add the effective fees to the outgoing channel's revenue, to
	// hold it accountable for the htlc's behavior.
	r.updateRevenue(false, htlc, resolution)

	// If there's nothing left, we can delete the channel map as well.
	if len(inFlightChan) == 0 {
		delete(r.outgoingInFlight, htlc.IncomingChannel)
		return nil
	}

	r.outgoingInFlight[htlc.IncomingChannel] = inFlightChan

	return nil
}

// inFlightHTLCRisk returns the total outstanding risk of the incoming
// in-flight HTLCs from a specific channel.
func (r *reputationTracker) inFlightHTLCRisk(incoming bool) float64 {
	var inFlightRisk float64
	if !incoming {
		for _, incomingChan := range r.outgoingInFlight {
			for _, htlc := range incomingChan {
				// Note: this risk uses "stale" utilization
				// because we don't store all the values
				// required to calculate risk fresh every time.
				inFlightRisk += htlc.risk
			}
		}

		return inFlightRisk
	}

	for _, htlc := range r.incomingInFlight {
		inFlightRisk += r.calcInFlightRisk(htlc.ProposedHTLC, true)
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

func (r reputationTracker) calcInFlightRisk(htlc *ProposedHTLC,
	incoming bool) float64 {

	// Only endorsed HTLCs count towards our in flight risk.
	if htlc.IncomingEndorsed != EndorsementTrue {
		return 0
	}

	return r.htlcRisk(htlc, incoming)
}

// htlcRisk calculates the outstanding risk of in-flight HTLCs, regardless of
// whether it's endorsed or not.
func (r reputationTracker) htlcRisk(htlc *ProposedHTLC,
	incoming bool) float64 {

	utiliation := r.incomingUtilization.maxUtilization()
	if !incoming {
		utiliation = r.outgoingUtilization.maxUtilization()
	}

	return (float64(htlc.ForwardingFee()) *
		float64(htlc.CltvExpiryDelta) * r.blockTime * 60) /
		r.resolutionPeriod.Seconds() * utiliation
}
