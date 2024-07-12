package lrc

import (
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// Creates a forwarding record with the values provided, hardcoding a few
// irrelevant details (endorsement, resolution time < period in params,
// success).
func createForwardAt(ts time.Time, fees lnwire.MilliSatoshi,
	scid lnwire.ShortChannelID, incoming bool) *ForwardedHTLC {

	incomingChannel := scid
	outgoingChannel := lnwire.NewShortChanIDFromInt(9999)

	if !incoming {
		incomingChannel = outgoingChannel
		outgoingChannel = scid
	}

	return &ForwardedHTLC{

		InFlightHTLC: InFlightHTLC{
			TimestampAdded: ts,
			ProposedHTLC: &ProposedHTLC{
				IncomingChannel:  incomingChannel,
				OutgoingChannel:  outgoingChannel,
				IncomingEndorsed: EndorsementTrue,
				// Just set difference to fees.
				IncomingAmount: fees,
				OutgoingAmount: 0,
			},
		},
		Resolution: &ResolvedHTLC{
			TimestampSettled: ts,
			Success:          true,
			IncomingChannel:  incomingChannel,
			OutgoingChannel:  outgoingChannel,
		},
	}
}

// TestBootstrap tests bootstrap of revenue and reputation at the same time,
// as they use very similar data.
func TestBootstrap(t *testing.T) {
	scid := lnwire.NewShortChanIDFromInt(100)
	testClock := clock.NewTestClock(testTime)

	// Test the zero case.
	repBootstrap, err := BootstrapReputation(scid, testParams, nil, testClock)
	require.NoError(t, err)
	require.Nil(t, repBootstrap)

	revBootstrap, err := BootstrapRevenue(scid, testParams, nil, testClock)
	require.NoError(t, err)
	require.Nil(t, revBootstrap)

	// To test our bootstrap, we'll create a decaying average and add
	// values as normal, storing each value as we go. Then, we'll run our
	// bootstrap with all the values we've added and compare the two.
	var (
		incomingHTLCs     []*ForwardedHTLC
		outgoingHTLCs     []*ForwardedHTLC
		reputationTracker = newDecayingAverage(
			testClock, testParams.reputationWindow(), nil,
		)
		revenueTracker = newDecayingAverage(
			testClock, testParams.RevenueWindow, nil,
		)
	)

	// Create a forward where we're the *incoming channel*, so it should
	// be added to our reputation tracker.
	fwd1 := createForwardAt(testClock.Now(), 100, scid, true)
	effectiveFees := effectiveFees(
		testParams.ResolutionPeriod,
		fwd1.Resolution.TimestampSettled, &fwd1.InFlightHTLC,
		fwd1.Resolution.Success,
	)

	reputationTracker.add(effectiveFees)
	incomingHTLCs = append(incomingHTLCs, fwd1)

	// Now, create two forwards where we're the *outgoing channel*, so it
	// should be added to our revenue tracker.
	testClock.SetTime(testClock.Now().Add(time.Hour))
	fwd2 := createForwardAt(testClock.Now(), 500, scid, false)
	revenueTracker.add(float64(fwd2.ForwardingFee()))
	outgoingHTLCs = append(outgoingHTLCs, fwd2)

	testClock.SetTime(testClock.Now().Add(time.Minute))
	fwd3 := createForwardAt(testClock.Now(), 1500, scid, false)
	revenueTracker.add(float64(fwd3.ForwardingFee()))
	outgoingHTLCs = append(outgoingHTLCs, fwd3)

	// Assert that we fail if we use the wrong htlcs.
	_, err = BootstrapRevenue(
		scid, testParams, incomingHTLCs, testClock,
	)
	require.Error(t, err)
	_, err = BootstrapReputation(
		scid, testParams, outgoingHTLCs, testClock,
	)
	require.Error(t, err)

	// Now, bootstrap these values for our channel.
	revenueStart, err := BootstrapRevenue(
		scid, testParams, outgoingHTLCs, testClock,
	)
	require.NoError(t, err)
	require.NotNil(t, revenueStart)

	reputationStart, err := BootstrapReputation(
		scid, testParams, incomingHTLCs, testClock,
	)
	require.NoError(t, err)
	require.NotNil(t, reputationStart)

	// Next, test the we have the same values for our bootstrap and "live"
	// averages.
	require.Equal(t, revenueTracker.getValue(), revenueStart.Value)
	require.Equal(t, reputationTracker.getValue(), reputationStart.Value)

	// Finally, create a decaying average from these "start" values and
	// assert that values remain the same as we add new entries and
	// progress time.
	reputationBootstrapped := newDecayingAverage(
		testClock, testParams.reputationWindow(), reputationStart,
	)
	revenueBootstrapped := newDecayingAverage(
		testClock, testParams.RevenueWindow, revenueStart,
	)
	// Add a new value to both sets of trackers, we can just hard-code a
	// value now that we don't need to save the forward itself.
	revenueTracker.add(105)
	revenueBootstrapped.add(105)

	reputationTracker.add(600)
	reputationBootstrapped.add(600)

	testClock.SetTime(testClock.Now().Add(time.Minute * 8))
	require.Equal(
		t, reputationTracker.getValue(), reputationBootstrapped.getValue(),
	)
	require.Equal(
		t, revenueTracker.getValue(), revenueBootstrapped.getValue(),
	)
}
