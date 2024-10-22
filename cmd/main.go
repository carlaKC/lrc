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

	network, err := processForwards(
		fwds, clock.NewDefaultClock(),
		lrc.ManagerParams{
			RevenueWindow:        time.Hour * 24 * 14,
			ReputationMultiplier: 24,
			ProtectedPercentage:  50,
			ResolutionPeriod:     time.Second * 90,
			BlockTime:            5,
		},
	)
	if err != nil {
		fmt.Println("Could not bootstrap network ", err)
		os.Exit(1)
	}

	// Convert parsed nodes to channel history struct so we can get values
	// easily.
	channelHistories := make(map[string]map[lnwire.ShortChannelID]*lrc.ChannelHistory)
	for nodeID, channels := range network {
		newChannels := make(map[lnwire.ShortChannelID]*lrc.ChannelHistory)

		for channelID, channelBootstrap := range channels {
			newChannels[channelID] = channelBootstrap.ToChannelHistory()
		}

		channelHistories[nodeID] = newChannels
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

	edges := getNetworkEdges(graphData)

	networkData, err := getNetworkData(channelHistories, edges)
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

type NetworkForward struct {
	NodeAlias string
	*lrc.ForwardedHTLC
}

// incoming_amt,incoming_expiry,incoming_add_ts,incoming_remove_ts,outgoing_amt,outgoing_expiry,outgoing_add_ts,outgoing_remove_ts,forwarding_node,forwarding_alias,chan_in,chan_out
func readCSV(reader *csv.Reader) ([]*NetworkForward, error) {
	// Read in all the CSV records
	records, err := reader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("Error reading CSV: %v", err)
	}

	// Slice to store parsed data
	var forwardedHTLCs []*NetworkForward

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

		networkHtlc := NetworkForward{
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

func processForwards(forwards []*NetworkForward, clock clock.Clock,
	params lrc.ManagerParams) (
	map[string]map[lnwire.ShortChannelID]*lrc.ChannelBoostrap, error) {

	nodes := make(map[string]map[lnwire.ShortChannelID]*lrc.ChannelBoostrap)

	for _, h := range forwards {
		channels, ok := nodes[h.NodeAlias]
		if !ok {
			channels = make(
				map[lnwire.ShortChannelID]*lrc.ChannelBoostrap,
			)
		}

		incoming, ok := channels[h.IncomingChannel]
		if !ok {
			incoming = &lrc.ChannelBoostrap{
				Scid: h.IncomingChannel,
			}
		}

		outgoing, ok := channels[h.OutgoingChannel]
		if !ok {
			outgoing = &lrc.ChannelBoostrap{
				Scid: h.OutgoingChannel,
			}
		}

		err := incoming.AddHTLC(clock, h.ForwardedHTLC, params)
		if err != nil {
			return nil, err
		}

		err = outgoing.AddHTLC(clock, h.ForwardedHTLC, params)
		if err != nil {
			return nil, err
		}

		channels[h.IncomingChannel] = incoming
		channels[h.OutgoingChannel] = outgoing
		nodes[h.NodeAlias] = channels
	}

	return nodes, nil
}

type networkReputation struct {
	node    string
	chanIn  uint64
	chanOut uint64

	incomingRepAmt  float64
	incomingRepTsNs uint64

	incomingRevAmt  float64
	incomingRevTsNs uint64

	outgoingRepAmt  float64
	outgoingRepTsNs uint64

	outgoingRevAmt  float64
	outgoingRevTsNs uint64
}

func (n networkReputation) incomingReputation() bool {
	return n.incomingRepAmt > n.outgoingRevAmt
}

func (n networkReputation) outgoingReputation() bool {
	return n.outgoingRepAmt > n.incomingRevAmt
}

func (n networkReputation) bidiReputation() bool {
	return n.outgoingReputation() && n.incomingReputation()
}
func getNetworkData(bootstrap map[string]map[lnwire.ShortChannelID]*lrc.ChannelHistory,
	graph map[string][]lnwire.ShortChannelID) ([]networkReputation, error) {

	var (
		records                  []networkReputation
		goodRepPairs, totalPairs int
	)

	// For each node, run through each channel and get its pairwise rep.
	for node, channels := range graph {
		// Count values for individual pair, just for logging.
		var (
			goodReputation int
			pairs          int
		)

		nodeBootstrap, ok := bootstrap[node]
		if !ok {
			// If there's no history for the node, we just make an
			// empty map, which will look like we just don't have
			// any data for our channels.
			nodeBootstrap = make(map[lnwire.ShortChannelID]*lrc.ChannelHistory)
		}

		// For each channel, we need to generate every possible pair.
		// This requires picking each channel as an incoming candidate
		// and then comparing it to every other channel.
		for _, incomingChannel := range channels {
			incomingReputation := 0.0
			var incomingReputationTs uint64 = 0

			incomingRevenueBidi := 0.0
			var incomingRevTs uint64 = 0

			incomingBoostrap, ok := nodeBootstrap[incomingChannel]
			if ok {
				if incomingBoostrap.IncomingReputation != nil {
					incomingReputation = incomingBoostrap.IncomingReputation.Value
					incomingReputationTs = uint64(
						incomingBoostrap.IncomingReputation.LastUpdate.UnixNano(),
					)
				}

				if incomingBoostrap.Revenue != nil {
					incomingRevenueBidi = incomingBoostrap.Revenue.Value
					incomingRevTs = uint64(
						incomingBoostrap.Revenue.LastUpdate.UnixNano(),
					)
				}
			}

			for _, outgoingChannel := range channels {
				if outgoingChannel == incomingChannel {
					continue
				}

				record := networkReputation{
					node:            node,
					chanIn:          incomingChannel.ToUint64(),
					chanOut:         outgoingChannel.ToUint64(),
					incomingRepAmt:  incomingReputation,
					incomingRepTsNs: incomingReputationTs,
					incomingRevAmt:  incomingRevenueBidi,
					incomingRevTsNs: incomingRevTs,
				}

				outgoingBootstrap, ok := nodeBootstrap[outgoingChannel]
				if ok {
					if outgoingBootstrap.OutgoingReputation != nil {
						record.outgoingRepAmt = outgoingBootstrap.OutgoingReputation.Value
						record.outgoingRepTsNs = uint64(
							outgoingBootstrap.OutgoingReputation.LastUpdate.UnixNano(),
						)
					}

					if outgoingBootstrap.Revenue != nil {
						record.outgoingRevAmt = outgoingBootstrap.Revenue.Value
						record.outgoingRevTsNs = uint64(
							outgoingBootstrap.Revenue.LastUpdate.UnixNano(),
						)
					}
				}

				pairs++
				// Right now, we're concerned with outgoing rep.
				if record.outgoingReputation() {
					goodReputation++
				}
				records = append(records, record)
			}
		}

		// If a node has a single channel, we don't worry about it.
		if pairs == 0 {
			continue
		}

		goodRepPairs += goodReputation
		totalPairs += pairs
		fmt.Printf("Node: %v has %v/%v good reputation pairs (%v%%)\n",
			node, goodReputation, pairs, (goodReputation * 100 / pairs))

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
	header := []string{"node", "chan_in", "chan_out", "reputation_in", "revenue_in", "reputation_out", "revenue_out", "reputation_in_ns", "revenue_in_ns", "reputation_out_ns", "revenue_out_ns", "incoming_rep", "outgoing_rep", "bidi_rep"}
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
			strconv.FormatUint(record.incomingRepTsNs, 10),
			strconv.FormatUint(record.incomingRevTsNs, 10),
			strconv.FormatUint(record.outgoingRepTsNs, 10),
			strconv.FormatUint(record.outgoingRevTsNs, 10),
			strconv.FormatBool(record.incomingReputation()),
			strconv.FormatBool(record.outgoingReputation()),
			strconv.FormatBool(record.bidiReputation()),
		}
		if err := writer.Write(row); err != nil {
			return fmt.Errorf("failed to write record: %v", err)
		}
	}

	return nil
}

type Edge struct {
	ChannelID uint64 `json:"scid"`
	Node1     Node   `json:"node_1"`
	Node2     Node   `json:"node_2"`
}

type Node struct {
	Alias string `json:"alias"`
}

type SimNetwork struct {
	Edges []Edge `json:"sim_network"`
}

func getNetworkEdges(graphData SimNetwork) map[string][]lnwire.ShortChannelID {
	network := make(map[string][]lnwire.ShortChannelID)

	for _, edge := range graphData.Edges {
		network[edge.Node1.Alias] = append(
			network[edge.Node1.Alias],
			lnwire.NewShortChanIDFromInt(edge.ChannelID),
		)

		network[edge.Node2.Alias] = append(
			network[edge.Node2.Alias],
			lnwire.NewShortChanIDFromInt(edge.ChannelID),
		)
	}

	return network
}
