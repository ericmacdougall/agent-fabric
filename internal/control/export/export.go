package export

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// MaxOutput caps one call's output, the cursor line included.
const MaxOutput = 1 << 20

// cursorReserve is room kept for the final cursor line.
const cursorReserve = 768 // the cursor line plus a possible lag event

const pageSize = 500

// ErrBadCursor is a cursor this store refuses: malformed, from another
// database, or naming an outbox row that does not exist (the store was
// restored or rebuilt). The caller must not resume from it.
var ErrBadCursor = errors.New("cursor refused")

// DB is the one capability the exporter needs: begin a transaction.
type DB interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// Result is what one call produced.
type Result struct {
	Events int
	Cursor string
	More   bool // the cap stopped the page; call again with Cursor
	// Lagging counts outbox rows past the cursor that exist but are held back by an
	// open earlier transaction. Above zero, "no new events" does not mean caught up.
	Lagging int64
}

// Cursor is "fx1." + base64url("<dbid>:<outbox id>") + "." + 8 hex of a digest,
// so a typo or a cursor from another database is refused rather than guessed.
func encodeCursor(dbid string, id int64, rowhash string, backfill bool) string {
	mode := "l"
	if backfill {
		mode = "b"
	}
	body := dbid + ":" + strconv.FormatInt(id, 10) + ":" + rowhash + ":" + mode
	sum := sha256.Sum256([]byte("fx1|" + body))
	return "fx1." + base64.RawURLEncoding.EncodeToString([]byte(body)) + "." + hex.EncodeToString(sum[:4])
}

func decodeCursor(c string) (dbid string, id int64, rowhash string, backfill bool, err error) {
	parts := strings.Split(c, ".")
	if len(parts) != 3 || parts[0] != "fx1" {
		return "", 0, "", false, fmt.Errorf("%w: not an export cursor", ErrBadCursor)
	}
	raw, e := base64.RawURLEncoding.DecodeString(parts[1])
	if e != nil {
		return "", 0, "", false, fmt.Errorf("%w: malformed", ErrBadCursor)
	}
	sum := sha256.Sum256([]byte("fx1|" + string(raw)))
	if hex.EncodeToString(sum[:4]) != parts[2] {
		return "", 0, "", false, fmt.Errorf("%w: checksum mismatch", ErrBadCursor)
	}
	f := strings.Split(string(raw), ":")
	if len(f) != 4 || f[0] == "" || f[2] == "" || (f[3] != "b" && f[3] != "l") {
		return "", 0, "", false, fmt.Errorf("%w: malformed", ErrBadCursor)
	}
	id, e = strconv.ParseInt(f[1], 10, 64)
	if e != nil || id < 0 {
		return "", 0, "", false, fmt.Errorf("%w: malformed", ErrBadCursor)
	}
	return f[0], id, f[2], f[3] == "b", nil
}

// rowHashSQL binds a cursor to the content of the outbox row it names (mission,
// seq and insert time), so a different store with the same database name and
// oid but other rows refuses the cursor instead of silently resuming.
const rowHashSQL = `substr(encode(sha256(convert_to(o.mission_id || '|' || o.seq::text || '|' ||
	to_char(o.at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US'), 'UTF8')), 'hex'), 1, 12)`

