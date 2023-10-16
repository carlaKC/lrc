package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/carlakc/lrc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	migrate "github.com/rubenv/sql-migrate"
	_ "modernc.org/sqlite"
)

var migrations = &migrate.MemoryMigrationSource{
	Migrations: []*migrate.Migration{
		{
			Id: "1",
			Up: []string{
				`
				CREATE TABLE IF NOT EXISTS limits (
					peer TEXT PRIMARY KEY NOT NULL,
					htlc_max_pending INTEGER NOT NULL,
					htlc_max_hourly_rate INTEGER NOT NULL,
					mode TEXT CHECK(mode IN ('FAIL', 'QUEUE', 'QUEUE_PEER_INITIATED')) NOT NULL DEFAULT 'FAIL'
				);
				
				INSERT OR IGNORE INTO limits(peer, htlc_max_pending, htlc_max_hourly_rate) 
				VALUES('000000000000000000000000000000000000000000000000000000000000000000', 5, 3600);
				`,
			},
		},
		{
			Id: "2",
			Up: []string{
				`
				ALTER TABLE limits RENAME TO limits_old;

				CREATE TABLE IF NOT EXISTS limits (
					peer TEXT PRIMARY KEY NOT NULL,
					htlc_max_pending INTEGER NOT NULL,
					htlc_max_hourly_rate INTEGER NOT NULL,
					mode TEXT CHECK(mode IN ('FAIL', 'QUEUE', 'QUEUE_PEER_INITIATED', 'BLOCK')) NOT NULL DEFAULT 'FAIL'
				);

				INSERT INTO limits(peer, htlc_max_pending, htlc_max_hourly_rate, mode)
					SELECT peer, htlc_max_pending, htlc_max_hourly_rate, mode FROM limits_old;

				DROP TABLE limits_old;
				`,
			},
		},
		{
			Id: "3",
			Up: []string{
				`CREATE TABLE IF NOT EXISTS forwarding_history (
                                        add_time TIMESTAMP NOT NULL,
                                        resolved_time TIMESTAMP NOT NULL,
                                        settled BOOLEAN NOT NULL,
                                        incoming_amt_msat INTEGER NOT NULL CHECK (incoming_amt_msat > 0),
                                        outgoing_amt_msat INTEGER NOT NULL CHECK (outgoing_amt_msat > 0),
                                        incoming_peer TEXT NOT NULL,
                                        incoming_channel INTEGER NOT NULL,
                                        incoming_htlc_index INTEGER NOT NULL,
                                        outgoing_peer TEXT NOT NULL,
                                        outgoing_channel INTEGER NOT NULL,
                                        outgoing_htlc_index INTEGER NOT NULL,
                                        incoming_endorsed INTEGER NOT NULL,
                                        outgoing_endorsed INTEGER NOT NULL,
                                       
                                        CONSTRAINT unique_incoming_circuit UNIQUE (incoming_channel, incoming_htlc_index),
                                        CONSTRAINT unique_outgoing_circuit UNIQUE (outgoing_channel, outgoing_htlc_index)
                                );`,
				`CREATE INDEX add_time_index ON forwarding_history (add_time);`,
			},
		},
		{
			Id: "4",
			Up: []string{
				`CREATE TABLE IF NOT EXISTS rejected_htlcs (
                                        id INTEGER PRIMARY KEY NOT NULL,
                                        reject_time_ns TIMESTAMP NOT NULL,
                                        incoming_channel INTEGER NOT NULL,
                                        incoming_index INTEGER NOT NULL,
                                        outgoing_channel INTEGER NOT NULL,
                                        incoming_msat INTEGER NOT NULL,
                                        outgoing_msat INTEGER NOT NULL,
                                        cltv_delta INTEGER NOT NULL,
                                        incoming_endorsed BOOLEAN NOT NULL
                                );`,
			},
		},
	},
}

const (
	// defaultFwdHistoryLimit is the default limit we place on the forwarding_history table
	// to prevent creation of an ever-growing table.
	//
	// Justification for value:
	// * ~100 bytes per row in the table.
	// * Help ourselves to 10MB of disk space
	// -> 100_000 entries
	defaultFwdHistoryLimit = 100_000
)

var defaultNodeKey = route.Vertex{}

type Db struct {
	db *sql.DB

	fwdHistoryLimit int
}

func NewDb(dbPath string, opts ...func(*Db)) (*Db, error) {
	const busyTimeoutMs = 5000

	dsn := dbPath + fmt.Sprintf("?_pragma=busy_timeout=%d", busyTimeoutMs)

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}

	n, err := migrate.Exec(db, "sqlite3", migrations, migrate.Up)
	if err != nil {
		return nil, fmt.Errorf("migration error: %w", err)
	}
	if n > 0 {
		//log.Infow("Applied migrations", "count", n)
	}

	database := &Db{
		db:              db,
		fwdHistoryLimit: defaultFwdHistoryLimit,
	}
	for _, opt := range opts {
		opt(database)
	}

	return database, nil
}

func (d *Db) Close() error {
	return d.db.Close()
}

type CircuitKey struct {
	channel uint64
	htlc    uint64
}

