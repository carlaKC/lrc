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
	ForwardHTLC(htlc *ProposedHTLC, chanIn,
		chanOut ChannelInfo) (*ForwardDecision, error)

	// ResolveHTLC updates the reputation manager to reflect that an
	// in-flight htlc has been resolved. It returns the in flight HTLC as
	// tracked by the manager. It will error if the HTLC is not found.
	//
	// Note that this API expects resolutions to be reported for *all*
	// HTLCs, even if the decision for ForwardHTLC was that we have no
	// resources for the forward - this function must still be used to
	// indicate that the HTLC has been cleared from our state (as it would
	// have been locked in on our incoming link).
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

type ReputationCheck struct {
	IncomingChannel Reputation

	OutgoingChannel Reputation
}

// IncomingReputation returns a boolean indicating whether the proposed forward
// has good reputation in the incoming direction.
func (r ReputationCheck) IncomingReputation() bool {
	risk := r.IncomingChannel.InFlightRisk + r.OutgoingChannel.HTLCRisk
	return r.IncomingChannel.Reputation > r.OutgoingChannel.Revenue+risk
}

// OutgoingReputation returns a boolean indicating whether the proposed forward
// has good reputation in the outgoing direction.
func (r ReputationCheck) OutgoingReputation() bool {
	risk := r.OutgoingChannel.InFlightRisk + r.IncomingChannel.HTLCRisk
	return r.OutgoingChannel.Reputation > r.IncomingChannel.Revenue+risk
}

// SufficientReputation returns a boolean indicating whether the forward has
// sufficient reputation.
func (r ReputationCheck) SufficientReputation() bool {
	return r.IncomingReputation() && r.OutgoingReputation()
}

func (r ReputationCheck) String() string {
	return fmt.Sprintf("Reputation check for HTLC risk %v\n"+
		"- Incoming check (%v): %v - inflight %v - htlc %v > %v\n"+
		"- Outgoing check (%v): %v - inflight %v - htlc %v > %v",
		r.SufficientReputation(),
		r.IncomingReputation(), r.IncomingChannel.Revenue,
		r.IncomingChannel.InFlightRisk, r.OutgoingChannel.HTLCRisk,
		r.OutgoingChannel.Revenue, r.OutgoingReputation(),
		r.OutgoingChannel.Reputation, r.OutgoingChannel.InFlightRisk,
		r.IncomingChannel.HTLCRisk, r.IncomingChannel.Revenue)
}

// Reputation reflects the components that make up the reputation of a link in
// the outgoing direction.
type Reputation struct {
	// Revenue represents the revenue that the forwarding channel has
	// accrued over time bidirectionally. This value represents the
	// threshold that other channels must meet to have good reputation with
	// this channel.
	Revenue float64

	// Reputation represents the directional fee contribution from this
	// channel: (either as the outgoing or the incoming link) that has built
	// - Incoming: the fees earned with this channel as the incoming link.
	// - Outgoing: the fees earned with this channel as the outgoing link.
	Reputation float64

	// InFlightRisk represents the directional outstanding risk of the
	// outstanding HTLCs with the channel:
	// - Incoming reputation: the HTLCs the channel has forwarded us.
	// - Outgoing reputation: the HTLCs we have forwarded the channel.
	InFlightRisk float64

	// HTLC risk is the risk of the proposed HTLC's addition to the
	// channel in the selected direction.
	HTLCRisk float64
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

	// ForwardOutcomeOutgoingUnkonwn is returned when the outgoing link
	// does not have sufficient reputation and htlc is endorsed. The
	// forwarding node must drop this payment or risk being jammed by the
	// unknown downstream peer.
	ForwardOutcomeOutgoingUnkonwn
)

// NoForward returns true if we're expected to drop the HTLC.
func (f ForwardOutcome) NoForward() bool {
	return f == ForwardOutcomeNoResources ||
		f == ForwardOutcomeOutgoingUnkonwn
}

