package lrc

import (
	"fmt"

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

// mockProposedHtlc returns a proposed htlc over the channel pair provided.
func mockProposedHtlc(chanIn, chanOut uint64, idx int,
	endorsed bool) *ProposedHTLC {

	return &ProposedHTLC{
		IncomingChannel:  lnwire.NewShortChanIDFromInt(chanIn),
		OutgoingChannel:  lnwire.NewShortChanIDFromInt(chanOut),
		IncomingIndex:    idx,
		IncomingEndorsed: NewEndorsementSignal(endorsed),
		IncomingAmount:   11000,
		OutgoingAmount:   10000,
		CltvExpiryDelta:  80,
	}
}
