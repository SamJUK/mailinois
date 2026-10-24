package store

import (
	"strconv"
	"strings"
	"time"
)

// Scope is a site: messages sent from these domains (or their subdomains), or through these servers.
type Scope struct {
	Domains []string
	Senders []string
	Servers []string
}

// Filter narrows every read. Times are the period; the rest applies to messages.
type Filter struct {
	Since, Until time.Time
	Server       string  // the server picker
	Sites        []Scope // the site picker: any of these; nil is everything
	Allowed      []Scope // what the viewer may see: any of these; nil is everything
	Terms        []Term  // the search
}

// Term is one piece of a search: a free word (Field ""), or field:value, negated with a leading -.
type Term struct {
	Field string
	Value string
	Not   bool
	Site  *Scope // site: terms, resolved by the caller; nil matches nothing
}

var fields = map[string]bool{"from": true, "to": true, "domain": true, "status": true, "provider": true, "server": true,
	"site": true, "dsn": true, "qid": true, "msgid": true, "subject": true, "client": true, "relay": true, "has": true}

// Search is a parsed search box: its terms, and the period it sets with after: and before:.
type Search struct {
	Terms         []Term
	After, Before *time.Time
}

// ParseSearch reads `from:store-a.example -status:sent subject:"order confirmed" bob`. Unknown fields are words.
func ParseSearch(q string, now time.Time) Search {
	var s Search
	for _, tok := range tokens(q) {
		t := Term{}
		if strings.HasPrefix(tok, "-") && len(tok) > 1 {
			t.Not, tok = true, tok[1:]
		}
		field, value, ok := strings.Cut(tok, ":")
		field = strings.ToLower(field)
		switch {
		case ok && (field == "after" || field == "before"):
			if at, ok := parseWhen(value, now); ok {
				if field == "after" {
					s.After = &at
				} else {
					s.Before = &at
				}
			}
			continue
		case ok && fields[field] && value != "":
			t.Field, t.Value = field, strings.Trim(value, `"`)
		default:
			t.Value = strings.Trim(tok, `"`)
		}
		if t.Value != "" {
			s.Terms = append(s.Terms, t)
		}
	}
	return s
}

// tokens splits on spaces outside double quotes.
func tokens(q string) []string {
	var out []string
	var cur strings.Builder
	quoted := false
	for _, r := range q {
		switch {
		case r == '"':
			quoted = !quoted
			cur.WriteRune(r)
		case (r == ' ' || r == '\t') && !quoted:
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// parseWhen reads 2026-10-18, an RFC 3339 time, or an age: 30m, 6h, 2d, 1w.
func parseWhen(v string, now time.Time) (time.Time, bool) {
	if t, err := time.ParseInLocation("2006-01-02", v, time.Local); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, true
	}
	if len(v) < 2 {
		return time.Time{}, false
	}
	n, err := strconv.Atoi(v[:len(v)-1])
	unit := map[byte]time.Duration{'m': time.Minute, 'h': time.Hour, 'd': 24 * time.Hour, 'w': 7 * 24 * time.Hour}[v[len(v)-1]]
	if err != nil || unit == 0 {
		return time.Time{}, false
	}
	return now.Add(-time.Duration(n) * unit), true
}

// cond collects SQL conditions on messages m, joined with AND.
type cond struct {
	parts []string
	args  []any
}

func (c *cond) add(sql string, args ...any) {
	c.parts = append(c.parts, sql)
	c.args = append(c.args, args...)
}

// from is table (recipients r or events e), joined to its messages m when a condition needs them. With none,
// counts read the table's own index and never visit a message.
func (c *cond) from(table string) string {
	if len(c.parts) == 0 {
		return table
	}
	alias := table[strings.LastIndexByte(table, ' ')+1:]
	return table + " JOIN messages m ON m.id = " + alias + ".message_id"
}

func (c *cond) sql() string {
	if len(c.parts) == 0 {
		return "1 = 1"
	}
	return strings.Join(c.parts, " AND ")
}

// match is the filter's conditions on messages m, apart from the period.
func (f Filter) match() *cond {
	c := &cond{}
	if f.Server != "" {
		c.add(`m.server_id = (SELECT id FROM servers WHERE name = ?)`, f.Server)
	}
	if f.Sites != nil {
		anyScope(c, f.Sites)
	}
	if f.Allowed != nil {
		anyScope(c, f.Allowed)
	}
	for _, t := range f.Terms {
		sql, args := t.sql()
		if t.Not {
			sql = "NOT (" + sql + ")"
		}
		c.add(sql, args...)
	}
	return c
}

func anyScope(c *cond, scopes []Scope) {
	var parts []string
	var args []any
	for _, s := range scopes {
		sql, a := s.sql()
		parts = append(parts, sql)
		args = append(args, a...)
	}
	if len(parts) == 0 {
		c.add("1 = 0")
		return
	}
	c.add("("+strings.Join(parts, " OR ")+")", args...)
}

func (s Scope) sql() (string, []any) {
	var parts []string
	var args []any
	if len(s.Servers) > 0 {
		parts = append(parts, `m.server_id IN (SELECT id FROM servers WHERE name IN (`+marks(len(s.Servers))+`))`)
		for _, v := range s.Servers {
			args = append(args, v)
		}
	}
	for _, d := range s.Domains {
		sql, a := domainIs("m.sender_domain", d)
		parts = append(parts, sql)
		args = append(args, a...)
	}
	if len(s.Senders) > 0 {
		placeholders := marks(len(s.Senders))
		parts = append(parts, `(lower(m.sender) IN (`+placeholders+`) OR
            (m.sender = '' AND EXISTS (SELECT 1 FROM recipients sr WHERE sr.message_id = m.id AND lower(sr.address) IN (`+placeholders+`))))`)
		for range 2 {
			for _, sender := range s.Senders {
				args = append(args, strings.ToLower(sender))
			}
		}
	}

	if len(parts) == 0 {
		return "1 = 0", nil
	}
	return "(" + strings.Join(parts, " OR ") + ")", args
}

func marks(n int) string { return strings.TrimSuffix(strings.Repeat("?, ", n), ", ") }

// domainIs matches a domain and its subdomains.
func domainIs(col, d string) (string, []any) {
	d = strings.ToLower(strings.TrimPrefix(d, "@"))
	return "(" + col + " = ? OR " + col + ` LIKE ? ESCAPE '\')`, []any{d, "%." + escapeLike(d)}
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func contains(s string) string { return "%" + escapeLike(strings.ToLower(s)) + "%" }

const like = ` LIKE ? ESCAPE '\'`

func recipientHas(sql string) string {
	return `EXISTS (SELECT 1 FROM recipients r WHERE r.message_id = m.id AND (` + sql + `))`
}

func list(v string) []any {
	var out []any
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, strings.ToLower(s))
		}
	}
	return out
}

