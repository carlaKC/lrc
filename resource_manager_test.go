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
		}, &TestLogger{},
	)
	require.NoError(t, err)

	// Replace constructors with our mocks.
	r.newRevenueMonitor = mockDeps.newRevenueMonitor
	r.newReputationMonitor = mockDeps.newReputationMonitor

	return testClock, r
}

// mockDeps is used to mock out constructors for fetching our other mocked
// dependencies, so that we can return different values on different calls.
type mockDeps struct {
	mock.Mock
}

func (m *mockDeps) newRevenueMonitor(start *DecayingAverageStart,
	chanInfo *ChannelInfo) (revenueMonitor, error) {

	args := m.Called(start, chanInfo)
	return args.Get(0).(revenueMonitor), args.Error(1)
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

	// Set to a different value so that the mock can distinguish.
	chanInInfo := &ChannelInfo{
		InFlightHTLC:      300,
		InFlightLiquidity: 100000,
	}
	// Start with a HTLC that overflows.

	htlc0.OutgoingAmount = MaxMilliSatoshi + 1
	_, err := mgr.ForwardHTLC(htlc0, chanInInfo, chanOutInfo)
	require.ErrorIs(t, err, ErrAmtOverflow)

	// Start from scratch, when trackers need to be made for both of our
	// channels.
	chan100Incoming := &MockReputation{}
	defer chan100Incoming.AssertExpectations(t)

	chan200Incoming := &MockReputation{}
	defer chan200Incoming.AssertExpectations(t)

	chan200Outgoing := &MockRevenue{}
	chan200Outgoing.On("Revenue").Return(0.0)
	defer chan200Outgoing.AssertExpectations(t)

	chan100Outgoing := &MockRevenue{}
	chan100Outgoing.On("Revenue").Return(0.0)
	defer chan100Outgoing.AssertExpectations(t)

	// Mock for setting up new incoming and outgoing channels, allow
	// multiple calls for each direction.
	deps.On("newReputationMonitor", mock.Anything).Once().Return(
		chan100Incoming,
	)
	deps.On("newReputationMonitor", mock.Anything).Once().Return(
		chan200Incoming,
	)

	deps.On("newRevenueMonitor", mock.Anything, chanOutInfo).Return(
		chan200Outgoing, nil,
	)
	deps.On("newRevenueMonitor", mock.Anything, chanInInfo).Return(
		chan100Outgoing, nil,
	)

	// We don't actually use our incoming reputation info anywhere, so
	// we can just set it to return a single value every time.
	incomingRep := Reputation{}
	chan100Incoming.On("Reputation", mock.Anything).Return(incomingRep)
	chan200Incoming.On("Reputation", mock.Anything).Return(incomingRep)

	// Add a HTLC which is assessed to be able to forward, but not
	// endorsed - assert that it's forwarded without endorsement.
	htlc1 := mockProposedHtlc(100, 200, 0, true)
	chan200Outgoing.On("AddInFlight", htlc1, false).Once().Return(
		ForwardOutcomeUnendorsed)
	chan100Incoming.On("AddIncomingInFlight", htlc1, ForwardOutcomeUnendorsed).Once().Return(nil)

	f, err := mgr.ForwardHTLC(htlc1, chanInInfo, chanOutInfo)
	require.NoError(t, err)
	require.Equal(t, ForwardOutcomeUnendorsed, f.ForwardOutcome)

	// Next, add a HTLC that can't be forwarded due to lack of resources
	// and assert that it is not added to the incoming channel's in-flight.
	// Using the same channels, do we don't need any setup assertions.
	htlc2 := mockProposedHtlc(100, 200, 0, true)
	chan200Outgoing.On("AddInFlight", htlc2, false).Once().Return(
		ForwardOutcomeNoResources)
	chan100Incoming.On("AddIncomingInFlight", htlc2, ForwardOutcomeNoResources).Once().Return(nil)

	f, err = mgr.ForwardHTLC(htlc2, chanInInfo, chanOutInfo)
	require.NoError(t, err)
	require.Equal(t, ForwardOutcomeNoResources, f.ForwardOutcome)

	// Now, test resolving of HTLCs, staring with the obvious error cases
	// where the channels are not known to us (incoming).
	htlc3 := mockProposedHtlc(300, 400, 1, true)
	htlc3res := resolutionForProposed(htlc3, true, testTime)

	_, err = mgr.ResolveHTLC(htlc3res)
	require.ErrorIs(t, err, ErrChannelNotFound)

	// And the case where the incoming channel is known to us, but the
	// outgoing channel is not (and the htlc _was_ supposedly forwarded).
	// We'll call our resolution on the incoming channel then fail on the
	// outgoing channel (note that a non-mocked incoming channel would fail
	// on ResolveInFlight, so we're really just testing coverage here).
	htlc4 := mockProposedHtlc(100, 500, 1, false)
	htlc4res := resolutionForProposed(htlc4, false, testTime)
	chan100Incoming.On("ResolveInFlight", htlc4res).Once().Return(
		&InFlightHTLC{
			OutgoingDecision: ForwardOutcomeEndorsed,
			ProposedHTLC:     &ProposedHTLC{},
		}, nil,
	)

	_, err = mgr.ResolveHTLC(htlc4res)
	require.ErrorIs(t, err, ErrChannelNotFound)

	// Finally, test resolving of a htlc that was only added on the
	// incoming link, and not the outgoing link due to lack of resources.
	htlc5 := mockProposedHtlc(100, 500, 1, false)
	htlc5res := resolutionForProposed(htlc5, false, testTime)
	chan100Incoming.On("ResolveInFlight", htlc5res).Once().Return(
		&InFlightHTLC{
			OutgoingDecision: ForwardOutcomeNoResources,
			ProposedHTLC:     &ProposedHTLC{},
		}, nil,
	)

	_, err = mgr.ResolveHTLC(htlc5res)
	require.NoError(t, err)
}
