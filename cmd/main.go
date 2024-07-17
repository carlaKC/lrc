package main

import (
	"encoding/csv"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/carlakc/lrc"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lnwire"
)

func main() {
	// Check if the file path argument is provided
	if len(os.Args) < 2 {
		fmt.Println("Provide path to data as argument")
		return
	}

	file, err := os.Open(os.Args[1])
	if err != nil {
		fmt.Println("Error opening file:", err)
		return
	}
	defer file.Close()

	// Create a new CSV reader
	reader := csv.NewReader(file)
	fwds, err := readCSV(reader)
	if err != nil {
		fmt.Println("Failed to read file: ", err)
		os.Exit(1)
	}

	network, err := lrc.BootstrapNetwork(
		lrc.ManagerParams{
			RevenueWindow:        time.Hour,
			ReputationMultiplier: 12,
			ProtectedPercentage:  50,
			ResolutionPeriod:     time.Second * 90,
			BlockTime:            5,
		}, fwds, clock.NewDefaultClock(),
	)
	if err != nil {
		fmt.Println("Could not bootstrap network ", err)
		os.Exit(1)
	}

	for alias, channels := range network {
		fmt.Printf("Node: %v has %v incoming / %v outgoing\n",
			alias, len(channels.Incoming), len(channels.Outgoing))
	}
}

// incoming_amt,incoming_expiry,incoming_add_ts,incoming_remove_ts,outgoing_amt,outgoing_expiry,outgoing_add_ts,outgoing_remove_ts,forwarding_node,forwarding_alias,chan_in,chan_out
func readCSV(reader *csv.Reader) ([]*lrc.NetworkForward, error) {
	// Read in all the CSV records
	records, err := reader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("Error reading CSV: %v", err)
	}

	// Slice to store parsed data
	var forwardedHTLCs []*lrc.NetworkForward

	// Process each row
	for _, row := range records[1:] {
		// Assuming the CSV has at least 12 columns as per the example format provided
		if len(row) < 12 {
			fmt.Println("Invalid CSV format: each row should have at least 12 columns.")
			continue
		}

		var (
			inFlightHTLC = lrc.InFlightHTLC{
				ProposedHTLC: &lrc.ProposedHTLC{},
			}
			resolvedHTLC = lrc.ResolvedHTLC{
				// We only store successful htlcs.
				Success: true,
			}
		)

		for i := 0; i < 6; i++ {
			val, err := strconv.ParseUint(row[i], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("Error parsing column %d as uint64: %v\n", i+1, err)
			}

			switch i {
			case 0:
				inFlightHTLC.ProposedHTLC.IncomingAmount = lnwire.MilliSatoshi(val)
			case 2:
				inFlightHTLC.TimestampAdded = time.Unix(0, int64(val))
			case 3:
				resolvedHTLC.TimestampSettled = time.Unix(0, int64(val))
			case 4:
				inFlightHTLC.ProposedHTLC.OutgoingAmount = lnwire.MilliSatoshi(val)
			}
		}

		chanIn, err := strconv.ParseUint(row[10], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("Error parsing column 10 as uint64: %v\n", err)
		}
		inFlightHTLC.ProposedHTLC.IncomingChannel = lnwire.NewShortChanIDFromInt(chanIn)
		resolvedHTLC.IncomingChannel = lnwire.NewShortChanIDFromInt(chanIn)

		chanOut, err := strconv.ParseUint(row[11], 10, 64)
		if err != nil {
			fmt.Printf("Error parsing column 11 as uint64: %v\n", err)
			continue
		}
		inFlightHTLC.ProposedHTLC.OutgoingChannel = lnwire.NewShortChanIDFromInt(chanOut)
		resolvedHTLC.OutgoingChannel = lnwire.NewShortChanIDFromInt(chanOut)

		networkHtlc := lrc.NetworkForward{
			NodeAlias: row[9],
			ForwardedHTLC: &lrc.ForwardedHTLC{
				InFlightHTLC: inFlightHTLC,
				Resolution:   &resolvedHTLC,
			},
		}

		// Append to slice
		forwardedHTLCs = append(forwardedHTLCs, &networkHtlc)
	}

	return forwardedHTLCs, nil
}