type HtlcInfo struct {
	AddTime          time.Time
	ResolveTime      time.Time
	Settled          bool
	IncomingMsat     lnwire.MilliSatoshi
	OutgoingMsat     lnwire.MilliSatoshi
	IncomingPeer     route.Vertex
	OutgoingPeer     route.Vertex
	IncomingCircuit  CircuitKey
	OutgoingCircuit  CircuitKey
	IncomingEndorsed lrc.Endorsement
	OutgoingEndorsed lrc.Endorsement
}

func serializeEndorsement(endorsed lrc.Endorsement) (int, error) {
	switch endorsed {
	case lrc.EndorsementNone:
		return -1, nil

	case lrc.EndorsementFalse:
		return 0, nil

	case lrc.EndorsementTrue:
		return 1, nil

	default:
		return 0, fmt.Errorf("unknown endorsement: %v", endorsed)
	}
}

func deserializeEndorsement(endorsed int) lrc.Endorsement {
	switch endorsed {
	case 0:
		return lrc.EndorsementFalse
	case 1:
		return lrc.EndorsementTrue
	default:
		return lrc.EndorsementNone
	}
}

// ListForwardingHistory returns a list of htlcs that were resolved within the
// time range provided (start time is inclusive, end time is exclusive)
func (d *Db) ListForwardingHistory(ctx context.Context, start, end time.Time) (
	[]*HtlcInfo, error) {

	list := `SELECT 
                add_time,
                resolved_time,
                settled,
                incoming_amt_msat,
                outgoing_amt_msat,
                incoming_peer,
                incoming_channel,
                incoming_htlc_index,
                outgoing_peer,
                outgoing_channel,
                outgoing_htlc_index,
                incoming_endorsed,
                outgoing_endorsed
                FROM forwarding_history
                WHERE add_time >= ? AND add_time < ?;`

	rows, err := d.db.QueryContext(ctx, list, start.UnixNano(), end.UnixNano())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var htlcs []*HtlcInfo
	for rows.Next() {
		var (
			incomingPeer, outgoingPeer         string
			addTime, resolveTime               uint64
			incomingEndorsed, outgoingEndorsed int
			htlc                               HtlcInfo
		)

		err := rows.Scan(
			&addTime,
			&resolveTime,
			&htlc.Settled,
			&htlc.IncomingMsat,
			&htlc.OutgoingMsat,
			&incomingPeer,
			&htlc.IncomingCircuit.channel,
			&htlc.IncomingCircuit.htlc,
			&outgoingPeer,
			&htlc.OutgoingCircuit.channel,
			&htlc.OutgoingCircuit.htlc,
			&incomingEndorsed,
			&outgoingEndorsed,
		)
		if err != nil {
			return nil, err
		}
		htlc.AddTime = time.Unix(0, int64(addTime))
		htlc.ResolveTime = time.Unix(0, int64(resolveTime))

		htlc.IncomingPeer, err = route.NewVertexFromStr(incomingPeer)
		if err != nil {
			return nil, err
		}

		htlc.OutgoingPeer, err = route.NewVertexFromStr(outgoingPeer)
		if err != nil {
			return nil, err
		}

		htlc.IncomingEndorsed = deserializeEndorsement(incomingEndorsed)
		htlc.OutgoingEndorsed = deserializeEndorsement(outgoingEndorsed)

		htlcs = append(htlcs, &htlc)
	}

	return htlcs, nil
}

type RejectedHTLC struct {
	RejectTime       time.Time
	IncomingCircuit  CircuitKey
	OutgoingChannel  uint64
	IncomingAmount   lnwire.MilliSatoshi
	OutgoingAmount   lnwire.MilliSatoshi
	CltvDelta        uint32
	IncomingEndorsed lrc.Endorsement
}

func (d *Db) ListRejectedHTLCs(ctx context.Context, start, end time.Time) (
	[]*RejectedHTLC, error) {

	list := `SELECT
        reject_time_ns,
        incoming_channel,
        incoming_index,
        outgoing_channel,
        incoming_msat,
        outgoing_msat,
        cltv_delta,
        incoming_endorsed
        FROM rejected_htlcs
        WHERE reject_time_ns >= ? AND reject_time_ns < ?;`

	rows, err := d.db.QueryContext(ctx, list, start.UnixNano(), end.UnixNano())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var htlcs []*RejectedHTLC
	for rows.Next() {
		var (
			rejectTime uint64
			endorsed   int
			htlc       = &RejectedHTLC{}
		)

		err := rows.Scan(
			&rejectTime,
			&htlc.IncomingCircuit.channel,
			&htlc.IncomingCircuit.htlc,
			&htlc.OutgoingChannel,
			&htlc.IncomingAmount,
			&htlc.OutgoingAmount,
			&htlc.CltvDelta,
			&endorsed,
		)
		if err != nil {
			return nil, err
		}

		htlc.RejectTime = time.Unix(0, int64(rejectTime))
		htlc.IncomingEndorsed = deserializeEndorsement(endorsed)
		htlcs = append(htlcs, htlc)
	}

	return htlcs, nil
}
