package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/carlakc/lrc"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lnwire"
)

func main() {
	// Check if the file path argument is provided
	if len(os.Args) < 2 {
		fmt.Println("Please provide basedir containing data.csv and .json of graph")
		return
	}

	// First, pull out the generated data for the graph and get all
	// existing reputation scores for it.
	baseDir := os.Args[1]
	dataFile := path.Join(baseDir, "data.csv")

	file, err := os.Open(dataFile)
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
			RevenueWindow:        time.Hour * 24 * 14,
			ReputationMultiplier: 24,
			ProtectedPercentage:  50,
			ResolutionPeriod:     time.Second * 90,
			BlockTime:            5,
		}, fwds, clock.NewDefaultClock(),
	)
	if err != nil {
		fmt.Println("Could not bootstrap network ", err)
		os.Exit(1)
	}

	// Next, get the full graph file and collect channels for each node.
	// It's possible that some channels don't have any activity at all,
	// and thus no reputation, and we want to include them in our count of
	// reputation pairs.
	graphFile := path.Join(baseDir, "simln.json")
	graphJson, err := os.Open(graphFile)
	if err != nil {
		fmt.Println("Error opening graph:", err)
		return
	}
	defer graphJson.Close()

	fileContents, err := io.ReadAll(graphJson)
	if err != nil {
		fmt.Println("Error reading file:", graphFile, err)
		return
	}

	var graphData SimNetwork
	err = json.Unmarshal(fileContents, &graphData)
	if err != nil {
		fmt.Println("Error decoding JSON:", graphFile, err)
		return
	}

	edges, err := getNetworkEdges(graphData)
	if err != nil {
		fmt.Println("Error getting edge data: ", err)
		os.Exit(1)
	}

	networkData, err := getNetworkData(network, edges)
	if err != nil {
		fmt.Println("Error getting netowrk data: ", err)
		os.Exit(1)
	}

	err = writeNetworkData(os.Args[1], networkData)
	if err != nil {
		fmt.Println("Write network data: ", err)
		os.Exit(1)
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

type networkReputation struct {
	node           string
	chanIn         uint64
	chanOut        uint64
	incomingRepAmt float64
	incomingRevAmt float64
	outgoingRepAmt float64
	outgoingRevAmt float64
}

func (n networkReputation) incomingReputation() bool {
	return n.incomingRepAmt > n.incomingRevAmt
}

func (n networkReputation) outgoingReputation() bool {
	return n.outgoingRepAmt > n.outgoingRevAmt
}

func (n networkReputation) goodReputation() bool {
	return n.incomingReputation() && n.outgoingReputation()
}

func getNetworkData(data map[string]*lrc.ChannelBootstrap,
	graph map[string]map[uint64]struct{}) ([]networkReputation, error) {

	var (
		records                  []networkReputation
		goodRepPairs, totalPairs int
	)

	for alias, channels := range data {
		var (
			goodReputation int
			pairs          int
		)

		allChannels, ok := graph[alias]
		if !ok {
			return nil, fmt.Errorf("Node: %v not found in graph",
				alias)
		}

		// Add any channels for the node that have no bootstrapped
		// activity, and thus no reputation or revenue.
		for scid, _ := range allChannels {
			scid := lnwire.NewShortChanIDFromInt(scid)
			_, ok := channels.Incoming[scid]
			if !ok {
				channels.Incoming[scid] = nil
			}

			_, ok = channels.Outgoing[scid]
			if !ok {
				channels.Outgoing[scid] = nil
			}
		}

		// TODO: all of these value will be at different timestamps
		// because the decaying average was last updated at different
		// times. Even if we update this, our clock will always be
		// different, so we'll end up with different values per-pair.
		for chanIn, reputation := range channels.Incoming {
			// Allow nil entires for the case where we have no
			// reputation tracked.
			incomingReputation := 0.0
			if reputation != nil {
				incomingReputation = reputation.DebugValue()
			}
			incomingReputation = (100 + incomingReputation) * 100

			for chanOut, revenue := range channels.Outgoing {
				if chanOut == chanIn {
					continue
				}

				incomingRevenue := 0.0
				if revenue != nil {
					incomingRevenue = revenue.DebugValue()
				}

				// Get the reputation of the outgoing channel.
				outgoingReputation := 0.0
				outRepAvg, _ := channels.Incoming[chanOut]
				if outRepAvg != nil {
					outgoingReputation = outRepAvg.DebugValue()
				}

				outgoingRevenue := 0.0
				inRevAvg, _ := channels.Outgoing[chanIn]
				if inRevAvg != nil {
					outgoingRevenue = inRevAvg.DebugValue()
				}

				outgoingReputation = (100 + outgoingReputation) * 100
				record := networkReputation{
					node:           alias,
					chanIn:         chanIn.ToUint64(),
					chanOut:        chanOut.ToUint64(),
					incomingRepAmt: incomingReputation,
					incomingRevAmt: incomingRevenue,
					outgoingRepAmt: outgoingReputation,
					outgoingRevAmt: outgoingRevenue,
				}

				pairs++
				if record.goodReputation() {
					goodReputation++
				}
				records = append(records, record)
			}
		}

		goodRepPairs += goodReputation
		totalPairs += pairs
		fmt.Printf("Node: %v has %v/%v good reputation pairs (%v%%)\n",
			alias, goodReputation, pairs, (goodReputation * 100 / pairs))
	}

	fmt.Printf("Total pairs: %v, with good reputation: %v (%v %%)\n",
		totalPairs, goodRepPairs, (goodRepPairs * 100 / totalPairs))

	return records, nil
}

func writeNetworkData(path string, records []networkReputation) error {
	baseName := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	filePath := baseName + "_reputations.csv"

	// Create and open the CSV file
	file, err := os.Create(filePath)
	if err != nil {
		return fmt.Errorf("failed to create file: %v", err)
	}
	defer file.Close()

	writer := csv.NewWriter(file)
	defer writer.Flush()

	// Write the header row
	header := []string{"node", "chan_in", "chan_out", "reputation_in", "revenue_in", "reputation_out", "revenue_out", "good_rep"}
	if err := writer.Write(header); err != nil {
		return fmt.Errorf("failed to write header: %v", err)
	}
	for _, record := range records {
		row := []string{
			record.node,
			strconv.FormatUint(record.chanIn, 10),
			strconv.FormatUint(record.chanOut, 10),
			fmt.Sprintf("%f", record.incomingRepAmt),
			fmt.Sprintf("%f", record.incomingRevAmt),
			fmt.Sprintf("%f", record.outgoingRepAmt),
			fmt.Sprintf("%f", record.outgoingRevAmt),
			strconv.FormatBool(record.goodReputation()),
		}
		if err := writer.Write(row); err != nil {
			return fmt.Errorf("failed to write record: %v", err)
		}
	}

	return nil
}

type Edge struct {
	ChannelID uint64 `json:"channel_id"`
	Node1     Node   `json:"node_1"`
	Node2     Node   `json:"node_2"`
}

type Node struct {
	Alias string `json:"alias"`
}

type SimNetwork struct {
	Edges []Edge `json:"sim_network"`
}

func getNetworkEdges(graphData SimNetwork) (map[string]map[uint64]struct{},
	error) {

	network := make(map[string]map[uint64]struct{})
	addEdge := func(alias string, channelID uint64) error {
		channels, ok := network[alias]
		if !ok {
			channels = make(map[uint64]struct{})
			network[alias] = channels
		}

		channels[channelID] = struct{}{}
		return nil
	}

	for _, edge := range graphData.Edges {
		if err := addEdge(edge.Node1.Alias, edge.ChannelID); err != nil {
			return nil, err
		}

		if err := addEdge(edge.Node2.Alias, edge.ChannelID); err != nil {
			return nil, err
		}
	}

	return network, nil
}
