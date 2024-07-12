package lrc

import (
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

var (
	testParams = ManagerParams{
		RevenueWindow:        time.Hour,
		ReputationMultiplier: 24,
		BlockTime:            10,
		ProtectedPercentage:  50,
		ResolutionPeriod:     time.Second * 90,
	}
)

// setup creates a resource manager for testing with some sane values set.
// If a channel history function is not provided, we stub it out with an
// empty impl.
func setup(t *testing.T, mockDeps *mockDeps) (*clock.TestClock,
	*ResourceManager) {

	testClock := clock.NewTestClock(testTime)

	r, err := NewResourceManager(
		testParams,
		testClock,
		// Don't return any start values for reputation or revenue.
		func(_ lnwire.ShortChannelID) (*DecayingAverageStart, error) {
			return nil, nil
		},
		func(_ lnwire.ShortChannelID) (*DecayingAverageStart, error) {
			return nil, nil
		}, 50, &TestLogger{},
	)
	require.NoError(t, err)

	// Replace constructors with our mocks.
	r.newTargetMonitor = mockDeps.newTargetMonitor
	r.newReputationMonitor = mockDeps.newReputationMonitor

	return testClock, r
}

// mockDeps is used to mock out constructors for fetching our other mocked
// dependencies, so that we can return different values on different calls.
type mockDeps struct {
	mock.Mock
}

func (m *mockDeps) newTargetMonitor(start *DecayingAverageStart,
	chanInfo *ChannelInfo) (targetMonitor, error) {

	args := m.Called(start, chanInfo)
	return args.Get(0).(targetMonitor), args.Error(1)
}

func (m *mockDeps) newReputationMonitor(
	start *DecayingAverageStart) reputationMonitor {

	args := m.Called(start)
	return args.Get(0).(reputationMonitor)
}

// TestResourceManager tests resource manager's handling of HTLCs
func TestResourceManager(t *testing.T) {
	// Create a mock resource bucket and
	targetBucket := &MockBucket{}
	defer targetBucket.AssertExpectations(t)

	// We don't care about a history bootstrap function here.
	deps := &mockDeps{}
	defer deps.AssertExpectations(t)

	_, mgr := setup(t, deps)

	htlc0 := mockProposedHtlc(100, 200, 0, true)
	chanOutInfo := &ChannelInfo{
		InFlightHTLC:      483,
		InFlightLiquidity: 100000,
	}

	// Start with a HTLC that overflows.
	htlc0.OutgoingAmount = MaxMilliSatoshi + 1
	_, err := mgr.ForwardHTLC(htlc0, chanOutInfo)
	require.ErrorIs(t, err, ErrAmtOverflow)

	// Start from scratch, when trackers need to be made for both of our
	// channels.
	chan100Incoming := &MockReputation{}
	defer chan100Incoming.AssertExpectations(t)

	chan200Outgoing := &MockTarget{}
	defer chan200Outgoing.AssertExpectations(t)

	deps.On("newReputationMonitor", mock.Anything).Once().Return(
		chan100Incoming,
	)
	deps.On("newTargetMonitor", mock.Anything, chanOutInfo).Once().Return(
		chan200Outgoing, nil,
	)

	// We don't actually use our incoming reputation info anywhere, so
	// we can just set it to return a single value every time.
	incomingRep := IncomingReputation{}
	chan100Incoming.On("IncomingReputation").Return(incomingRep)

	// Add a HTLC which is assessed to be able to forward, but not
	// endorsed - assert that it's forwarded without endorsement.
	htlc1 := mockProposedHtlc(100, 200, 0, true)
	chan200Outgoing.On("AddInFlight", incomingRep, htlc1).Once().Return(
		ForwardDecision{ForwardOutcome: ForwardOutcomeUnendorsed},
	)
	chan100Incoming.On("AddInFlight", htlc1, EndorsementFalse).Once().Return(nil)

	f, err := mgr.ForwardHTLC(htlc1, chanOutInfo)
	require.NoError(t, err)
	require.Equal(t, ForwardOutcomeUnendorsed, f.ForwardOutcome)

	// Next, add a HTLC that can't be forwarded due to lack of resources
	// and assert that it is not added to the incoming channel's in-flight.
	// Using the same channels, do we don't need any setup assertions.
	htlc2 := mockProposedHtlc(100, 200, 0, true)
	chan200Outgoing.On("AddInFlight", incomingRep, htlc2).Once().Return(
		ForwardDecision{ForwardOutcome: ForwardOutcomeNoResources},
	)

	f, err = mgr.ForwardHTLC(htlc2, chanOutInfo)
	require.NoError(t, err)
	require.Equal(t, ForwardOutcomeNoResources, f.ForwardOutcome)

	// Now, test resolving of HTLCs, staring with the obvious error cases
	// where the channels are not known to us (incoming).
	htlc3 := mockProposedHtlc(300, 400, 1, true)
	htlc3res := resolutionForProposed(htlc3, true, testTime)

	_, err = mgr.ResolveHTLC(htlc3res)
	require.ErrorIs(t, err, ErrChannelNotFound)

	// And the case where the incoming channel is known to us, but the
	// outgoing channel is not. We'll call our resolution on the incoming
	// channel then fail on the outgoing channel (note that a non-mocked
	// incoming channel would fail on ResolveInFlight, so we're really
	// just testing coverage here).
	htlc4 := mockProposedHtlc(100, 500, 1, false)
	htlc4res := resolutionForProposed(htlc4, false, testTime)
	chan100Incoming.On("ResolveInFlight", htlc4res).Once().Return(
		&InFlightHTLC{}, nil,
	)

	_, err = mgr.ResolveHTLC(htlc4res)
	require.ErrorIs(t, err, ErrChannelNotFound)
}
