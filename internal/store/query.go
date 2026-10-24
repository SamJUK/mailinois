package store

import (
	"context"
	"time"
)

type Bucket struct {
	At       time.Time `json:"at"`
	Sent     int       `json:"sent"`
	Deferred int       `json:"deferred"`
	Bounced  int       `json:"bounced"`
	Rejected int       `json:"rejected"`
}

type Failure struct {
	At        time.Time `json:"at"`
	Server    string    `json:"server"`
	MessageID int64     `json:"message_id"`
	Recipient string    `json:"recipient"`
	Sender    string    `json:"sender"`
	Status    string    `json:"status"`
	DSN       string    `json:"dsn"`
	Response  string    `json:"response"`
}

type Dashboard struct {
	Received  int        `json:"received"`
	Sent      int        `json:"sent"`
	Deferrals int        `json:"deferrals"`
	Bounced   int        `json:"bounced"`
	Expired   int        `json:"expired"`
	Rejected  int        `json:"rejected"`
	Queue     int        `json:"queue"`
	Oldest    *time.Time `json:"oldest,omitempty"`
	P50       *float64   `json:"p50"`
	P95       *float64   `json:"p95"`
	Buckets   []Bucket   `json:"buckets,omitempty"`
	Failures  []Failure  `json:"failures,omitempty"`
}

// Finals counts recipients with an outcome: what delivery and bounce rates divide by.
func (d Dashboard) Finals() int { return d.Sent + d.Bounced + d.Expired }