func (t Term) sql() (string, []any) {
	v := t.Value
	switch t.Field {
	case "from":
		if strings.Contains(v, "@") {
			return `lower(coalesce(m.sender, ''))` + like, []any{contains(v)}
		}
		// The sender's own address, so bounces (null sender) never match: sites count those by recipient.
		d := escapeLike(strings.ToLower(strings.TrimPrefix(v, "@")))
		return `(lower(coalesce(m.sender, ''))` + like + ` OR lower(coalesce(m.sender, ''))` + like + `)`, []any{"%@" + d, "%." + d}
	case "to":
		if strings.Contains(v, "@") {
			return recipientHas(`r.address` + like), []any{contains(v)}
		}
		sql, args := domainIs("r.domain", v)
		return recipientHas(sql), args
	case "domain":
		from, a1 := Term{Field: "from", Value: v}.sql()
		to, a2 := Term{Field: "to", Value: v}.sql()
		return "(" + from + " OR " + to + ")", append(a1, a2...)
	case "status":
		vs := list(v)
		return `m.status IN (` + marks(len(vs)) + `)`, vs
	case "provider":
		vs := list(v)
		return recipientHas(`r.provider IN (` + marks(len(vs)) + `)`), vs
	case "server":
		vs := list(v)
		return `m.server_id IN (SELECT id FROM servers WHERE lower(name) IN (` + marks(len(vs)) + `))`, vs
	case "site":
		if t.Site == nil {
			return "1 = 0", nil
		}
		return t.Site.sql()
	case "dsn":
		return `EXISTS (SELECT 1 FROM events e WHERE e.message_id = m.id AND e.dsn` + like + `)`, []any{escapeLike(v) + "%"}
	case "qid":
		return `m.queue_id = ?`, []any{v}
	case "msgid":
		return `m.message_id = ?`, []any{strings.Trim(v, "<>")}
	case "subject":
		return `lower(` + subjectSQL + `)` + like, []any{contains(v)}
	case "client":
		return `(lower(coalesce(m.client_host, ''))` + like + ` OR coalesce(m.client_ip, '')` + like + `)`, []any{contains(v), contains(v)}
	case "relay":
		return recipientHas(`lower(coalesce(r.relay, ''))` + like + ` OR coalesce(r.relay_ip, '')` + like), []any{contains(v), contains(v)}
	case "has":
		if strings.ToLower(v) == "capture" {
			return capturedSQL, nil
		}
		return "1 = 1", nil
	}
	return `(lower(coalesce(m.sender, ''))` + like + ` OR lower(` + subjectSQL + `)` + like +
			` OR m.queue_id = ? OR m.message_id = ? OR ` + recipientHas(`r.address`+like) + `)`,
		[]any{contains(v), contains(v), v, strings.Trim(v, "<>"), contains(v)}
}

// A capture belongs to a message when the queue ID matches within an hour of it arriving.
const (
	captureOf   = `FROM captures c WHERE c.queue_id = m.queue_id AND c.at BETWEEN m.received_at - 3600000000 AND m.received_at + 3600000000`
	capturedSQL = `EXISTS (SELECT 1 ` + captureOf + `)`
	// subjectSQL is the subject Postfix logged, or else the captured one.
	subjectSQL = `coalesce(m.subject, (SELECT c.subject ` + captureOf + ` AND c.subject IS NOT NULL LIMIT 1), '')`
)