// Export writes the events after the cursor (all of them when since is empty)
// to w as mc.event/1 lines followed by one {"cursor":"..."} line, in one
// read-only repeatable-read transaction. The output is buffered and written
// once at the end, so an error leaves w untouched and a line is never cut.
func Export(ctx context.Context, db DB, since string, w io.Writer) (Result, error) {
	tx, err := db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Result{}, err
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck // read-only: nothing to commit
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = '30s'"); err != nil {
		return Result{}, err
	}
	// The identity of this database: a recreated or restored one has another oid.
	var dbid string
	if err := tx.QueryRow(ctx, `SELECT substr(encode(sha256(convert_to(datname || '|' || oid::text, 'UTF8')), 'hex'), 1, 16)
		FROM pg_database WHERE datname = current_database()`).Scan(&dbid); err != nil {
		return Result{}, fmt.Errorf("database identity: %w", err)
	}
	var after int64
	lastHash := "0"
	// A backfill (no cursor, or a cursor chain that began without one and has not
	// yet caught up) is marked historic so Mission Control never pulses old events.
	backfill := true
	if since != "" {
		d, id, h, bf, err := decodeCursor(since)
		if err != nil {
			return Result{}, err
		}
		if d != dbid {
			return Result{}, fmt.Errorf("%w: it belongs to another database", ErrBadCursor)
		}
		if id > 0 {
			var have string
			if err := tx.QueryRow(ctx, `SELECT `+rowHashSQL+` FROM outbox o WHERE o.id = $1`, id).Scan(&have); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return Result{}, fmt.Errorf("%w: it names an outbox row this store does not have (stale)", ErrBadCursor)
				}
				return Result{}, err
			}
			if have != h {
				return Result{}, fmt.Errorf("%w: the outbox row it names has different content (another or rebuilt store)", ErrBadCursor)
			}
		} else if h != "0" {
			return Result{}, fmt.Errorf("%w: malformed", ErrBadCursor)
		}
		after, lastHash, backfill = id, h, bf
	}

	var buf bytes.Buffer
	res := Result{}
	last := after
page:
	for {
		// xid filter (goodqa#15577): only rows whose transaction is older than
		// every in-progress one, so a row that commits late with a lower id is
		// never passed over by the cursor. Execute assigns ids in xid order.
		rows, err := tx.Query(ctx, `SELECT o.id, o.mission_id, o.seq, COALESCE(e.kind, ''), COALESCE(e.at, o.at),
				COALESCE(h.last_id IS NOT NULL AND o.id > h.last_id, false), COALESCE(m.ticket, ''), `+rowHashSQL+`
			FROM outbox o
			LEFT JOIN events e ON e.mission_id = o.mission_id AND e.seq = o.seq
			LEFT JOIN outbox_cursors h ON h.consumer = 'sched.hold.' || o.mission_id
			LEFT JOIN missions m ON m.id = o.mission_id
			WHERE o.id > $1 AND o.xid < pg_snapshot_xmin(pg_current_snapshot())
			ORDER BY o.id LIMIT $2`, last, pageSize)
		if err != nil {
			return Result{}, err
		}
		var page []Row
		var hashes []string
		for rows.Next() {
			var r Row
			var h string
			if err := rows.Scan(&r.ID, &r.Mission, &r.Seq, &r.Kind, &r.At, &r.Held, &r.Ticket, &h); err != nil {
				rows.Close()
				return Result{}, err
			}
			page = append(page, r)
			hashes = append(hashes, h)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return Result{}, err
		}
		for i, r := range page {
			ev := MapRow(r)
			ev.Historic = backfill
			line, err := json.Marshal(ev)
			if err != nil {
				return Result{}, err
			}
			if buf.Len()+len(line)+1+cursorReserve > MaxOutput {
				res.More = true
				break page
			}
			buf.Write(line)
			buf.WriteByte('\n')
			res.Events++
			last, lastHash = r.ID, hashes[i]
		}
		if len(page) < pageSize {
			break
		}
	}
	if !res.More {
		// Rows committed but held back by an older open transaction (anywhere in the
		// cluster): without this, a stall looks exactly like being caught up.
		var firstHeld int64
		var now time.Time
		if err := tx.QueryRow(ctx, `SELECT count(*), COALESCE(min(o.id), 0), now() FROM outbox o WHERE o.id > $1 AND o.xid >= pg_snapshot_xmin(pg_current_snapshot())`, last).Scan(&res.Lagging, &firstHeld, &now); err != nil {
			return Result{}, err
		}
		if res.Lagging > 0 {
			// A file-drop reader never sees stderr: say it in the stream too, once per episode.
			line, err := json.Marshal(MapLag(res.Lagging, firstHeld, now))
			if err != nil {
				return Result{}, err
			}
			buf.Write(line)
			buf.WriteByte('\n')
		}
	}
	res.Cursor = encodeCursor(dbid, last, lastHash, res.More && backfill)
	line, err := json.Marshal(struct {
		Cursor string `json:"cursor"`
	}{res.Cursor})
	if err != nil {
		return Result{}, err
	}
	buf.Write(line)
	buf.WriteByte('\n')
	if _, err := w.Write(buf.Bytes()); err != nil {
		return Result{}, err
	}
	return res, nil
}
