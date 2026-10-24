package store

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"io"
	"mime"
	"net/textproto"
	"strings"
	"time"
)

func (s *Store) SavePTR(ctx context.Context, ip, ptr string, confirmed bool, at time.Time) error {
	ok := 0
	if confirmed {
		ok = 1
	}
	return s.exec(ctx, s.db, `INSERT INTO sending_ips (ip, ptr, forward_confirmed, checked_at) VALUES (?, nullif(?, ''), ?, ?)
		ON CONFLICT (ip) DO UPDATE SET ptr = excluded.ptr, forward_confirmed = excluded.forward_confirmed, checked_at = excluded.checked_at`,
		ip, ptr, ok, us(at))
}

func (s *Store) SaveCheck(ctx context.Context, ip, zone, status, response string, at time.Time) error {
	return s.exec(ctx, s.db, `INSERT INTO dnsbl_checks (ip, zone, status, response, checked_at) VALUES (?, ?, ?, nullif(?, ''), ?)`,
		ip, zone, status, response, us(at))
}

type SendingIP struct {
	IP               string     `json:"ip"`
	PTR              string     `json:"ptr"`
	ForwardConfirmed bool       `json:"forward_confirmed"`
	CheckedAt        *time.Time `json:"checked_at,omitempty"`
	Checks           []Check    `json:"checks,omitempty"`
}

type Check struct {
	IP        string    `json:"ip"`
	Zone      string    `json:"zone"`
	Status    string    `json:"status"`
	Response  string    `json:"response"`
	CheckedAt time.Time `json:"checked_at"`
}

// SendingIPs returns each IP with its latest result per blocklist.
func (s *Store) SendingIPs(ctx context.Context, ips []string) ([]SendingIP, error) {
	out := make([]SendingIP, len(ips))
	index := map[string]*SendingIP{}
	for i, ip := range ips {
		out[i].IP = ip
		index[ip] = &out[i]
	}
	if len(ips) == 0 {
		return out, nil
	}
	args := make([]any, len(ips))
	for i, ip := range ips {
		args[i] = ip
	}
	rows, err := s.rows(ctx, s.db, `SELECT ip, coalesce(ptr, ''), forward_confirmed, checked_at FROM sending_ips WHERE ip IN (`+marks(len(ips))+`)`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var ip, ptr string
		var ok int
		var at int64
		if err := rows.Scan(&ip, &ptr, &ok, &at); err != nil {
			rows.Close()
			return nil, err
		}
		t := fromUS(at)
		index[ip].PTR, index[ip].ForwardConfirmed, index[ip].CheckedAt = ptr, ok == 1, &t
	}
	rows.Close()
	checks, err := s.checks(ctx, `SELECT ip, zone, status, coalesce(response, ''), checked_at FROM (
			SELECT *, row_number() OVER (PARTITION BY ip, zone ORDER BY checked_at DESC) AS rn FROM dnsbl_checks WHERE ip IN (`+marks(len(ips))+`)
		) c WHERE rn = 1 ORDER BY ip, zone`, args...)
	for _, c := range checks {
		index[c.IP].Checks = append(index[c.IP].Checks, c)
	}
	return out, err
}

// ListingChanges returns the checks whose status differed from the one before, newest first.
func (s *Store) ListingChanges(ctx context.Context, limit int) ([]Check, error) {
	return s.checks(ctx, `SELECT ip, zone, status, coalesce(response, ''), checked_at FROM (
			SELECT *, lag(status) OVER (PARTITION BY ip, zone ORDER BY checked_at) AS prev FROM dnsbl_checks
		) c WHERE prev IS NULL OR prev <> status ORDER BY checked_at DESC LIMIT ?`, limit)
}

