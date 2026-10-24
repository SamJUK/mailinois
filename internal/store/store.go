// Package store keeps messages, recipients and their events in SQLite or PostgreSQL, correlating log lines
// by queue ID.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"hash/fnv"
	"io/fs"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"

	"github.com/SamJUK/mailinois/internal/postfix"
)

//go:embed schema/*.sql
var schema embed.FS

type Store struct {
	db *sql.DB
	pg bool

	stmts sync.Map // query text to *sql.Stmt, for statements run inside transactions

	// write queues SQLite's writers in turn. SQLite has no queue of its own: a waiting writer polls, and a
	// busy ingest took the lock back each time it let go, so at 2,000,000 messages the hourly purge waited
	// out its busy timeout and failed (BenchmarkPurge). PostgreSQL queues writers itself.
	write sync.Mutex
}

// writing holds SQLite's turn to write until the returned func is called.
func (s *Store) writing() func() {
	if s.pg {
		return func() {}
	}
	s.write.Lock()
	return s.write.Unlock
}

// querier is a *sql.DB or a *sql.Tx.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Open takes sqlite:<path> or a postgres:// URL.
func Open(ctx context.Context, dsn string) (*Store, error) {
	s := &Store{}
	var err error
	switch {
	case strings.HasPrefix(dsn, "postgres://"), strings.HasPrefix(dsn, "postgresql://"):
		s.pg = true
		s.db, err = sql.Open("pgx", dsn)
	case strings.HasPrefix(dsn, "sqlite:"):
		path := strings.TrimPrefix(strings.TrimPrefix(dsn, "sqlite:"), "//")
		pragmas := url.Values{"_pragma": {"busy_timeout(10000)", "journal_mode(WAL)", "foreign_keys(1)", "synchronous(NORMAL)"}, "_txlock": {"immediate"}}
		s.db, err = sql.Open("sqlite", "file:"+path+"?"+pragmas.Encode())
	default:
		return nil, fmt.Errorf("database url %q: want sqlite:<path> or postgres://", dsn)
	}
	if err != nil {
		return nil, err
	}
	if err := s.migrate(ctx); err != nil {
		s.db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func (s *Store) Close() {
	s.stmts.Range(func(_, v any) bool { v.(*sql.Stmt).Close(); return true })
	s.db.Close()
}

// Optimize refreshes SQLite's statistics where tables have grown or shrunk enough to need it. Without them its
// planner guesses that a server or site narrows mail down, and reads a filtered dashboard message by message;
// with them a 30-day dashboard for one server took half as long (bench_test.go). PostgreSQL's autovacuum
// keeps its own statistics.
func (s *Store) Optimize(ctx context.Context) error {
	if s.pg {
		return nil
	}
	defer s.writing()()
	_, err := s.db.ExecContext(ctx, "PRAGMA optimize=0x10002")
	return err
}

func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *Store) Dialect() string {
	if s.pg {
		return "postgres"
	}
	return "sqlite"
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY, applied_at BIGINT NOT NULL)`); err != nil {
		return err
	}
	names, err := fs.Glob(schema, "schema/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	types := strings.NewReplacer("{{id}}", "INTEGER PRIMARY KEY", "{{blob}}", "BLOB")
	if s.pg {
		types = strings.NewReplacer("{{id}}", "BIGSERIAL PRIMARY KEY", "{{blob}}", "BYTEA")
	}
	for _, name := range names {
		var done int
		if err := s.row(ctx, s.db, `SELECT count(*) FROM schema_migrations WHERE name = ?`, name).Scan(&done); err != nil {
			return err
		}
		if done > 0 {
			continue
		}
		body, _ := schema.ReadFile(name)
		err := s.tx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, types.Replace(string(body))); err != nil {
				return err
			}
			return s.exec(ctx, tx, `INSERT INTO schema_migrations (name, applied_at) VALUES (?, ?)`, name, us(time.Now()))
		})
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	defer s.writing()()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

var placeholder = regexp.MustCompile(`\?`)

// bind turns ? placeholders into $1, $2... for PostgreSQL. No query here has a literal question mark.
func (s *Store) bind(q string) string {
	if !s.pg {
		return q
	}
	n := 0
	return placeholder.ReplaceAllStringFunc(q, func(string) string { n++; return "$" + strconv.Itoa(n) })
}

func (s *Store) exec(ctx context.Context, db querier, q string, args ...any) error {
	if db == querier(s.db) { // a write on its own; inside a transaction the turn is already held
		defer s.writing()()
	}
	_, err := s.on(ctx, db, q).ExecContext(ctx, s.bind(q), args...)
	return err
}

func (s *Store) row(ctx context.Context, db querier, q string, args ...any) *sql.Row {
	return s.on(ctx, db, q).QueryRowContext(ctx, s.bind(q), args...)
}

func (s *Store) rows(ctx context.Context, db querier, q string, args ...any) (*sql.Rows, error) {
	return s.on(ctx, db, q).QueryContext(ctx, s.bind(q), args...)
}

// on runs a statement inside a transaction as a prepared one, prepared once per connection and kept: parsing
// the SQL again for every line was most of the time ingest took (bench_test.go). Their text never changes, so
// the set is small. Queries on the DB itself are built from searches, and stay unprepared.
func (s *Store) on(ctx context.Context, db querier, q string) querier {
	tx, ok := db.(*sql.Tx)
	if !ok {
		return db
	}
	stmt, ok := s.stmts.Load(q)
	if !ok {
		prepared, err := s.db.PrepareContext(ctx, s.bind(q))
		if err != nil {
			return db // the plain call reports the same error
		}
		if stmt, ok = s.stmts.LoadOrStore(q, prepared); ok {
			prepared.Close()
		}
	}
	return stmtQuerier{tx.StmtContext(ctx, stmt.(*sql.Stmt))}
}

// stmtQuerier lets a transaction's prepared statement stand in for the transaction; the query text it is
// given again is the same one it was prepared from.
type stmtQuerier struct{ *sql.Stmt }

func (q stmtQuerier) ExecContext(ctx context.Context, _ string, args ...any) (sql.Result, error) {
	return q.Stmt.ExecContext(ctx, args...)
}

func (q stmtQuerier) QueryContext(ctx context.Context, _ string, args ...any) (*sql.Rows, error) {
	return q.Stmt.QueryContext(ctx, args...)
}

func (q stmtQuerier) QueryRowContext(ctx context.Context, _ string, args ...any) *sql.Row {
	return q.Stmt.QueryRowContext(ctx, args...)
}

// us is a time as stored: microseconds since 1970.
func us(t time.Time) int64 { return t.UnixMicro() }

func fromUS(v int64) time.Time { return time.UnixMicro(v) }

func fromUSp(v *int64) *time.Time {
	if v == nil {
		return nil
	}
	t := fromUS(*v)
	return &t
}

func lineHash(raw string) int64 {
	h := fnv.New64a()
	h.Write([]byte(raw))
	return int64(h.Sum64())
}

// Ingest applies events in order, in one transaction. Lines already stored are skipped, so a log can be
// imported again safely. It returns how many were new.
func (s *Store) Ingest(ctx context.Context, events []postfix.Event) (int, error) {
	added := 0
	err := s.tx(ctx, func(tx *sql.Tx) error {
		added = 0
		for _, e := range events {
			ok, err := s.apply(ctx, tx, e)
			if err != nil {
				return fmt.Errorf("%s %s %q: %w", e.Server, e.At.Format(time.RFC3339), e.Raw, err)
			}
			if ok {
				added++
			}
		}
		return nil
	})
	return added, err
}

func (s *Store) apply(ctx context.Context, tx *sql.Tx, e postfix.Event) (bool, error) {
	serverID, err := s.serverID(ctx, tx, e.Server, e.At)
	if err != nil {
		return false, err
	}
	var seen int
	if err := s.row(ctx, tx, `SELECT count(*) FROM events WHERE server_id = ? AND at = ? AND line_hash = ?`, serverID, us(e.At), lineHash(e.Raw)).Scan(&seen); err != nil || seen > 0 {
		return false, err
	}

	if e.DKIMDomain != "" {
		if err := s.saveSelector(ctx, tx, e.DKIMDomain, e.DKIMSelector, e.At); err != nil {
			return false, err
		}
	}
	if e.Kind == postfix.Reject && e.QueueID == "" {
		return true, s.rejectedAttempt(ctx, tx, serverID, e)
	}
	msgID, err := s.message(ctx, tx, serverID, e)
	if err != nil {
		return false, err
	}

	var recipientID *int64
	switch e.Kind {
	case postfix.Client, postfix.Pickup:
		err = s.exec(ctx, tx, `UPDATE messages SET client_host = ?, client_ip = nullif(?, ''), sasl_username = nullif(?, '') WHERE id = ?`, e.Client, e.ClientIP, e.SASLUser, msgID)
	case postfix.Cleanup:
		err = s.exec(ctx, tx, `UPDATE messages SET message_id = ? WHERE id = ?`, e.MsgID, msgID)
	case postfix.Queued:
		err = s.exec(ctx, tx, `UPDATE messages SET sender = ?, sender_domain = CASE WHEN ? <> '' THEN ? ELSE sender_domain END, size = ?, nrcpt = ? WHERE id = ?`,
			e.Sender, e.Sender, senderDomain(e.Sender), e.Size, e.Nrcpt, msgID)
	case postfix.Header:
		err = s.exec(ctx, tx, `UPDATE messages SET subject = ? WHERE id = ?`, e.Subject, msgID)
	case postfix.Delivery:
		var id int64
		if id, err = s.attempt(ctx, tx, msgID, e); err == nil {
			recipientID = &id
			// A bounce has no sender: it belongs to the site it's sent back to.
			err = s.exec(ctx, tx, `UPDATE messages SET sender_domain = ? WHERE id = ? AND sender = '' AND coalesce(sender_domain, '') = ''`, postfix.Domain(e.To), msgID)
		}
	case postfix.Expired:
		// One event per recipient given up on, so providers count them.
		var ids []int64
		if ids, err = s.ids(ctx, tx, `SELECT id FROM recipients WHERE message_id = ? AND status IN ('queued', 'deferred')`, msgID); err == nil {
			err = s.exec(ctx, tx, `UPDATE recipients SET status = 'expired', response = ?, final_at = ? WHERE message_id = ? AND status IN ('queued', 'deferred')`, e.Response, us(e.At), msgID)
		}
		for _, id := range ids {
			if err == nil {
				err = s.event(ctx, tx, serverID, msgID, &id, e)
			}
		}
		if err == nil && len(ids) > 0 {
			return true, s.refresh(ctx, tx, msgID)
		}
	case postfix.Cancelled:
		err = s.exec(ctx, tx, `UPDATE messages SET removed_at = ? WHERE id = ?`, us(e.At), msgID)
	case postfix.Removed:
		err = s.exec(ctx, tx, `UPDATE messages SET removed_at = ? WHERE id = ?`, us(e.At), msgID)
		if err == nil && e.Program == "postfix/postsuper" {
			err = s.exec(ctx, tx, `UPDATE recipients SET status = 'deleted', final_at = ? WHERE message_id = ? AND status IN ('queued', 'deferred')`, us(e.At), msgID)
		}
	case postfix.Reject:
		err = s.exec(ctx, tx, `UPDATE messages SET removed_at = ?, sender = coalesce(sender, ?), sender_domain = coalesce(sender_domain, ?) WHERE id = ?`, us(e.At), e.Sender, senderDomain(e.Sender), msgID)
		if err == nil && e.To != "" {
			var id int64
			if id, err = s.recipient(ctx, tx, msgID, e); err == nil {
				recipientID = &id
			}
		}
	}
	if err == nil {
		err = s.event(ctx, tx, serverID, msgID, recipientID, e)
	}
	if err == nil {
		err = s.refresh(ctx, tx, msgID)
	}
	return err == nil, err
}

func senderDomain(sender string) string {
	if sender == "" {
		return ""
	}
	return postfix.Domain(sender)
}

func (s *Store) ids(ctx context.Context, db querier, q string, args ...any) ([]int64, error) {
	rows, err := s.rows(ctx, db, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *Store) serverID(ctx context.Context, tx *sql.Tx, name string, at time.Time) (int64, error) {
	if name == "" {
		name = "unknown"
	}
	var id int64
	err := s.row(ctx, tx, `INSERT INTO servers (name, first_seen, last_seen) VALUES (?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET
			last_seen = CASE WHEN excluded.last_seen > servers.last_seen THEN excluded.last_seen ELSE servers.last_seen END,
			first_seen = CASE WHEN excluded.first_seen < servers.first_seen THEN excluded.first_seen ELSE servers.first_seen END
		RETURNING id`, name, us(at), us(at)).Scan(&id)
	return id, err
}

// message finds the message a queue ID currently belongs to, or starts one. Queue IDs are reused, so a
// client= or pickup line starts a new message unless the latest one has no client yet and wasn't removed
// before the line was logged (its lines arrived out of order, as a replayed syslog queue can send them). Notices, removals and headers always join the latest message: alone they can't be a new
// one, which would otherwise sit in the queue view forever. Other lines join it unless it was removed
// before they were logged: then it's a reused queue ID whose start was missed (Mailinois started
// mid-flight, or a line was lost).
func (s *Store) message(ctx context.Context, tx *sql.Tx, serverID int64, e postfix.Event) (int64, error) {
	var id int64
	var removedAt *int64
	var hasClient bool
	err := s.row(ctx, tx, `SELECT id, removed_at, client_host IS NOT NULL FROM messages
		WHERE server_id = ? AND queue_id = ? AND received_at > ? ORDER BY id DESC LIMIT 1`,
		serverID, e.QueueID, us(e.At.Add(-30*24*time.Hour))).Scan(&id, &removedAt, &hasClient)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	found := err == nil
	starts := e.Kind == postfix.Client || e.Kind == postfix.Pickup
	trails := e.Kind == postfix.Notice || e.Kind == postfix.Removed || e.Kind == postfix.Cancelled || e.Kind == postfix.Header
	switch {
	case found && starts && !hasClient && (removedAt == nil || us(e.At) <= *removedAt):
		return id, nil
	case found && !starts && (trails || removedAt == nil || us(e.At) <= *removedAt):
		return id, nil
	}
	err = s.row(ctx, tx, `INSERT INTO messages (server_id, queue_id, received_at) VALUES (?, ?, ?) RETURNING id`,
		serverID, e.QueueID, us(e.At)).Scan(&id)
	return id, err
}

func (s *Store) rejectedAttempt(ctx context.Context, tx *sql.Tx, serverID int64, e postfix.Event) error {
	var msgID int64
	err := s.row(ctx, tx, `INSERT INTO messages (server_id, received_at, client_host, client_ip, sender, sender_domain, status, removed_at)
		VALUES (?, ?, ?, nullif(?, ''), ?, ?, ?, ?) RETURNING id`,
		serverID, us(e.At), e.Client, e.ClientIP, e.Sender, senderDomain(e.Sender), e.Status, us(e.At)).Scan(&msgID)
	if err != nil {
		return err
	}
	var recipientID *int64
	if e.To != "" {
		id, err := s.recipient(ctx, tx, msgID, e)
		if err != nil {
			return err
		}
		recipientID = &id
	}
	return s.event(ctx, tx, serverID, msgID, recipientID, e)
}

// recipient records a recipient refused before delivery (a rejection), with no attempt.
func (s *Store) recipient(ctx context.Context, tx *sql.Tx, msgID int64, e postfix.Event) (int64, error) {
	domain := postfix.Domain(e.To)
	var id int64
	err := s.row(ctx, tx, `INSERT INTO recipients (message_id, address, domain, provider, status, dsn, response, final_at)
		VALUES (?, ?, ?, ?, ?, nullif(?, ''), ?, ?)
		ON CONFLICT (message_id, address) DO UPDATE SET status = excluded.status, dsn = excluded.dsn,
			response = excluded.response, final_at = excluded.final_at
		RETURNING id`, msgID, e.To, domain, postfix.Provider(domain, ""), e.Status, e.DSN, e.Response, us(e.At)).Scan(&id)
	return id, err
}

func (s *Store) attempt(ctx context.Context, tx *sql.Tx, msgID int64, e postfix.Event) (int64, error) {
	domain := postfix.Domain(e.To)
	var final *int64
	if e.Status != "deferred" {
		v := us(e.At)
		final = &v
	}
	var id int64
	err := s.row(ctx, tx, `INSERT INTO recipients (message_id, address, orig_to, domain, provider, status, dsn, relay,
			relay_ip, response, attempts, delay, first_attempt_at, last_attempt_at, final_at)
		VALUES (?, ?, nullif(?, ''), ?, ?, ?, nullif(?, ''), nullif(?, ''), nullif(?, ''), ?, 1, ?, ?, ?, ?)
		ON CONFLICT (message_id, address) DO UPDATE SET
			status = excluded.status, dsn = excluded.dsn, response = excluded.response, delay = excluded.delay,
			relay = coalesce(excluded.relay, recipients.relay), relay_ip = coalesce(excluded.relay_ip, recipients.relay_ip),
			provider = CASE WHEN excluded.provider <> 'other' THEN excluded.provider ELSE recipients.provider END,
			attempts = recipients.attempts + 1, last_attempt_at = excluded.last_attempt_at, final_at = excluded.final_at,
			first_attempt_at = coalesce(recipients.first_attempt_at, excluded.first_attempt_at)
		RETURNING id`,
		msgID, e.To, e.OrigTo, domain, postfix.Provider(domain, e.Relay), e.Status, e.DSN, e.Relay, e.RelayIP,
		e.Response, e.Delay, us(e.At), us(e.At), final).Scan(&id)
	return id, err
}

func (s *Store) event(ctx context.Context, tx *sql.Tx, serverID, msgID int64, recipientID *int64, e postfix.Event) error {
	var key int64
	if recipientID != nil {
		key = *recipientID
	}
	return s.exec(ctx, tx, `INSERT INTO events (server_id, message_id, recipient_id, at, kind, program, status, dsn, response, raw, line_hash, recipient_key)
		VALUES (?, ?, ?, ?, ?, ?, nullif(?, ''), nullif(?, ''), nullif(?, ''), ?, ?, ?)
		ON CONFLICT DO NOTHING`,
		serverID, msgID, recipientID, us(e.At), string(e.Kind), e.Program, e.Status, e.DSN, e.Response, e.Raw, lineHash(e.Raw), key)
}

func (s *Store) refresh(ctx context.Context, tx *sql.Tx, msgID int64) error {
	has := func(cond string) string { return `max(CASE WHEN ` + cond + ` THEN 1 ELSE 0 END) = 1` }
	return s.exec(ctx, tx, `UPDATE messages SET status = coalesce((
			SELECT CASE
				WHEN `+has(`r.status = 'deferred'`)+` THEN 'deferred'
				WHEN `+has(`r.status = 'queued'`)+` THEN 'queued'
				WHEN `+has(`r.status = 'expired'`)+` THEN 'expired'
				WHEN `+has(`r.status = 'bounced'`)+` THEN 'bounced'
				WHEN `+has(`r.status = 'deleted'`)+` THEN 'deleted'
				WHEN `+has(`r.status IN ('rejected', 'discarded')`)+` THEN 'rejected'
				ELSE 'sent' END
			FROM recipients r WHERE r.message_id = ?
			HAVING count(*) > 0
		), CASE WHEN EXISTS (SELECT 1 FROM events WHERE message_id = messages.id AND kind = 'cancelled') THEN 'cancelled'
			WHEN removed_at IS NULL THEN 'queued' ELSE 'unknown' END)
		WHERE id = ?`, msgID, msgID)
}

// Purge deletes messages received before cutoff (with their lines), captures before captureCutoff, and
// checks and alert history before cutoff.
func (s *Store) Purge(ctx context.Context, cutoff, captureCutoff time.Time) (int64, error) {
	// In chunks, so ingest gets the write lock in between: the first purge after a long stop, or a shorter
	// retention, can be millions of rows. Each is a transaction that takes the lock first, as ingest's do: as a
	// lone statement, a delete that read before it wrote met SQLITE_BUSY whenever ingest committed in between.
	del := func(q string, at time.Time) (int64, error) {
		var k int64
		err := s.tx(ctx, func(tx *sql.Tx) error {
			res, err := tx.ExecContext(ctx, s.bind(q), us(at))
			if err == nil {
				k, err = res.RowsAffected()
			}
			return err
		})
		return k, err
	}
	var n int64
	for {
		k, err := del(`DELETE FROM messages WHERE id IN (SELECT id FROM messages WHERE received_at < ? LIMIT 2000)`, cutoff)
		if err != nil {
			return n, err
		}
		if n += k; k == 0 {
			break
		}
	}
	for _, q := range []string{`DELETE FROM dnsbl_checks WHERE checked_at < ?`, `DELETE FROM dns_checks WHERE checked_at < ?`, `DELETE FROM alert_events WHERE at < ?`} {
		if _, err := del(q, cutoff); err != nil {
			return n, err
		}
	}
	_, err := del(`DELETE FROM captures WHERE at < ?`, captureCutoff)
	return n, err
}

// Empty reports whether nothing has been ingested yet.
func (s *Store) Empty(ctx context.Context) (bool, error) {
	var n int
	err := s.row(ctx, s.db, `SELECT count(*) FROM (SELECT 1 FROM messages LIMIT 1) x`).Scan(&n)
	return n == 0, err
}

// Setting returns a stored value, making and storing it with make the first time.
func (s *Store) Setting(ctx context.Context, key string, make func() string) (string, error) {
	var v string
	err := s.row(ctx, s.db, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err == nil || !errors.Is(err, sql.ErrNoRows) {
		return v, err
	}
	if err := s.exec(ctx, s.db, `INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT (key) DO NOTHING`, key, make()); err != nil {
		return "", err
	}
	err = s.row(ctx, s.db, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	return v, err
}
