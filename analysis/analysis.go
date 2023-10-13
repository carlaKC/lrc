package analysis

import (
	"context"
	"fmt"
	"time"

	"github.com/carlakc/lrc/db"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
)

const (
	// TODO: pick a number that is informed by size of responses.
	maxPayments = 100_000
	maxForwards = 250_000
)

type Analysis struct {
	// The path to the targeted node's circuitbreaker database.
	targetDBPath string

	attacker *lndclient.GrpcLndServices
}

func NewAnalysis(attacker *lndclient.GrpcLndServices,
	targetDBPath string) *Analysis {

	return &Analysis{
		targetDBPath: targetDBPath,
		attacker:     attacker,
	}
}

type attackerData struct {
	payments map[lntypes.Hash]struct{}
	failures int
	fees     lnwire.MilliSatoshi
}

func (a *attackerData) String() string {
	return fmt.Sprintf("Attacker sent: %v payments paying %v fees",
		len(a.payments), a.fees)
}

type AttackerReport struct {
	// PaymentsSent is the total number of payments that the attacker sent
	// in its efforts to jam a targeted chanel.
	PaymentsSent int

	// PaymentsFailed is the number of the attacker's payments that were
	// failed.
	PaymentsFailed int

	// FeesPaid is the total fees paid by the attacker.
	FeesPaid lnwire.MilliSatoshi
}

func (a *attackerData) report() *AttackerReport {
	return &AttackerReport{
		PaymentsSent:   len(a.payments),
		PaymentsFailed: a.failures,
		FeesPaid:       a.fees,
	}
}

func newAttackerData() *attackerData {
	return &attackerData{
		payments: make(map[lntypes.Hash]struct{}),
	}
}

// attackerData returns the payment hashes of all payments sent by an attacking
// node and the total fees paid by those payments.
func (a *Analysis) attackerData(ctx context.Context) (*attackerData, error) {
	data := newAttackerData()

	var offset uint64
	for {
		pmts, err := a.attacker.Client.ListPayments(
			ctx, lndclient.ListPaymentsRequest{
				MaxPayments: maxPayments,
				Offset:      offset,
			},
		)
		if err != nil {
			return nil, err
		}

		for _, pmt := range pmts.Payments {
			if pmt.Status.State == lnrpc.Payment_FAILED {
				data.failures++
			}

			data.fees += pmt.Fee
			data.payments[pmt.Hash] = struct{}{}
		}

		// If we haven't returned the max payments, we've reached our
		// limit.
		if len(pmts.Payments) < maxPayments {
			break
		}

		offset = pmts.LastIndexOffset
	}

	return data, nil
}

type forward struct {
	hash lntypes.Hash
	fee  lnwire.MilliSatoshi
}

type targetData struct {
	// failedPayments is a map of all the payments that the targeted node
	// failed to their fee.
	failedPayments map[lntypes.Hash]lnwire.MilliSatoshi

	// forwardedPayments is a list of all the payments successfully
	// forwarded by the targeted node.
	forwardedPayments []forward
}

func newTargetData() *targetData {
	return &targetData{
		failedPayments: make(map[lntypes.Hash]lnwire.MilliSatoshi),
	}
}

func (t *targetData) String() string {
	return fmt.Sprintf("Target forwarded: %v payments, dropped: %v.",
		len(t.forwardedPayments), len(t.failedPayments))
}

func (a *Analysis) targetData(ctx context.Context) (*targetData, error) {
	data := newTargetData()

	db, err := db.NewDb("")
	if err != nil {
		return nil, err
	}
	defer db.Close()

	fwds, err := db.ListForwardingHistory(ctx, time.Time{}, time.Now())
	if err != nil {
		return nil, err
	}

	for _, fwd := range fwds {
		forward := forward{
			// TODO: surfacehash!
			fee: fwd.IncomingMsat - fwd.OutgoingMsat,
		}

		data.forwardedPayments = append(data.forwardedPayments, forward)
	}

	_, err = db.ListRejectedHTLCs(ctx, time.Time{}, time.Now())
	if err != nil {
		return nil, err
	}

	/*for _, htlc := range rejected {
	        data.failedPayments[_] = htlc.IncomingAmount - htlc.OutgoingAmount
	}*/

	return data, nil
}

type TargetReport struct {
	// ForwardedPayments is the count of payments that the jamming
	// mitigation allowed through.
	ForwardedPayments int

	// DroppedPayments is the count of the payments that the jamming
	// mitigation dropped.
	DroppedPayments int

	// DroppedHonestPayments is the count of payments that the jamming
	// mitigation dropped that were honest payments.
	DroppedHonestPayments int

	// TotalFees is the total fees earned from payments that were forwarded.
	TotalFees lnwire.MilliSatoshi

	// AttackerFees is the total fees paid by the attacker to forward
	// payments.
	AttackerFees lnwire.MilliSatoshi

	// DroppedHonestFees is the total amount of fees lost to honest
	// payments that were dropped by the mitigation.
	DroppedHonestFees lnwire.MilliSatoshi
}

type Report struct {
	Attacker *AttackerReport
	Target   *TargetReport
}

func generateReport(attacker *attackerData, target *targetData) *Report {
	report := &Report{
		Attacker: attacker.report(),
		Target: &TargetReport{
			ForwardedPayments: len(target.forwardedPayments),
			DroppedPayments:   len(target.failedPayments),
		},
	}

	for hash, amt := range target.failedPayments {
		// If a dropped payment is from the attacker, we successfully
		// cut them off. If not, then we've dropped honest payments
		// and lost fees that we could have earned if not under attack.
		if _, isMalicious := attacker.payments[hash]; !isMalicious {
			report.Target.DroppedPayments++
			report.Target.DroppedHonestFees += amt
		}
	}

	for _, fwd := range target.forwardedPayments {
		report.Target.TotalFees += fwd.fee

		if _, isMalicious := attacker.payments[fwd.hash]; isMalicious {
			report.Target.AttackerFees += fwd.fee
		}
	}

	return report
}
