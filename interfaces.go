package localreputation

import (
	"time"

	"github.com/lightningnetwork/lnd/lnwire"
)

// LocalReputationManager is an interface representing an entity that tracks
// the reputation of channel peers based on HTLC forwarding behavior.
type LocalReputationManager interface {
	// ForwardHTLC updates the reputation manager to reflect that a
	// proposed HTLC has been forwarded.
	ForwardHTLC(htlc *ProposedHTLC) (ForwardOutcome, error)

	// ResolveHTLC updates the reputation manager to reflect that an
	// in-flight HLTC has been resolved.
	ResolveHTLC(htlc *ResolvedHLTC)
}

// ForwardOutcome represents the various forwarding outcomes for a proposed
// HTLC forward.
type ForwardOutcome int

const (
	// ForwardOutcomeError covers the zero values for error return so
	// that we don't return a meaningful enum by mistake.
	ForwardOutcomeError ForwardOutcome = iota

	// ForwardOutcomeNoResources means that a HTLC should be dropped
	// because the resource bucket that it qualifies for is full.
	ForwardOutcomeNoResources

	// ForwardOutcomeUnendorsed means that the HTLC should be forwarded but
	// not endorsed.
	ForwardOutcomeUnendorsed

	// ForwardOutcomeEndorsed means that the HTLC should be forwarded
	// with a positive endorsement signal.
	ForwardOutcomeEndorsed
)

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
	IncomingEndorsed bool

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

	// OutgoingEndorsed indicates whether the outgoing HLTC was endorsed
	// (and thus, that it occupied protected resources on the outgoing
	// channel).
	OutgoingEndorsed bool

	// ProposedHTLC contains the original details of the HTLC that was
	// forwarded to us.
	// TODO: We probably don't need to store all of this info.
	*ProposedHTLC
}

// ResolvedHLTC summarizes the resolution of an in-flight HTLC.
type ResolvedHLTC struct {
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

// ChannelInfo provides information about a channel's routing restrictions.
type ChannelInfo struct {
	// InFlightHTLC is the total number of htlcs allowed in flight.
	InFlightHTLC uint64

	// InFlightLiquidity is the total amount of liquidity allowed in
	// flight.
	InFlightLiquidity lnwire.MilliSatoshi
}