func (s *Store) checks(ctx context.Context, q string, args ...any) ([]Check, error) {
	rows, err := s.rows(ctx, s.db, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Check
	for rows.Next() {
		var c Check
		var at int64
		if err := rows.Scan(&c.IP, &c.Zone, &c.Status, &c.Response, &at); err != nil {
			return nil, err
		}
		c.CheckedAt = fromUS(at)
		out = append(out, c)
	}
	return out, rows.Err()
}

type DNSResult struct {
	Domain    string    `json:"domain"`
	Kind      string    `json:"kind"`
	Subject   string    `json:"subject"`
	Status    string    `json:"status"`
	Summary   string    `json:"summary"`
	Record    string    `json:"record"`
	CheckedAt time.Time `json:"checked_at"`
}

func (s *Store) SaveDNS(ctx context.Context, r DNSResult) error {
	return s.exec(ctx, s.db, `INSERT INTO dns_checks (domain, kind, subject, status, summary, record, checked_at) VALUES (?, ?, ?, ?, ?, nullif(?, ''), ?)`,
		r.Domain, r.Kind, r.Subject, r.Status, r.Summary, r.Record, us(r.CheckedAt))
}

// DNSResults returns the latest result of every check, by domain.
func (s *Store) DNSResults(ctx context.Context) ([]DNSResult, error) {
	rows, err := s.rows(ctx, s.db, `SELECT domain, kind, subject, status, summary, coalesce(record, ''), checked_at FROM (
			SELECT *, row_number() OVER (PARTITION BY domain, kind, subject ORDER BY checked_at DESC) AS rn FROM dns_checks
		) c WHERE rn = 1 ORDER BY domain, kind, subject`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DNSResult
	for rows.Next() {
		var r DNSResult
		var at int64
		if err := rows.Scan(&r.Domain, &r.Kind, &r.Subject, &r.Status, &r.Summary, &r.Record, &at); err != nil {
			return nil, err
		}
		r.CheckedAt = fromUS(at)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) saveSelector(ctx context.Context, db querier, domain, selector string, at time.Time) error {
	return s.exec(ctx, db, `INSERT INTO dkim_selectors (domain, selector, last_seen) VALUES (?, ?, ?)
		ON CONFLICT (domain, selector) DO UPDATE SET last_seen = CASE WHEN excluded.last_seen > dkim_selectors.last_seen THEN excluded.last_seen ELSE dkim_selectors.last_seen END`,
		strings.ToLower(domain), selector, us(at))
}

// Selectors are the DKIM selectors seen signing for each domain since a time.
func (s *Store) Selectors(ctx context.Context, since time.Time) (map[string][]string, error) {
	rows, err := s.rows(ctx, s.db, `SELECT domain, selector FROM dkim_selectors WHERE last_seen > ? ORDER BY domain, selector`, us(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var d, sel string
		if err := rows.Scan(&d, &sel); err != nil {
			return nil, err
		}
		out[d] = append(out[d], sel)
	}
	return out, rows.Err()
}

// Capture is a captured message as stored; Header and Body are raw bytes here and gzip in the table.
type Capture struct {
	QueueID   string    `json:"queue_id"`
	MessageID string    `json:"message_id"`
	Server    string    `json:"server"`
	At        time.Time `json:"at"`
	Header    []byte    `json:"-"`
	Body      []byte    `json:"-"`
	Size      int64     `json:"size"`
	Truncated bool      `json:"truncated"`
}

func (s *Store) SaveCapture(ctx context.Context, c Capture) error {
	header, err := gz(c.Header)
	if err != nil {
		return err
	}
	var body []byte
	if c.Body != nil {
		if body, err = gz(c.Body); err != nil {
			return err
		}
	}
	truncated := 0
	if c.Truncated {
		truncated = 1
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		err := s.exec(ctx, tx, `INSERT INTO captures (queue_id, message_id, server, at, subject, header, body, size, truncated) VALUES (?, nullif(?, ''), nullif(?, ''), ?, nullif(?, ''), ?, ?, ?, ?)`,
			c.QueueID, strings.Trim(c.MessageID, "<>"), c.Server, us(c.At), subjectOf(c.Header), header, body, c.Size, truncated)
		for _, sig := range dkimSignatures(c.Header) {
			if err == nil {
				err = s.saveSelector(ctx, tx, sig[0], sig[1], c.At)
			}
		}
		return err
	})
}

// CaptureFor finds the capture of a message: the same queue ID within an hour of it arriving, preferring a
// matching Message-ID.
func (s *Store) CaptureFor(ctx context.Context, m *Message) (*Capture, error) {
	if m.QueueID == "" {
		return nil, nil
	}
	var c Capture
	var at int64
	var header, body []byte
	var truncated int
	err := s.row(ctx, s.db, `SELECT queue_id, coalesce(message_id, ''), coalesce(server, ''), at, header, body, size, truncated FROM captures
		WHERE queue_id = ? AND at BETWEEN ? AND ?
		ORDER BY CASE WHEN message_id = ? THEN 0 ELSE 1 END, abs(at - ?) LIMIT 1`,
		m.QueueID, us(m.ReceivedAt.Add(-time.Hour)), us(m.ReceivedAt.Add(time.Hour)), m.MessageID, us(m.ReceivedAt)).
		Scan(&c.QueueID, &c.MessageID, &c.Server, &at, &header, &body, &c.Size, &truncated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.At, c.Truncated = fromUS(at), truncated == 1
	if c.Header, err = gunzip(header); err != nil {
		return nil, err
	}
	if body != nil {
		if c.Body, err = gunzip(body); err != nil {
			return nil, err
		}
	}
	return &c, nil
}

func headerOf(raw []byte) textproto.MIMEHeader {
	h, _ := textproto.NewReader(bufio.NewReader(bytes.NewReader(raw))).ReadMIMEHeader()
	return h
}

// subjectOf is the captured Subject, with encoded words (=?utf-8?q?...?=) decoded.
func subjectOf(raw []byte) string {
	s := headerOf(raw).Get("Subject")
	if decoded, err := new(mime.WordDecoder).DecodeHeader(s); err == nil {
		return decoded
	}
	return s
}

// dkimSignatures returns the domain and selector of each DKIM-Signature header.
func dkimSignatures(header []byte) [][2]string {
	h := headerOf(header)
	var out [][2]string
	for _, sig := range h.Values("Dkim-Signature") {
		tags := map[string]string{}
		for _, part := range strings.Split(sig, ";") {
			k, v, _ := strings.Cut(part, "=")
			tags[strings.TrimSpace(k)] = strings.Join(strings.Fields(v), "")
		}
		if tags["d"] != "" && tags["s"] != "" {
			out = append(out, [2]string{tags["d"], tags["s"]})
		}
	}
	return out
}

func gz(b []byte) ([]byte, error) {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(b); err != nil {
		return nil, err
	}
	err := w.Close()
	return buf.Bytes(), err
}

func gunzip(b []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

type AlertState struct {
	Name       string     `json:"name"`
	State      string     `json:"state"` // ok, firing
	Value      float64    `json:"value"`
	Since      time.Time  `json:"since"`
	NotifiedAt *time.Time `json:"notified_at,omitempty"`
}

func (s *Store) AlertStates(ctx context.Context) (map[string]AlertState, error) {
	rows, err := s.rows(ctx, s.db, `SELECT name, state, coalesce(value, 0), since, notified_at FROM alert_states`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]AlertState{}
	for rows.Next() {
		var a AlertState
		var since int64
		var notified *int64
		if err := rows.Scan(&a.Name, &a.State, &a.Value, &since, &notified); err != nil {
			return nil, err
		}
		a.Since, a.NotifiedAt = fromUS(since), fromUSp(notified)
		out[a.Name] = a
	}
	return out, rows.Err()
}

// SaveAlert records an alert's state, and an event when the state changed.
func (s *Store) SaveAlert(ctx context.Context, a AlertState, changed bool) error {
	var notified *int64
	if a.NotifiedAt != nil {
		v := us(*a.NotifiedAt)
		notified = &v
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		err := s.exec(ctx, tx, `INSERT INTO alert_states (name, state, value, since, notified_at) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (name) DO UPDATE SET state = excluded.state, value = excluded.value, since = excluded.since, notified_at = excluded.notified_at`,
			a.Name, a.State, a.Value, us(a.Since), notified)
		if err == nil && changed {
			err = s.exec(ctx, tx, `INSERT INTO alert_events (name, state, value, at) VALUES (?, ?, ?, ?)`, a.Name, a.State, a.Value, us(a.Since))
		}
		return err
	})
}

type AlertEvent struct {
	Name  string    `json:"name"`
	State string    `json:"state"`
	Value float64   `json:"value"`
	At    time.Time `json:"at"`
}

func (s *Store) AlertEvents(ctx context.Context, limit int) ([]AlertEvent, error) {
	rows, err := s.rows(ctx, s.db, `SELECT name, state, coalesce(value, 0), at FROM alert_events ORDER BY at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AlertEvent
	for rows.Next() {
		var e AlertEvent
		var at int64
		if err := rows.Scan(&e.Name, &e.State, &e.Value, &at); err != nil {
			return nil, err
		}
		e.At = fromUS(at)
		out = append(out, e)
	}
	return out, rows.Err()
}