func (s *Store) Dashboard(ctx context.Context, f Filter, step, lifetime time.Duration) (Dashboard, error) {
	var d Dashboard
	c := f.match()
	err := s.row(ctx, s.db, `SELECT count(*) FROM messages m
		WHERE m.queue_id IS NOT NULL AND m.received_at >= ? AND m.received_at < ? AND `+c.sql(),
		append([]any{us(f.Since), us(f.Until)}, c.args...)...).Scan(&d.Received)
	if err != nil {
		return d, err
	}
	var cancelled int
	err = s.row(ctx, s.db, `SELECT count(*) FROM messages m
		WHERE m.status = 'cancelled' AND m.queue_id IS NOT NULL AND m.received_at >= ? AND m.received_at < ? AND `+c.sql(),
		append([]any{us(f.Since), us(f.Until)}, c.args...)...).Scan(&cancelled)
	if err != nil {
		return d, err
	}
	d.Received -= cancelled

	err = s.row(ctx, s.db, `SELECT
			count(CASE WHEN r.status = 'sent' THEN 1 END),
			count(CASE WHEN r.status = 'bounced' THEN 1 END),
			count(CASE WHEN r.status = 'expired' THEN 1 END),
			count(CASE WHEN r.status IN ('rejected', 'discarded') THEN 1 END)
		FROM `+c.from("recipients r")+`
		WHERE r.final_at >= ? AND r.final_at < ? AND `+c.sql(),
		append([]any{us(f.Since), us(f.Until)}, c.args...)...).Scan(&d.Sent, &d.Bounced, &d.Expired, &d.Rejected)
	if err != nil {
		return d, err
	}
	if d.Sent > 0 {
		// Nearest-rank percentiles from one sort: SQLite has no percentile_cont.
		p50, p95 := int(0.5*float64(d.Sent-1)), int(0.95*float64(d.Sent-1))
		rows, err := s.rows(ctx, s.db, `SELECT rank, delay FROM (
				SELECT r.delay, row_number() OVER (ORDER BY r.delay) - 1 AS rank FROM `+c.from("recipients r")+`
				WHERE r.status = 'sent' AND r.final_at >= ? AND r.final_at < ? AND `+c.sql()+`
			) ranked WHERE rank IN (?, ?)`,
			append(append([]any{us(f.Since), us(f.Until)}, c.args...), p50, p95)...)
		if err != nil {
			return d, err
		}
		for rows.Next() {
			var rank int
			var v float64
			if err := rows.Scan(&rank, &v); err != nil {
				rows.Close()
				return d, err
			}
			if rank == p50 {
				d.P50 = &v
			}
			if rank == p95 {
				d.P95 = &v
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return d, err
		}
	}
	// kind IN (…) spelled as events_outcomes has it, so the index applies; only deliveries are ever deferred.
	err = s.row(ctx, s.db, `SELECT count(*) FROM `+c.from("events e")+`
		WHERE e.kind IN ('delivery', 'reject', 'expired') AND e.status = 'deferred' AND e.at >= ? AND e.at < ? AND `+c.sql(),
		append([]any{us(f.Since), us(f.Until)}, c.args...)...).Scan(&d.Deferrals)
	if err != nil {
		return d, err
	}
	if d.Queue, d.Oldest, err = s.queueDepth(ctx, f, lifetime); err != nil {
		return d, err
	}
	if d.Buckets, err = s.buckets(ctx, f, step); err != nil {
		return d, err
	}
	d.Failures, err = s.Failures(ctx, f, 10)
	return d, err
}

func (s *Store) queueDepth(ctx context.Context, f Filter, lifetime time.Duration) (int, *time.Time, error) {
	c := f.match()
	var n int
	var oldest *int64
	err := s.row(ctx, s.db, `SELECT count(*), min(m.received_at) FROM messages m
		WHERE m.removed_at IS NULL AND m.queue_id IS NOT NULL AND m.received_at > ? AND `+c.sql(),
		append([]any{us(time.Now().Add(-lifetime))}, c.args...)...).Scan(&n, &oldest)
	return n, fromUSp(oldest), err
}

func (s *Store) buckets(ctx context.Context, f Filter, step time.Duration) ([]Bucket, error) {
	c := f.match()
	rows, err := s.rows(ctx, s.db, `SELECT (e.at - ?) / ? AS b,
			count(CASE WHEN e.status = 'sent' THEN 1 END),
			count(CASE WHEN e.status = 'deferred' THEN 1 END),
			count(CASE WHEN e.status IN ('bounced', 'expired') THEN 1 END),
			count(CASE WHEN e.status IN ('rejected', 'discarded') THEN 1 END)
		FROM `+c.from("events e")+`
		WHERE e.kind IN ('delivery', 'reject', 'expired') AND e.at >= ? AND e.at < ? AND `+c.sql()+`
		GROUP BY b`, append([]any{us(f.Since), step.Microseconds(), us(f.Since), us(f.Until)}, c.args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	n := int(f.Until.Sub(f.Since) / step)
	out := make([]Bucket, n)
	for i := range out {
		out[i].At = f.Since.Add(time.Duration(i) * step)
	}
	for rows.Next() {
		var i int64
		var b Bucket
		if err := rows.Scan(&i, &b.Sent, &b.Deferred, &b.Bounced, &b.Rejected); err != nil {
			return nil, err
		}
		if i >= 0 && int(i) < n {
			b.At = out[i].At
			out[i] = b
		}
	}
	return out, rows.Err()
}

// Failures lists the latest deferrals, bounces, expiries and rejections, newest first.
func (s *Store) Failures(ctx context.Context, f Filter, limit int) ([]Failure, error) {
	c := f.match()
	rows, err := s.rows(ctx, s.db, `SELECT e.at, sv.name, m.id, coalesce(rc.address, ''), coalesce(m.sender, ''), e.status,
			coalesce(e.dsn, ''), coalesce(e.response, '')
		FROM events e CROSS JOIN messages m JOIN servers sv ON sv.id = e.server_id
		LEFT JOIN recipients rc ON rc.id = e.recipient_id
		WHERE m.id = e.message_id AND e.kind IN ('delivery', 'reject', 'expired') AND e.status <> 'sent'
			AND e.at >= ? AND e.at < ? AND `+c.sql()+`
		ORDER BY e.at DESC, e.id DESC LIMIT ?`, append(append([]any{us(f.Since), us(f.Until)}, c.args...), limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Failure
	for rows.Next() {
		var x Failure
		var at int64
		if err := rows.Scan(&at, &x.Server, &x.MessageID, &x.Recipient, &x.Sender, &x.Status, &x.DSN, &x.Response); err != nil {
			return nil, err
		}
		x.At = fromUS(at)
		out = append(out, x)
	}
	return out, rows.Err()
}

type MessageRow struct {
	ID          int64     `json:"id"`
	ReceivedAt  time.Time `json:"received_at"`
	Server      string    `json:"server"`
	QueueID     string    `json:"queue_id"`
	Sender      string    `json:"sender"`
	SenderKnown bool      `json:"sender_known"`
	Subject     string    `json:"subject"`
	Status      string    `json:"status"`
	Size        int64     `json:"size"`
	Recipient   string    `json:"recipient"`
	Recipients  int       `json:"recipients"`
}

// Messages lists messages received in the period, newest first: limit rows from offset.
func (s *Store) Messages(ctx context.Context, f Filter, offset, limit int) ([]MessageRow, error) {
	c := f.match()
	rows, err := s.rows(ctx, s.db, `SELECT m.id, m.received_at, sv.name, coalesce(m.queue_id, ''), coalesce(m.sender, ''), m.sender IS NOT NULL,
			`+subjectSQL+`, m.status, coalesce(m.size, 0),
			coalesce((SELECT address FROM recipients r WHERE r.message_id = m.id ORDER BY r.id LIMIT 1), ''),
			(SELECT count(*) FROM recipients r WHERE r.message_id = m.id)
		FROM messages m JOIN servers sv ON sv.id = m.server_id
		WHERE m.received_at >= ? AND m.received_at < ? AND `+c.sql()+`
		ORDER BY m.received_at DESC, m.id DESC LIMIT ? OFFSET ?`,
		append(append([]any{us(f.Since), us(f.Until)}, c.args...), limit, offset)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MessageRow
	for rows.Next() {
		var m MessageRow
		var at int64
		if err := rows.Scan(&m.ID, &at, &m.Server, &m.QueueID, &m.Sender, &m.SenderKnown, &m.Subject, &m.Status, &m.Size, &m.Recipient, &m.Recipients); err != nil {
			return nil, err
		}
		m.ReceivedAt = fromUS(at)
		out = append(out, m)
	}
	return out, rows.Err()
}

type Message struct {
	ID           int64       `json:"id"`
	Server       string      `json:"server"`
	QueueID      string      `json:"queue_id"`
	ReceivedAt   time.Time   `json:"received_at"`
	ClientHost   string      `json:"client_host"`
	ClientIP     string      `json:"client_ip"`
	SASLUser     string      `json:"sasl_user"`
	Sender       string      `json:"sender"`
	SenderKnown  bool        `json:"sender_known"`
	SenderDomain string      `json:"sender_domain"`
	MessageID    string      `json:"message_id"`
	Subject      string      `json:"subject"`
	Size         int64       `json:"size"`
	Nrcpt        int         `json:"nrcpt"`
	Status       string      `json:"status"`
	RemovedAt    *time.Time  `json:"removed_at,omitempty"`
	Recipients   []Recipient `json:"recipients,omitempty"`
	Events       []Event     `json:"events,omitempty"`
}

type Recipient struct {
	ID          int64      `json:"id"`
	Address     string     `json:"address"`
	OrigTo      string     `json:"orig_to"`
	Provider    string     `json:"provider"`
	Status      string     `json:"status"`
	DSN         string     `json:"dsn"`
	Relay       string     `json:"relay"`
	RelayIP     string     `json:"relay_ip"`
	Response    string     `json:"response"`
	Attempts    int        `json:"attempts"`
	Delay       *float64   `json:"delay,omitempty"`
	LastAttempt *time.Time `json:"last_attempt,omitempty"`
}

type Event struct {
	At        time.Time `json:"at"`
	Kind      string    `json:"kind"`
	Program   string    `json:"program"`
	Status    string    `json:"status"`
	DSN       string    `json:"dsn"`
	Recipient string    `json:"recipient"`
	Raw       string    `json:"raw"`
}

// Message returns one message, or sql.ErrNoRows when it doesn't exist or f doesn't allow it.
func (s *Store) Message(ctx context.Context, f Filter, id int64) (*Message, error) {
	var m Message
	var at int64
	var removed *int64
	c := f.match()
	err := s.row(ctx, s.db, `SELECT m.id, sv.name, coalesce(m.queue_id, ''), m.received_at, coalesce(m.client_host, ''),
			coalesce(m.client_ip, ''), coalesce(m.sasl_username, ''), coalesce(m.sender, ''), m.sender IS NOT NULL, coalesce(m.sender_domain, ''),
			coalesce(m.message_id, ''), `+subjectSQL+`, coalesce(m.size, 0), coalesce(m.nrcpt, 0), m.status, m.removed_at
		FROM messages m JOIN servers sv ON sv.id = m.server_id WHERE m.id = ? AND `+c.sql(),
		append([]any{id}, c.args...)...).Scan(
		&m.ID, &m.Server, &m.QueueID, &at, &m.ClientHost, &m.ClientIP, &m.SASLUser, &m.Sender, &m.SenderKnown, &m.SenderDomain,
		&m.MessageID, &m.Subject, &m.Size, &m.Nrcpt, &m.Status, &removed)
	if err != nil {
		return nil, err
	}
	m.ReceivedAt, m.RemovedAt = fromUS(at), fromUSp(removed)

	rows, err := s.rows(ctx, s.db, `SELECT id, address, coalesce(orig_to, ''), provider, status, coalesce(dsn, ''),
			coalesce(relay, ''), coalesce(relay_ip, ''), coalesce(response, ''), attempts, delay, last_attempt_at
		FROM recipients WHERE message_id = ? ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var r Recipient
		var last *int64
		if err := rows.Scan(&r.ID, &r.Address, &r.OrigTo, &r.Provider, &r.Status, &r.DSN, &r.Relay, &r.RelayIP, &r.Response, &r.Attempts, &r.Delay, &last); err != nil {
			rows.Close()
			return nil, err
		}
		r.LastAttempt = fromUSp(last)
		m.Recipients = append(m.Recipients, r)
	}
	rows.Close()

	rows, err = s.rows(ctx, s.db, `SELECT e.at, e.kind, e.program, coalesce(e.status, ''), coalesce(e.dsn, ''),
			coalesce(r.address, ''), e.raw
		FROM events e LEFT JOIN recipients r ON r.id = e.recipient_id WHERE e.message_id = ? ORDER BY e.at, e.id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var e Event
		var at int64
		if err := rows.Scan(&at, &e.Kind, &e.Program, &e.Status, &e.DSN, &e.Recipient, &e.Raw); err != nil {
			return nil, err
		}
		e.At = fromUS(at)
		m.Events = append(m.Events, e)
	}
	return &m, rows.Err()
}

type QueuedMessage struct {
	ID          int64     `json:"id"`
	ReceivedAt  time.Time `json:"received_at"`
	Server      string    `json:"server"`
	QueueID     string    `json:"queue_id"`
	Sender      string    `json:"sender"`
	SenderKnown bool      `json:"sender_known"`
	Pending     int       `json:"pending"`
	Attempts    int       `json:"attempts"`
	DSN         string    `json:"dsn"`
	Response    string    `json:"response"`
}

// Queue lists messages still in a queue, oldest first, with the latest answer for a pending recipient.
func (s *Store) Queue(ctx context.Context, f Filter, lifetime time.Duration, limit int) ([]QueuedMessage, error) {
	c := f.match()
	pending := `FROM recipients r WHERE r.message_id = m.id AND r.status IN ('queued', 'deferred')`
	rows, err := s.rows(ctx, s.db, `SELECT m.id, m.received_at, sv.name, m.queue_id, coalesce(m.sender, ''), m.sender IS NOT NULL,
			(SELECT count(*) `+pending+`),
			coalesce((SELECT max(r.attempts) FROM recipients r WHERE r.message_id = m.id), 0),
			coalesce((SELECT r.dsn `+pending+` ORDER BY r.last_attempt_at DESC LIMIT 1), ''),
			coalesce((SELECT r.response `+pending+` ORDER BY r.last_attempt_at DESC LIMIT 1), '')
		FROM messages m JOIN servers sv ON sv.id = m.server_id
		WHERE m.removed_at IS NULL AND m.queue_id IS NOT NULL AND m.received_at > ? AND `+c.sql()+`
		ORDER BY m.received_at LIMIT ?`, append(append([]any{us(time.Now().Add(-lifetime))}, c.args...), limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QueuedMessage
	for rows.Next() {
		var q QueuedMessage
		var at int64
		if err := rows.Scan(&q.ID, &at, &q.Server, &q.QueueID, &q.Sender, &q.SenderKnown, &q.Pending, &q.Attempts, &q.DSN, &q.Response); err != nil {
			return nil, err
		}
		q.ReceivedAt = fromUS(at)
		out = append(out, q)
	}
	return out, rows.Err()
}

type Reason struct {
	Provider string    `json:"provider"`
	DSN      string    `json:"dsn"`
	Count    int       `json:"count"`
	Last     time.Time `json:"last"`
	Example  string    `json:"example"`
}

// DeferralReasons groups deferrals by provider and DSN code, with the latest response as an example.
func (s *Store) DeferralReasons(ctx context.Context, f Filter) ([]Reason, error) {
	c := f.match()
	rows, err := s.rows(ctx, s.db, `SELECT provider, dsn, n, at, coalesce(response, '') FROM (
			SELECT rc.provider, coalesce(e.dsn, '') AS dsn, e.at, e.response,
				count(*) OVER (PARTITION BY rc.provider, coalesce(e.dsn, '')) AS n,
				row_number() OVER (PARTITION BY rc.provider, coalesce(e.dsn, '') ORDER BY e.at DESC) AS rn
			FROM events e JOIN recipients rc ON rc.id = e.recipient_id JOIN messages m ON m.id = e.message_id
			WHERE e.kind = 'delivery' AND e.status = 'deferred' AND e.at >= ? AND e.at < ? AND `+c.sql()+`
		) x WHERE rn = 1 ORDER BY n DESC LIMIT 25`, append([]any{us(f.Since), us(f.Until)}, c.args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Reason
	for rows.Next() {
		var r Reason
		var at int64
		if err := rows.Scan(&r.Provider, &r.DSN, &r.Count, &at, &r.Example); err != nil {
			return nil, err
		}
		r.Last = fromUS(at)
		out = append(out, r)
	}
	return out, rows.Err()
}

type ProviderStats struct {
	Provider     string     `json:"provider"`
	Attempts     int        `json:"attempts"`
	Sent         int        `json:"sent"`
	Deferred     int        `json:"deferred"`
	Failed       int        `json:"failed"`
	PrevAttempts int        `json:"prev_attempts"`
	PrevSent     int        `json:"prev_sent"`
	PrevDeferred int        `json:"prev_deferred"`
	PrevFailed   int        `json:"prev_failed"`
	Responses    []Response `json:"responses,omitempty"`
}

type Response struct {
	Status  string `json:"status"`
	DSN     string `json:"dsn"`
	Count   int    `json:"count"`
	Example string `json:"example"`
}

// Providers compares each provider's outcomes in the period with the period of the same length before it.
func (s *Store) Providers(ctx context.Context, f Filter) ([]ProviderStats, error) {
	c := f.match()
	since, prev := us(f.Since), us(f.Since.Add(-f.Until.Sub(f.Since)))
	cnt := func(when, what string) string {
		return `count(CASE WHEN e.at ` + when + ` ? AND ` + what + ` THEN 1 END)`
	}
	rows, err := s.rows(ctx, s.db, `SELECT rc.provider,
			`+cnt(">=", "e.kind = 'delivery'")+`, `+cnt(">=", "e.status = 'sent'")+`,
			`+cnt(">=", "e.status = 'deferred'")+`, `+cnt(">=", "e.status IN ('bounced', 'expired')")+`,
			`+cnt("<", "e.kind = 'delivery'")+`, `+cnt("<", "e.status = 'sent'")+`,
			`+cnt("<", "e.status = 'deferred'")+`, `+cnt("<", "e.status IN ('bounced', 'expired')")+`
		FROM events e JOIN recipients rc ON rc.id = e.recipient_id JOIN messages m ON m.id = e.message_id
		WHERE e.kind IN ('delivery', 'expired') AND e.at >= ? AND e.at < ? AND `+c.sql()+`
		GROUP BY rc.provider`,
		append([]any{since, since, since, since, since, since, since, since, prev, us(f.Until)}, c.args...)...)
	if err != nil {
		return nil, err
	}
	stats := map[string]*ProviderStats{}
	for rows.Next() {
		var p ProviderStats
		if err := rows.Scan(&p.Provider, &p.Attempts, &p.Sent, &p.Deferred, &p.Failed, &p.PrevAttempts, &p.PrevSent, &p.PrevDeferred, &p.PrevFailed); err != nil {
			rows.Close()
			return nil, err
		}
		stats[p.Provider] = &p
	}
	rows.Close()

	rows, err = s.rows(ctx, s.db, `SELECT provider, status, dsn, n, coalesce(response, '') FROM (
			SELECT rc.provider, e.status, coalesce(e.dsn, '') AS dsn, e.response,
				count(*) OVER (PARTITION BY rc.provider, e.status, coalesce(e.dsn, '')) AS n,
				row_number() OVER (PARTITION BY rc.provider, e.status, coalesce(e.dsn, '') ORDER BY e.at DESC) AS rn
			FROM events e JOIN recipients rc ON rc.id = e.recipient_id JOIN messages m ON m.id = e.message_id
			WHERE e.kind IN ('delivery', 'expired') AND e.status <> 'sent' AND e.at >= ? AND e.at < ? AND `+c.sql()+`
		) x WHERE rn = 1 ORDER BY n DESC`, append([]any{since, us(f.Until)}, c.args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var r Response
		if err := rows.Scan(&name, &r.Status, &r.DSN, &r.Count, &r.Example); err != nil {
			return nil, err
		}
		if st := stats[name]; st != nil && len(st.Responses) < 5 {
			st.Responses = append(st.Responses, r)
		}
	}
	var out []ProviderStats
	for _, key := range []string{"google", "microsoft", "yahoo", "apple", "other"} {
		if st := stats[key]; st != nil {
			out = append(out, *st)
		} else {
			out = append(out, ProviderStats{Provider: key})
		}
	}
	return out, rows.Err()
}

type Server struct {
	Name      string    `json:"name"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	Received  int       `json:"received"`
	Sent      int       `json:"sent"`
	Deferred  int       `json:"deferred"`
	Failed    int       `json:"failed"`
	Queue     int       `json:"queue"`
}

// Servers summarises every server over the last 24 hours, with its queue now.
func (s *Store) Servers(ctx context.Context, f Filter, lifetime time.Duration) ([]Server, error) {
	c := f.match()
	day, now := us(time.Now().Add(-24*time.Hour)), time.Now()
	rows, err := s.rows(ctx, s.db, `SELECT sv.name, sv.first_seen, sv.last_seen,
			count(CASE WHEN m.queue_id IS NOT NULL AND m.received_at > ? THEN 1 END),
			coalesce(sum((SELECT count(*) FROM events e WHERE e.message_id = m.id AND e.status = 'sent' AND e.at > ?)), 0),
			coalesce(sum((SELECT count(*) FROM events e WHERE e.message_id = m.id AND e.kind = 'delivery' AND e.status = 'deferred' AND e.at > ?)), 0),
			coalesce(sum((SELECT count(*) FROM events e WHERE e.message_id = m.id AND e.status IN ('bounced', 'expired', 'rejected') AND e.at > ?)), 0),
			count(CASE WHEN m.removed_at IS NULL AND m.queue_id IS NOT NULL AND m.received_at > ? THEN 1 END)
		FROM servers sv LEFT JOIN messages m ON m.server_id = sv.id AND m.received_at > ? AND `+c.sql()+`
		GROUP BY sv.id, sv.name, sv.first_seen, sv.last_seen ORDER BY sv.name`,
		append([]any{day, day, day, day, us(now.Add(-lifetime)), us(now.Add(-lifetime - 24*time.Hour))}, c.args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Server
	for rows.Next() {
		var x Server
		var first, last int64
		if err := rows.Scan(&x.Name, &first, &last, &x.Received, &x.Sent, &x.Deferred, &x.Failed, &x.Queue); err != nil {
			return nil, err
		}
		x.FirstSeen, x.LastSeen = fromUS(first), fromUS(last)
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Store) strings(ctx context.Context, q string, args ...any) ([]string, error) {
	rows, err := s.rows(ctx, s.db, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) ServerNames(ctx context.Context) ([]string, error) {
	return s.strings(ctx, `SELECT name FROM servers ORDER BY name`)
}

// SenderDomains are the busiest sending domains since a time, for the site picker when no sites are set.
func (s *Store) SenderDomains(ctx context.Context, since time.Time, limit int) ([]string, error) {
	return s.strings(ctx, `SELECT sender_domain FROM messages WHERE received_at > ? AND coalesce(sender_domain, '') <> ''
		GROUP BY sender_domain ORDER BY count(*) DESC LIMIT ?`, us(since), limit)
}

type TailLine struct {
	ID        int64     `json:"id"`
	At        time.Time `json:"at"`
	Server    string    `json:"server"`
	MessageID int64     `json:"message_id"`
	Recipient string    `json:"recipient"`
	Status    string    `json:"status"`
	DSN       string    `json:"dsn"`
}

// Tail returns delivery outcomes after an event id, newest first.
func (s *Store) Tail(ctx context.Context, f Filter, after int64, limit int) ([]TailLine, error) {
	c := f.match()
	rows, err := s.rows(ctx, s.db, `SELECT e.id, e.at, sv.name, m.id, coalesce(r.address, ''), coalesce(e.status, ''), coalesce(e.dsn, '')
		FROM events e JOIN messages m ON m.id = e.message_id JOIN servers sv ON sv.id = e.server_id
		LEFT JOIN recipients r ON r.id = e.recipient_id
		WHERE e.id > ? AND e.kind IN ('delivery', 'reject', 'expired') AND `+c.sql()+`
		ORDER BY e.id DESC LIMIT ?`, append(append([]any{after}, c.args...), limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TailLine
	for rows.Next() {
		var t TailLine
		var at int64
		if err := rows.Scan(&t.ID, &at, &t.Server, &t.MessageID, &t.Recipient, &t.Status, &t.DSN); err != nil {
			return nil, err
		}
		t.At = fromUS(at)
		out = append(out, t)
	}
	return out, rows.Err()
}

type QueueGauge struct {
	Server string     `json:"server"`
	Depth  int        `json:"depth"`
	Oldest *time.Time `json:"oldest,omitempty"`
}

func (s *Store) QueueGauges(ctx context.Context, lifetime time.Duration) ([]QueueGauge, error) {
	rows, err := s.rows(ctx, s.db, `SELECT sv.name, count(m.id), min(m.received_at) FROM servers sv
		LEFT JOIN messages m ON m.server_id = sv.id AND m.removed_at IS NULL AND m.queue_id IS NOT NULL AND m.received_at > ?
		GROUP BY sv.name ORDER BY sv.name`, us(time.Now().Add(-lifetime)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QueueGauge
	for rows.Next() {
		var g QueueGauge
		var oldest *int64
		if err := rows.Scan(&g.Server, &g.Depth, &oldest); err != nil {
			return nil, err
		}
		g.Oldest = fromUSp(oldest)
		out = append(out, g)
	}
	return out, rows.Err()
}

// Outcomes counts what an alert needs over a period: recipients finished, sent, bounced or expired,
// rejected, deferral attempts and all attempts, optionally for one provider.
type Outcomes struct {
	Finals    int `json:"finals"`
	Sent      int `json:"sent"`
	Failed    int `json:"failed"`
	Rejected  int `json:"rejected"`
	Deferrals int `json:"deferrals"`
	Attempts  int `json:"attempts"`
}

func (s *Store) Outcomes(ctx context.Context, f Filter, provider string) (Outcomes, error) {
	c := f.match()
	if provider != "" {
		c.add(`r.provider = ?`, provider)
	}
	var o Outcomes
	err := s.row(ctx, s.db, `SELECT
			count(CASE WHEN e.status IN ('sent', 'bounced', 'expired') THEN 1 END),
			count(CASE WHEN e.status = 'sent' THEN 1 END),
			count(CASE WHEN e.status IN ('bounced', 'expired') THEN 1 END),
			count(CASE WHEN e.status IN ('rejected', 'discarded') THEN 1 END),
			count(CASE WHEN e.kind = 'delivery' AND e.status = 'deferred' THEN 1 END),
			count(CASE WHEN e.kind = 'delivery' THEN 1 END)
		FROM events e JOIN messages m ON m.id = e.message_id LEFT JOIN recipients r ON r.id = e.recipient_id
		WHERE e.kind IN ('delivery', 'reject', 'expired') AND e.at >= ? AND e.at < ? AND `+c.sql(),
		append([]any{us(f.Since), us(f.Until)}, c.args...)...).Scan(&o.Finals, &o.Sent, &o.Failed, &o.Rejected, &o.Deferrals, &o.Attempts)
	return o, err
}

// QueueNow is the queue depth and oldest message for a filter, ignoring its period.
func (s *Store) QueueNow(ctx context.Context, f Filter, lifetime time.Duration) (int, *time.Time, error) {
	return s.queueDepth(ctx, f, lifetime)
}

// LastLine is when each server last sent a line.
func (s *Store) LastLine(ctx context.Context) (map[string]time.Time, error) {
	rows, err := s.rows(ctx, s.db, `SELECT name, last_seen FROM servers`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var name string
		var at int64
		if err := rows.Scan(&name, &at); err != nil {
			return nil, err
		}
		out[name] = fromUS(at)
	}
	return out, rows.Err()
}