func (f ForwardOutcome) String() string {
	switch f {
	case ForwardOutcomeEndorsed:
		return "endorsed"

	case ForwardOutcomeUnendorsed:

		return "unendorsed"

	case ForwardOutcomeNoResources:
		return "no resources"

	case ForwardOutcomeOutgoingUnkonwn:
		return "unknown outgoing"

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

// reputationMonitor is an interface that represents the tracking of reputation
// for links forwarding htlcs.
type reputationMonitor interface {
	// AddIncomingFlight updates the reputation monitor for an incoming
	// link to reflect that it currently has an outstanding forwarded htlc.
	AddIncomingInFlight(htlc *ProposedHTLC,
		outgoingDecision ForwardOutcome) error

	// AddOutgoingInFlight updates the reputation monitor for an outgoing
	// link to reflect that it currently has an outstanding htlc where it
	// is the outgoing link.
	AddOutgoingInFlight(htlc *ProposedHTLC, protectedResource bool) error

	// MayAddOutgoing examines whether a HTLC can be added to the channel,
	// returning a forwarding decision based on its endorsement and
	// reputation.
	MayAddOutgoing(reputation ReputationCheck,
		amt lnwire.MilliSatoshi, incomingEndorsed bool) ForwardOutcome

	// ResolveIncoming updates the reputation monitor to resolve a
	// previously in-flight htlc on the incoming link.
	ResolveIncoming(htlc *ResolvedHTLC) (*InFlightHTLC, error)

	// ResolveOutgoing updates the reputation monitor to resolve a
	// previously in-flight htlc on the outgoing link.
	ResolveOutgoing(htlc *InFlightHTLC, resolution *ResolvedHTLC) error

	// Reputation returns the details of a reputation monitor's current
	// standing as either an incoming or outgoing link.
	Reputation(htlc *ProposedHTLC, incoming bool) Reputation
}

// revenueMonitor is an interface that represents the tracking of forwading
// revenues for targeted outgoing links.
type revenueMonitor interface {
	// AddInFlight proposes the addition of a htlc to the outgoing channel,
	// returning a forwarding decision for the htlc based on its
	// endorsement and the reputation of the incoming link.
	AddInFlight(htlc *ProposedHTLC, sufficientReputation bool) ForwardOutcome

	// ResolveInFlight removes a htlc from the outgoing channel.
	ResolveInFlight(htlc *ResolvedHTLC, inFlight *InFlightHTLC) error
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
		return "endorsed"

	case EndorsementFalse:
		return "unendorsed"

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

	// OutgoingDecision indicates what resource allocation was assigned to
	// the outgoing htlc.
	OutgoingDecision ForwardOutcome

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

	// OutgoingIndex is the HTLC ID on the outgoing link. Note that HTLCs
	// that fail locally won't have this value assigned.
	OutgoingIndex int

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

// ChannelHistory provides historical values for reputation and revenue.
type ChannelHistory struct {
	// IncomingReputation is the reputation that the channel has built by
	// forwarding incoming htlcs.
	IncomingReputation *DecayingAverageStart

	// OutgoingReputation is the reputation that the channel has built by
	// forwarding outgoing htlcs.
	OutgoingReputation *DecayingAverageStart

	// Revenue is the bidirectional revenue of the channel that represents
	// our reputation threshold for other channels.
	Revenue *DecayingAverageStart
}

func (c *ChannelHistory) String() string {
	str := ""

	if c.IncomingReputation != nil {
		str = fmt.Sprintf("incoming reputation: %v\n",
			c.IncomingReputation.Value)
	}

	if c.OutgoingReputation != nil {
		str = fmt.Sprintf("%voutgoing reputation: %v\n", str,
			c.OutgoingReputation.Value)
	}

	if c.Revenue != nil {
		str = fmt.Sprintf("%vrevenue: %v", str, c.Revenue.Value)
	}

	if str == "" {
		return "no history"
	}

	return str
}
