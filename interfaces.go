package localreputation

import (
	"time"

	"github.com/lightningnetwork/lnd/lnwire"
)

// LocalReputationManager is an interface representing an entity that tracks
// the reputation of channel peers based on HTLC forwarding behavior.
type LocalReputationManager interface {
	// SufficientReputation returns a boolean indicating whether a proposed
	// HTLC forward has sufficient reputation for the outgoing channel
	// requested.
	SufficientReputation(htlc *ProposedHTLC) bool

	// ForwardHTLC updates the reputation manager to reflect that a
	// proposed HTLC has been forwarded.
	ForwardHTLC(htlc *ProposedHTLC)

	// ResolveHTLC updates the reputation manager to reflect that an
	// in-flight HLTC has been resolved.
	ResolveHTLC(htlc *ResolvedHLTC)
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

	// ForwardingFee is the value that is offered by the HTLC.
	ForwardingFee lnwire.MilliSatoshi

	// CltvExpiryDelta is the difference between the block height at which
	// the HTLC was forwarded and its outgoing_cltv_expiry.
	CltvExpiryDelta uint32
}

// InFlightHTLC tracks a HTLC forward that is currently in flight.
type InFlightHTLC struct {
	// TimestampAdded is the time at which the incoming HTLC was added to
	// the incoming channel.
	TimestampAdded time.Time

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
