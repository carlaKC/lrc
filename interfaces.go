package lrc

import (
	"errors"
	"fmt"
	"time"

	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// ErrAmtOverflow indicates that a HTLC's amount is more than the
	// maximum theoretical number of millisatoshis.
	ErrAmtOverflow = errors.New("htlc amount overflows max msat")
)

// LocalResourceManager is an interface representing an entity that tracks
// the reputation of channel peers based on HTLC forwarding behavior.
type LocalResourceManager interface {
	// ForwardHTLC updates the reputation manager to reflect that a
	// proposed HTLC has been forwarded. It requires the forwarding
	// restrictions of the outgoing channel to implement bucketing
	// appropriately.
	ForwardHTLC(htlc *ProposedHTLC, info *ChannelInfo) (*ForwardDecision,
		error)

	// ResolveHTLC updates the reputation manager to reflect that an
	// in-flight htlc has been resolved. It returs the in flight HTLC as
	// tracked by the manager. If the HTLC is not known, it may return
	// nil.
	ResolveHTLC(htlc *ResolvedHTLC) (*InFlightHTLC, error)
}

// ForwardDecision contains the action that should be taken for forwarding
// a HTLC and debugging details of the values used.
type ForwardDecision struct {
	// ReputationCheck contains the numerical values used in making a
	// reputation decision. This value will be non-nil
	ReputationCheck

	// ForwardOutcome is the action that the caller should take.
	ForwardOutcome
}

// ReputationCheck provides the reputation scores that are used to make a
// forwarding decision for a HTLC. These are surfaced for the sake of debugging
// and simulation, and wouldn't really be used much in a production
// implementation.
type ReputationCheck struct {
	// IncomingReputation represents the reputation that has been built
	// up by the incoming link, and any outstanding risk that it poses to
	// us.
	IncomingReputation

	// OutgoingRevenue represents the cost of using the outgoing link,
	// evaluated based on how valuable it has been to us in the past.
	OutgoingRevenue float64

	// HTLCRisk represents the risk of the newly proposed HTLC, should it
	// be used to jam our channel for its full expiry time.
	HTLCRisk float64
}

type IncomingReputation struct {
	// IncomingRevenue represents the reputation that the forwarding
	// channel has accrued over time.
	IncomingRevenue float64

	// InFlightRisk represents the outstanding risk of all of the
	// forwarding party's currently in flight HTLCs.
	InFlightRisk float64
}

// SufficientReputation returns a boolean indicating whether a HTLC meets the
// reputation bar to be forwarded with endorsement.
func (r *ReputationCheck) SufficientReputation() bool {
	// The incoming channel has sufficient reputation if:
	// incoming_channel_revenue - in_flight_risk - htlc_risk
	//  >= outgoing_link_revenue
	return r.IncomingRevenue > r.OutgoingRevenue+r.InFlightRisk+r.HTLCRisk
}

// String for a ReputationCheck.
func (r *ReputationCheck) String() string {
	return fmt.Sprintf("outgoing revenue threshold: %v vs incoming "+
		"revenue: %v with in flight risk: %v and htlc risk: %v",
		r.OutgoingRevenue, r.IncomingRevenue, r.InFlightRisk,
		r.HTLCRisk)
}

// ForwardOutcome represents the various forwarding outcomes for a proposed
// HTLC forward.
type ForwardOutcome int

const (
	// ForwardOutcomeNoResources means that a HTLC should be dropped
	// because the resource bucket that it qualifies for is full.
	ForwardOutcomeNoResources ForwardOutcome = iota

	// ForwardOutcomeUnendorsed means that the HTLC should be forwarded but
	// not endorsed.
	ForwardOutcomeUnendorsed

	// ForwardOutcomeEndorsed means that the HTLC should be forwarded
	// with a positive endorsement signal.
	ForwardOutcomeEndorsed
)

func (f ForwardOutcome) String() string {
	switch f {
	case ForwardOutcomeEndorsed:
		return "endorsed"

	case ForwardOutcomeUnendorsed:

		return "unendorsed"

	case ForwardOutcomeNoResources:
		return "no resources"

	default:
		return "unknown"
	}
}

// resourceBucketer implements basic resource bucketing for local resource
// conservation.
type resourceBucketer interface {
	// addHTLC poses a HTLC to the resource manager for addition to its
	// appropriate bucket. If there is space for the HTLC, this call will
	// update internal state and return true. If the bucket is full, the
	// resource manager will return false and its state will remain
	// unchanged.
	addHTLC(protected bool, amount lnwire.MilliSatoshi) bool

	// removeHTLC updates the resource manager to remove an in-flight HTLC
	// from its appropriate bucket. Note that this must *only* be called
	// for HTLCs that were added with a true response from addHTLC.
	removeHTLC(protected bool, amount lnwire.MilliSatoshi)
}

