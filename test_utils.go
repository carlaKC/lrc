package lrc

import (
	"fmt"
	"time"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/mock"
)

type TestLogger struct{}

func (t *TestLogger) Infof(template string, args ...interface{}) {
	fmt.Printf("INFO:"+template+"\n", args...)
}

func (t *TestLogger) Debugf(template string, args ...interface{}) {
	fmt.Printf("DBG:"+template+"\n", args...)
}

// MockClock implements the `clock.Clock` interface.
type MockBucket struct {
	mock.Mock
}

// Compile time assertion that MockClock implements clock.Clock.
var _ resourceBucketer = (*MockBucket)(nil)

func (m *MockBucket) addHTLC(protected bool, amount lnwire.MilliSatoshi) bool {
	args := m.Called(protected, amount)

	return args.Get(0).(bool)
}

func (m *MockBucket) removeHTLC(protected bool, amount lnwire.MilliSatoshi) {
	m.Called(protected, amount)
}

const mockProposedFee = 1000

// mockProposedHtlc returns a proposed htlc over the channel pair provided.
func mockProposedHtlc(chanIn, chanOut uint64, idx int,
	endorsed bool) *ProposedHTLC {

	return &ProposedHTLC{
		IncomingChannel:  lnwire.NewShortChanIDFromInt(chanIn),
		OutgoingChannel:  lnwire.NewShortChanIDFromInt(chanOut),
		IncomingIndex:    idx,
		IncomingEndorsed: NewEndorsementSignal(endorsed),
		IncomingAmount:   10000 + mockProposedFee,
		OutgoingAmount:   10000,
		CltvExpiryDelta:  80,
	}
}

func resolutionForProposed(proposed *ProposedHTLC, success bool,
	settleTS time.Time) *ResolvedHTLC {

	return &ResolvedHTLC{
		IncomingChannel:  proposed.IncomingChannel,
		IncomingIndex:    proposed.IncomingIndex,
		OutgoingChannel:  proposed.OutgoingChannel,
		Success:          success,
		TimestampSettled: settleTS,
	}
}

// MockRevenue mocks out revenueMontior.
type MockRevenue struct {
	mock.Mock
}

func (m *MockRevenue) AddInFlight(htlc *ProposedHTLC,
	sufficientReputation bool) ForwardOutcome {

	args := m.Called(htlc, sufficientReputation)

	return args.Get(0).(ForwardOutcome)
}

func (m *MockRevenue) ResolveInFlight(htlc *ResolvedHTLC,
	inFlight *InFlightHTLC) error {

	return m.Called(htlc, inFlight).Error(0)
}

func (m *MockRevenue) Revenue() float64 {
	return m.Called().Get(0).(float64)
}

type MockReputation struct {
	mock.Mock
}

func (m *MockReputation) AddIncomingInFlight(htlc *ProposedHTLC,
	outgoingDecision ForwardOutcome) error {

	args := m.Called(htlc, outgoingDecision)

	return args.Error(0)
}

func (m *MockReputation) AddOutgoingInFlight(htlc *ProposedHTLC) error {
	args := m.Called(htlc)

	return args.Error(0)
}

func (m *MockReputation) ResolveIncoming(htlc *ResolvedHTLC,
	incoming bool) (*InFlightHTLC, error) {

	args := m.Called(htlc, incoming)

	return args.Get(0).(*InFlightHTLC), args.Error(1)
}

func (m *MockReputation) Reputation(incoming bool) Reputation {
	args := m.Called(incoming)

	return args.Get(0).(Reputation)
}