// Endorsement represents the endorsement signaling that is passed along with
// a HTLC.
type Endorsement uint8

const (
	// EndorsementNone indicates that the TLV was not present.
	EndorsementNone Endorsement = iota

	// EndorsementFalse indicates that the TLV was present with a zero
	// value.
	EndorsementFalse

	// EndorsementTrue indicates that the TLV was present with a non-zero
	// value.
	EndorsementTrue
)

func (e Endorsement) String() string {
	switch e {
	case EndorsementNone:
		return "NULL"

	case EndorsementTrue:
		return "T"

	case EndorsementFalse:
		return "F"

	default:
		return "Unknown"
	}
}

// NewEndorsementSignal returns the enum representation of a boolean
// endorsement.
func NewEndorsementSignal(endorse bool) Endorsement {
	if endorse {
		return EndorsementTrue
	}

	return EndorsementFalse
}

// ProposedHTLC provides information about a HTLC has has been locked in on
// our incoming channel, but not yet forwarded.
type ProposedHTLC struct {
	// IncomingChannel is the channel that has sent this htlc to the local
	// node for forwarding.
	IncomingChannel lnwire.ShortChannelID

	// OutgoingChannel is the outgoing channel that the sending node has
	// requested.
	OutgoingChannel lnwire.ShortChannelID

	// IncomingIndex is the HTLC index on the incoming channel.
	IncomingIndex int

	// IncomingEndorsed indicates whether the incoming channel forwarded
	// this HTLC as endorsed.
	IncomingEndorsed Endorsement

	// IncomingAmount is the amount of the HTLC on the incoming channel.
	IncomingAmount lnwire.MilliSatoshi

	// OutgoingAmount is the amount of the HTLC on the outgoing channel.
	OutgoingAmount lnwire.MilliSatoshi

	// CltvExpiryDelta is the difference between the block height at which
	// the HTLC was forwarded and its outgoing_cltv_expiry.
	CltvExpiryDelta uint32
}

// ForwardingFee returns the fee paid by a htlc.
func (p *ProposedHTLC) ForwardingFee() lnwire.MilliSatoshi {
	return p.IncomingAmount - p.OutgoingAmount
}

// InFlightHTLC tracks a HTLC forward that is currently in flight.
type InFlightHTLC struct {
	// TimestampAdded is the time at which the incoming HTLC was added to
	// the incoming channel.
	TimestampAdded time.Time

	// OutgoingEndorsed indicates whether the outgoing htlc was endorsed
	// (and thus, that it occupied protected resources on the outgoing
	// channel).
	OutgoingEndorsed Endorsement

	// ProposedHTLC contains the original details of the HTLC that was
	// forwarded to us.
	// TODO: We probably don't need to store all of this info.
	*ProposedHTLC
}

// ResolvedHTLC summarizes the resolution of an in-flight HTLC.
type ResolvedHTLC struct {
	// TimestampSettled is the time at which a htlc was resolved.
	TimestampSettled time.Time

	// IncomingIndex is the HTLC ID on the incoming link.
	IncomingIndex int

	// IncomingChannel is the short channel ID of the channel that
	// originally forwarded the incoming HTLC.
	IncomingChannel lnwire.ShortChannelID

	// OutgoingChannel is the short channel ID of the channel that
	// forwarded the outgoing HTLC.
	OutgoingChannel lnwire.ShortChannelID

	// Success is true if the HTLC was fulfilled.
	Success bool
}

// ForwardedHTLC represents a HTLC that our node has previously forwarded.
type ForwardedHTLC struct {
	// InFlightHTLC contains the original forwarding details of the HTLC.
	InFlightHTLC

	// Resolution contains the details of the HTLC's resolution if it has
	// been finally settled or failed. If the HTLC is still in flight, this
	// field should be nil.
	Resolution *ResolvedHTLC
}

// ChannelInfo provides information about a channel's routing restrictions.
type ChannelInfo struct {
	// InFlightHTLC is the total number of htlcs allowed in flight.
	InFlightHTLC uint64

	// InFlightLiquidity is the total amount of liquidity allowed in
	// flight.
	InFlightLiquidity lnwire.MilliSatoshi
}

// Very basic logger interface so that calling code can provide us with logging
type Logger interface {
	Infof(template string, args ...interface{})
	Debugf(template string, args ...interface{})
}
