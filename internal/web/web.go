// Package web serves the dashboard, the REST API, Prometheus metrics and a health check.
package web

import (
	"crypto/sha256"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/SamJUK/mailinois/internal/alerts"
	"github.com/SamJUK/mailinois/internal/auth"
	"github.com/SamJUK/mailinois/internal/capture"
	"github.com/SamJUK/mailinois/internal/config"
	"github.com/SamJUK/mailinois/internal/ingest"
	"github.com/SamJUK/mailinois/internal/postfix"
	"github.com/SamJUK/mailinois/internal/store"
)

//go:embed templates static
var files embed.FS

type Server struct {
	Store    *store.Store
	Config   *config.Config
	Auth     *auth.Auth
	Pipeline *ingest.Pipeline // nil when nothing is being ingested
	// IngestWait is how long an ingest request waits for room before answering 503 (0: 10 seconds).
	IngestWait time.Duration
	Alerts     *alerts.Evaluator
	Version    string

	// LookupTXT is for DKIM verification of captures; nil uses the system resolver.
	LookupTXT func(domain string) ([]string, error)

	pages map[string]*template.Template
}

var periods = []struct {
	Key        string
	Span, Step time.Duration
}{
	{"1h", time.Hour, 5 * time.Minute},
	{"24h", 24 * time.Hour, time.Hour},
	{"7d", 7 * 24 * time.Hour, 6 * time.Hour},
	{"30d", 30 * 24 * time.Hour, 24 * time.Hour},
}

func (s *Server) lifetime() time.Duration { return s.Config.QueueLifetime.D() }

func (s *Server) Handler() http.Handler {
	s.pages = map[string]*template.Template{}
	for _, name := range []string{"dashboard", "messages", "message", "queue", "providers", "domains", "servers", "alerts", "error"} {
		s.pages[name] = template.Must(template.New("layout.html").Funcs(funcs).ParseFS(files, "templates/layout.html", "templates/"+name+".html"))
	}
	s.pages["login"] = template.Must(template.New("login.html").Funcs(funcs).ParseFS(files, "templates/login.html"))
	static, _ := fs.Sub(files, "static")

	mux := http.NewServeMux()
	page := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.Auth.Page(h)) }
	page("GET /{$}", s.dashboard)
	page("GET /messages", s.messages)
	page("GET /messages/{id}", s.message)
	page("GET /messages/{id}/html", s.messageHTML)
	page("GET /messages/{id}/eml", s.messageEML)
	page("GET /queue", s.queue)
	page("GET /providers", s.providers)
	page("GET /domains", s.domains)
	page("GET /servers", s.servers)
	page("GET /alerts", s.alertsPage)
	mux.HandleFunc("GET /login", s.login)
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("POST /logout", s.logout)
	mux.HandleFunc("GET /auth/login", s.ssoStart)
	mux.HandleFunc("GET /auth/callback", s.ssoCallback)
	mux.Handle("GET /metrics", s.Auth.API("read", http.HandlerFunc(s.metrics)))
	s.api(mux)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Store.Ping(r.Context()); err != nil {
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	return secure(mux)
}

// secure sets the headers every response carries, and refuses cross-origin POSTs (the CSRF check: browsers say
// where a form came from in Sec-Fetch-Site or Origin; agents and API clients send neither, and pass).
func secure(next http.Handler) http.Handler {
	next = http.NewCrossOriginProtection().Handler(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; frame-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Strict-Transport-Security", "max-age=31536000") // browsers ignore it over plain HTTP
		next.ServeHTTP(w, r)
	})
}

type option struct{ Value, Name string }

// page is what every template gets: who is looking, the pickers, links that keep them, and the page's data.
type page struct {
	Title, Nav, Path string
	Controls         bool // the site picker applies
	Periodic         bool // and so do the server and period pickers
	Period           string
	Server           string
	Site             string
	SiteName         string
	Q                string
	Servers          []string
	Sites            []option
	Viewer           *auth.Viewer
	AuthMode         string
	QueueCount       int
	Firing           int
	Live             int
	Version          string
	Query            url.Values
	Data             any
}

// Link is path with the current query, overridden by key, value pairs ("" removes a key).
func (p page) Link(path string, kv ...string) string {
	q := url.Values{}
	for k, v := range p.Query {
		q[k] = slices.Clone(v)
	}
	q.Del("offset")
	q.Del("tab")
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] == "" {
			q.Del(kv[i])
		} else {
			q.Set(kv[i], kv[i+1])
		}
	}
	if len(q) == 0 {
		return path
	}
	return path + "?" + q.Encode()
}

func (s *Server) newPage(r *http.Request, nav, title string) (page, store.Filter) {
	v := auth.From(r.Context())
	q := r.URL.Query()
	p := page{Title: title, Nav: nav, Path: r.URL.Path, Controls: true, Periodic: nav != "domains" && nav != "servers", Viewer: v, AuthMode: s.Auth.Mode(), Version: s.Version, Query: url.Values{}}
	p.Period = "24h"
	for _, x := range periods {
		if x.Key == q.Get("period") {
			p.Period = x.Key
		}
	}
	if p.Period != "24h" {
		p.Query.Set("period", p.Period)
	}
	for _, k := range []string{"server", "site"} {
		if val := q.Get(k); val != "" {
			p.Query.Set(k, val)
		}
	}
	p.Server, p.Site, p.Q = q.Get("server"), q.Get("site"), strings.TrimSpace(q.Get("q"))
	ctx := r.Context()
	p.Servers, _ = s.Store.ServerNames(ctx)

	f := s.filter(v, p.Period, p.Server, p.Site, p.Q, time.Now())
	if len(s.Config.Sites) > 0 {
		for _, site := range s.Config.Sites {
			if v.Sites == nil || slices.Contains(v.Sites, site.Slug) {
				p.Sites = append(p.Sites, option{site.Slug, site.Name})
			}
		}
	} else if v.Sites == nil {
		domains, _ := s.Store.SenderDomains(ctx, time.Now().Add(-7*24*time.Hour), 15)
		for _, d := range domains {
			p.Sites = append(p.Sites, option{"@" + d, d})
		}
	}
	for _, o := range p.Sites {
		if o.Value == p.Site {
			p.SiteName = o.Name
		}
	}
	if p.Site != "" && p.SiteName == "" {
		p.SiteName = strings.TrimPrefix(p.Site, "@")
	}

	scope := store.Filter{Allowed: f.Allowed, Sites: f.Sites, Server: f.Server}
	p.QueueCount, _, _ = s.Store.QueueNow(ctx, scope, s.lifetime())
	if s.Alerts != nil {
		rules, _ := s.Alerts.Rules(ctx)
		for _, rule := range rules {
			if rule.State.State == "firing" {
				p.Firing++
			}
		}
	}
	last, _ := s.Store.LastLine(ctx)
	for _, at := range last {
		if time.Since(at) < 15*time.Minute {
			p.Live++
		}
	}
	return p, f
}

// filter turns the pickers and search, and what the viewer may see, into a store filter.
func (s *Server) filter(v *auth.Viewer, period, server, site, q string, now time.Time) store.Filter {
	f := store.Filter{Server: server}
	for _, x := range periods {
		if x.Key == period {
			f.Until = now.Truncate(x.Step).Add(x.Step)
			f.Since = f.Until.Add(-x.Span)
		}
	}
	if v != nil && v.Sites != nil {
		f.Allowed = []store.Scope{}
		for _, slug := range v.Sites {
			if c, ok := s.Config.Site(slug); ok {
				f.Allowed = append(f.Allowed, scopeOf(c))
			}
		}
	}
	if site != "" {
		f.Sites = []store.Scope{s.scope(site)}
	}
	if q != "" {
		search := store.ParseSearch(q, now)
		for i, t := range search.Terms {
			if t.Field == "site" {
				sc := s.scope(t.Value)
				search.Terms[i].Site = &sc
			}
		}
		f.Terms = search.Terms
		if search.After != nil {
			f.Since = *search.After
		}
		if search.Before != nil {
			f.Until = *search.Before
		}
	}
	return f
}

// scope is a configured site by slug, or @domain for a sending domain; anything else matches nothing.
func (s *Server) scope(site string) store.Scope {
	if d, ok := strings.CutPrefix(site, "@"); ok {
		return store.Scope{Domains: []string{d}}
	}
	if c, ok := s.Config.Site(site); ok {
		return scopeOf(c)
	}
	return store.Scope{}
}

func scopeOf(c config.Site) store.Scope {
	return store.Scope{Domains: c.Domains, Senders: c.Senders, Servers: c.Servers}
}

func step(period string) time.Duration {
	for _, x := range periods {
		if x.Key == period {
			return x.Step
		}
	}
	return time.Hour
}

func (s *Server) render(w http.ResponseWriter, name string, p any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.pages[name].Execute(w, p); err != nil {
		slog.Error("render", "page", name, "err", err)
	}
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusInternalServerError
	msg := "Something went wrong reading the database. The details are in Mailinois's log."
	if errors.Is(err, sql.ErrNoRows) {
		status, msg = http.StatusNotFound, "Not found. It may have been removed by retention, or it belongs to a site you can't see."
	} else {
		slog.Error("request", "path", r.URL.Path, "err", err)
	}
	w.WriteHeader(status)
	s.render(w, "error", page{Title: http.StatusText(status), Version: s.Version, AuthMode: s.Auth.Mode(), Viewer: auth.From(r.Context()), Data: msg})
}

// localPath keeps a next= redirect on this site.
func localPath(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/" // browsers read /\host as //host
	}
	return next
}

func (s *Server) loginData(next string) map[string]any {
	sso, label := s.Auth.SSO()
	if !sso {
		label = ""
	}
	return map[string]any{"Next": next, "Error": "", "Name": "", "Version": s.Version, "SSO": label, "Passwords": s.Auth.Passwords()}
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if mode := s.Auth.Mode(); mode != "password" && mode != "oidc" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	next := localPath(r.FormValue("next"))
	data := s.loginData(next)
	if r.Method == http.MethodPost && !s.Auth.Passwords() {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if r.Method == http.MethodPost {
		name, client := r.PostFormValue("name"), s.Auth.Client(r)
		switch {
		case s.Auth.Blocked(client):
			data["Error"], data["Name"] = "Too many failed sign-ins lately. Try again in 15 minutes.", name
			w.WriteHeader(http.StatusTooManyRequests)
		case s.Auth.Login(name, r.PostFormValue("password")):
			s.Auth.SetSession(w, r, name)
			http.Redirect(w, r, next, http.StatusSeeOther)
			return
		default:
			s.Auth.Failed(client)
			slog.Warn("failed sign-in", "name", name, "client", client)
			data["Error"], data["Name"] = "That name and password don't match.", name
			w.WriteHeader(http.StatusUnauthorized)
		}
	}
	s.render(w, "login", data)
}

// ssoStart sends the browser to the OIDC provider.
func (s *Server) ssoStart(w http.ResponseWriter, r *http.Request) {
	if sso, _ := s.Auth.SSO(); !sso {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	next := localPath(r.FormValue("next"))
	to, err := s.Auth.StartSSO(w, r, next)
	if err != nil {
		slog.Error("OIDC sign-in", "err", err)
		data := s.loginData(next)
		data["Error"] = "Mailinois can't reach the sign-in provider. Try again shortly."
		w.WriteHeader(http.StatusBadGateway)
		s.render(w, "login", data)
		return
	}
	http.Redirect(w, r, to, http.StatusFound)
}

// ssoCallback is where the provider sends the browser back. Failures count towards the sign-in limits.
func (s *Server) ssoCallback(w http.ResponseWriter, r *http.Request) {
	if sso, _ := s.Auth.SSO(); !sso {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	client := s.Auth.Client(r)
	data := s.loginData("/")
	if s.Auth.Blocked(client) {
		data["Error"] = "Too many failed sign-ins lately. Try again in 15 minutes."
		w.WriteHeader(http.StatusTooManyRequests)
		s.render(w, "login", data)
		return
	}
	next, err := s.Auth.FinishSSO(w, r)
	if err != nil {
		e := auth.AsSSOError(err)
		s.Auth.Failed(client)
		slog.Warn("failed OIDC sign-in", "reason", e.Reason, "client", client)
		data["Error"] = e.Message
		w.WriteHeader(e.Status)
		s.render(w, "login", data)
		return
	}
	http.Redirect(w, r, localPath(next), http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	s.Auth.ClearSession(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

type dashboardData struct {
	store.Dashboard
	CheckEvery time.Duration
	Chart      chart
	Tail       []store.TailLine
	TailURL    string
	Reasons    []store.Reason
	IPs        []store.SendingIP
	Domains    []domainHealth
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	p, f := s.newPage(r, "dashboard", "Overview")
	ctx := r.Context()
	d, err := s.Store.Dashboard(ctx, f, step(p.Period), s.lifetime())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	data := dashboardData{Dashboard: d, CheckEvery: s.Config.Checks.Interval.D(), Chart: newChart(d.Buckets, step(p.Period))}
	data.Tail, _ = s.Store.Tail(ctx, f, 0, 12)
	data.TailURL = p.Link("/api/v1/tail")
	if reasons, err := s.Store.DeferralReasons(ctx, f); err == nil && len(reasons) > 5 {
		data.Reasons = reasons[:5]
	} else {
		data.Reasons = reasons
	}
	data.IPs, _ = s.Store.SendingIPs(ctx, s.Config.Checks.SendingIPs)
	data.Domains = s.health(r, f)
	p.Data = data
	s.render(w, "dashboard", p)
}

type messagesData struct {
	Rows       []store.MessageRow
	Statuses   []string
	Providers  []string
	Prev, Next string
}

const pageSize = 100

func (s *Server) messages(w http.ResponseWriter, r *http.Request) {
	p, f := s.newPage(r, "messages", "Messages")
	if p.Q != "" {
		p.Query.Set("q", p.Q)
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	offset = max(offset, 0)
	rows, err := s.Store.Messages(r.Context(), f, offset, pageSize+1)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	d := messagesData{Statuses: []string{"queued", "deferred", "sent", "bounced", "expired", "deleted", "cancelled", "rejected"}, Providers: postfix.Providers}
	if len(rows) > pageSize {
		rows = rows[:pageSize]
		d.Next = p.Link("/messages", "offset", strconv.Itoa(offset+pageSize))
	}
	if offset > 0 {
		d.Prev = p.Link("/messages", "offset", strconv.Itoa(max(offset-pageSize, 0)))
	}
	d.Rows = rows
	p.Data = d
	s.render(w, "messages", p)
}

type messageData struct {
	*store.Message
	Tab       string
	Summary   string
	Capture   *store.Capture
	Parsed    capture.Parsed
	DKIM      []capture.DKIMResult
	SenderDNS []store.DNSResult
}

func (s *Server) loadMessage(r *http.Request) (*store.Message, *store.Capture, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return nil, nil, sql.ErrNoRows
	}
	f := s.filter(auth.From(r.Context()), "", "", "", "", time.Now())
	m, err := s.Store.Message(r.Context(), f, id)
	if err != nil {
		return nil, nil, err
	}
	c, err := s.Store.CaptureFor(r.Context(), m)
	return m, c, err
}

func toCapture(c *store.Capture) capture.Message {
	return capture.Message{QueueID: c.QueueID, MessageID: c.MessageID, Server: c.Server, At: c.At, Header: c.Header, Body: c.Body, Size: c.Size, Truncated: c.Truncated}
}

func (s *Server) message(w http.ResponseWriter, r *http.Request) {
	p, _ := s.newPage(r, "messages", "Message")
	p.Controls = false
	m, c, err := s.loadMessage(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	d := messageData{Message: m, Tab: r.URL.Query().Get("tab"), Summary: summary(m), Capture: c}
	if !slices.Contains([]string{"timeline", "headers", "body", "log"}, d.Tab) {
		d.Tab = "timeline"
	}
	if c != nil {
		d.Parsed = capture.Parse(toCapture(c))
		if c.Body != nil {
			d.DKIM = capture.VerifyDKIM(toCapture(c), s.LookupTXT)
		}
	}
	for _, h := range s.health(r, store.Filter{}) {
		if h.Domain == m.SenderDomain {
			for _, k := range h.Kinds {
				if k.Kind != "dkim" { // the capture's own signature says more than the key's existence
					d.SenderDNS = append(d.SenderDNS, k)
				}
			}
		}
	}
	p.Title = cmp(m.Subject, cmp(m.QueueID, "Message"))
	p.Data = d
	s.render(w, "message", p)
}

// summary says in a sentence how a message got on.
func summary(m *store.Message) string {
	if m.Status == "cancelled" {
		return "SMTP transaction cancelled before Postfix accepted the message."
	}
	if m.QueueID == "" {
		return "Refused before Postfix accepted it."
	}
	n, sent := len(m.Recipients), 0
	var waiting, failed []string
	for _, r := range m.Recipients {
		switch r.Status {
		case "sent":
			sent++
		case "deferred", "queued":
			waiting = append(waiting, r.Address)
		case "bounced", "expired", "rejected", "deleted":
			failed = append(failed, r.Address)
		}
	}
	switch {
	case n == 0 && m.RemovedAt == nil:
		return "Accepted, waiting for its first delivery attempt."
	case n == 0:
		return "No delivery was logged for it."
	case sent == n && n == 1:
		return "Delivered."
	case sent == n:
		return fmt.Sprintf("Delivered to all %d recipients.", n)
	}
	out := fmt.Sprintf("%d of %d delivered.", sent, n)
	if len(waiting) > 0 {
		out += fmt.Sprintf(" Still trying %s.", strings.Join(waiting, ", "))
	}
	if len(failed) > 0 {
		out += fmt.Sprintf(" Failed for %s.", strings.Join(failed, ", "))
	}
	return out
}

func cmp(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// messageHTML serves a capture's HTML part to a sandboxed frame: no scripts, no remote content, its own origin.
func (s *Server) messageHTML(w http.ResponseWriter, r *http.Request) {
	_, c, err := s.loadMessage(r)
	if err != nil || c == nil {
		s.fail(w, r, cmpErr(err))
		return
	}
	h := w.Header()
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'; style-src 'unsafe-inline'; img-src data:; frame-ancestors 'self'")
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	fmt.Fprint(w, capture.Parse(toCapture(c)).HTML)
}

func (s *Server) messageEML(w http.ResponseWriter, r *http.Request) {
	m, c, err := s.loadMessage(r)
	if err != nil || c == nil || c.Body == nil {
		s.fail(w, r, cmpErr(err))
		return
	}
	w.Header().Set("Content-Type", "message/rfc822")
	w.Header().Set("Content-Disposition", `attachment; filename="`+cmp(m.QueueID, "message")+`.eml"`)
	w.Write(c.Header)
	w.Write(c.Body)
}

func cmpErr(err error) error {
	if err != nil {
		return err
	}
	return sql.ErrNoRows
}

type queueData struct {
	Queued   []store.QueuedMessage
	Reasons  []store.Reason
	Failures []store.Failure
	Lifetime time.Duration
}

func (s *Server) queue(w http.ResponseWriter, r *http.Request) {
	p, f := s.newPage(r, "queue", "Queue and failures")
	d := queueData{Lifetime: s.lifetime()}
	var err error
	if d.Queued, err = s.Store.Queue(r.Context(), f, s.lifetime(), 200); err == nil {
		if d.Reasons, err = s.Store.DeferralReasons(r.Context(), f); err == nil {
			d.Failures, err = s.Store.Failures(r.Context(), f, 50)
		}
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	p.Data = d
	s.render(w, "queue", p)
}

func (s *Server) providers(w http.ResponseWriter, r *http.Request) {
	p, f := s.newPage(r, "providers", "Providers")
	stats, err := s.Store.Providers(r.Context(), f)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	p.Data = stats
	s.render(w, "providers", p)
}

// domainHealth is one sending domain's DNS results, and the worst result of each kind.
type domainHealth struct {
	Domain    string
	Worst     string
	CheckedAt time.Time
	Results   []store.DNSResult
	Kinds     []store.DNSResult // spf, dkim, dmarc: the worst of each
}

func (d domainHealth) Check(kind string) *store.DNSResult {
	for i := range d.Kinds {
		if d.Kinds[i].Kind == kind {
			return &d.Kinds[i]
		}
	}
	return nil
}

var severity = map[string]int{"pass": 0, "warn": 1, "missing": 2, "error": 2, "fail": 3}

// health groups the latest DNS results by domain, keeping the domains the viewer and the site picker allow.
func (s *Server) health(r *http.Request, f store.Filter) []domainHealth {
	results, err := s.Store.DNSResults(r.Context())
	if err != nil {
		return nil
	}
	allowed := func(d string) bool {
		for _, scopes := range [][]store.Scope{f.Allowed, f.Sites} {
			if scopes == nil {
				continue
			}
			ok := false
			for _, sc := range scopes {
				domains := append([]string{}, sc.Domains...)
				for _, sender := range sc.Senders {
					domains = append(domains, postfix.Domain(sender))
				}
				for _, x := range domains {
					ok = ok || d == x || strings.HasSuffix(d, "."+x)
				}
			}
			if !ok {
				return false
			}
		}
		return true
	}
	var out []domainHealth
	for _, res := range results {
		if !allowed(res.Domain) {
			continue
		}
		if len(out) == 0 || out[len(out)-1].Domain != res.Domain {
			out = append(out, domainHealth{Domain: res.Domain, Worst: "pass"})
		}
		h := &out[len(out)-1]
		h.Results = append(h.Results, res)
		if res.CheckedAt.After(h.CheckedAt) {
			h.CheckedAt = res.CheckedAt
		}
		if severity[res.Status] > severity[h.Worst] {
			h.Worst = res.Status
		}
		i := slices.IndexFunc(h.Kinds, func(k store.DNSResult) bool { return k.Kind == res.Kind })
		switch {
		case i < 0:
			h.Kinds = append(h.Kinds, res)
		case severity[res.Status] > severity[h.Kinds[i].Status]:
			h.Kinds[i] = res
		}
	}
	for i := range out {
		slices.SortFunc(out[i].Kinds, func(a, b store.DNSResult) int { return strings.Compare(kindOrder[a.Kind], kindOrder[b.Kind]) })
	}
	return out
}

var kindOrder = map[string]string{"spf": "1", "dkim": "2", "dmarc": "3"}

func (s *Server) domains(w http.ResponseWriter, r *http.Request) {
	p, f := s.newPage(r, "domains", "Domains")
	p.Data = s.health(r, f)
	s.render(w, "domains", p)
}

type serversData struct {
	Servers []store.Server
	IPs     []store.SendingIP
	Changes []store.Check
}

func (s *Server) servers(w http.ResponseWriter, r *http.Request) {
	p, f := s.newPage(r, "servers", "Servers and sending IPs")
	var d serversData
	var err error
	if d.Servers, err = s.Store.Servers(r.Context(), f, s.lifetime()); err == nil {
		if d.IPs, err = s.Store.SendingIPs(r.Context(), s.Config.Checks.SendingIPs); err == nil {
			d.Changes, err = s.Store.ListingChanges(r.Context(), 50)
		}
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	p.Data = d
	s.render(w, "servers", p)
}

type alertsData struct {
	Rules    []alerts.Rule
	Events   []store.AlertEvent
	Webhooks []config.Webhook
}

func (s *Server) alertsPage(w http.ResponseWriter, r *http.Request) {
	p, _ := s.newPage(r, "alerts", "Alerts")
	p.Controls = false
	var d alertsData
	var err error
	if s.Alerts != nil {
		if d.Rules, err = s.Alerts.Rules(r.Context()); err == nil {
			d.Events, err = s.Store.AlertEvents(r.Context(), 50)
		}
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	d.Webhooks = s.Config.Webhooks
	p.Data = d
	s.render(w, "alerts", p)
}

// chart is a stacked bar chart, laid out here so the template only draws it.
type chart struct {
	W, H   float64
	Bars   []bar
	Max    int
	Labels []label
}

type bar struct {
	X, W  float64
	Segs  []seg
	Title string
}

type seg struct {
	Y, H  float64
	Class string
}

type label struct {
	X    float64
	Text string
}

func newChart(buckets []store.Bucket, step time.Duration) chart {
	c := chart{W: 960, H: 200}
	for _, b := range buckets {
		c.Max = max(c.Max, b.Sent+b.Deferred+b.Bounced+b.Rejected)
	}
	if len(buckets) == 0 {
		return c
	}
	scale := c.H / float64(max(c.Max, 1))
	slot := c.W / float64(len(buckets))
	format := "15:04"
	if step >= 24*time.Hour {
		format = "2 Jan"
	} else if step > time.Hour {
		format = "Mon 15:04"
	}
	every := int(math.Ceil(float64(len(buckets)) / 8))
	for i, b := range buckets {
		x := float64(i) * slot
		br := bar{X: x + slot*0.1, W: slot * 0.8, Title: fmt.Sprintf("%s: %d sent, %d deferred, %d bounced, %d rejected",
			b.At.Local().Format("Mon 2 Jan 15:04"), b.Sent, b.Deferred, b.Bounced, b.Rejected)}
		y := c.H
		for _, sg := range []struct {
			n     int
			class string
		}{{b.Sent, "sent"}, {b.Deferred, "deferred"}, {b.Bounced, "bounced"}, {b.Rejected, "rejected"}} {
			if sg.n == 0 {
				continue
			}
			h := float64(sg.n) * scale
			y -= h
			br.Segs = append(br.Segs, seg{Y: y, H: h, Class: sg.class})
		}
		c.Bars = append(c.Bars, br)
		if i%every == 0 {
			c.Labels = append(c.Labels, label{X: x + slot/2, Text: b.At.Local().Format(format)})
		}
	}
	return c
}

var funcs = template.FuncMap{
	"asset": func(name string) (string, error) {
		content, err := files.ReadFile("static/" + name)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("/static/%s?v=%x", name, sha256.Sum256(content)), nil
	},
	"ago": func(t time.Time) string {
		if s := time.Since(t).Seconds(); s >= 10 {
			return duration(s) + " ago"
		}
		return "just now"
	},
	"age": func(t *time.Time) string {
		if t == nil {
			return "–"
		}
		return duration(time.Since(*t).Seconds())
	},
	"when":  func(t time.Time) string { return t.Local().Format("2 Jan 15:04:05") },
	"clock": func(t time.Time) string { return t.Local().Format("15:04:05") },
	"whenp": func(t *time.Time) string {
		if t == nil {
			return "–"
		}
		return t.Local().Format("2 Jan 15:04:05")
	},
	"secs": func(f *float64) string {
		if f == nil {
			return "–"
		}
		return duration(*f)
	},
	"pct":      pct,
	"num":      num,
	"bytes":    humanBytes,
	"provider": providerName,
	"rate":     rate,
	"change":   change,
	"sender": func(s string, known ...bool) string {
		if len(known) > 0 && !known[0] {
			return "(sender not logged)"
		}
		if s == "" {
			return "<> (bounce)"
		}
		return s
	},
	"dur":     func(d time.Duration) string { return duration(d.Seconds()) },
	"add":     func(a, b int) int { return a + b },
	"addf":    func(a, b float64) float64 { return a + b },
	"deref":   func(t *time.Time) time.Time { return *t },
	"periods": func() []string { return []string{"1h", "24h", "7d", "30d"} },
	"zone":    func() string { name, _ := time.Now().Zone(); return name },
	"metric":  alerts.Format,
	"host": func(raw string) string {
		u, err := url.Parse(raw)
		if err != nil {
			return ""
		}
		return u.Host
	},
	"severity": func(s string) string {
		return map[string]string{"pass": "sent", "warn": "deferred", "missing": "bounced", "error": "bounced", "fail": "bounced"}[s]
	},
	"threshold": func(t config.Threshold) float64 { return float64(t) },
}

func duration(s float64) string {
	switch {
	case s < 0:
		return "0s"
	case s < 10:
		return strconv.FormatFloat(s, 'f', 2, 64) + "s"
	case s < 60:
		return fmt.Sprintf("%.0fs", s)
	case s < 3600:
		return fmt.Sprintf("%dm %ds", int(s)/60, int(s)%60)
	case s < 86400:
		return fmt.Sprintf("%dh %dm", int(s)/3600, int(s)%3600/60)
	}
	return fmt.Sprintf("%dd %dh", int(s)/86400, int(s)%86400/3600)
}

func rate(n, d int) float64 {
	if d == 0 {
		return math.NaN()
	}
	return float64(n) / float64(d)
}

func pct(f float64) string {
	if math.IsNaN(f) {
		return "–"
	}
	if f > 0 && f < 0.001 {
		return "<0.1%"
	}
	return strconv.FormatFloat(f*100, 'f', 1, 64) + "%"
}

type delta struct {
	Text, Class string
}

// change is the move in percentage points from prev to cur. higherIsBad colours a rise red.
func change(cur, prev float64, higherIsBad bool) delta {
	if math.IsNaN(cur) || math.IsNaN(prev) {
		return delta{}
	}
	pp := (cur - prev) * 100
	if math.Abs(pp) < 0.05 {
		return delta{"±0", "flat"}
	}
	d := delta{Text: fmt.Sprintf("%+.1fpp", pp), Class: "good"}
	if (pp > 0) == higherIsBad {
		d.Class = "bad"
	}
	return d
}

func num(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0 && s[i-1] != '-'; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func humanBytes(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
}

func providerName(key string) string {
	switch key {
	case "google":
		return "Google"
	case "microsoft":
		return "Microsoft"
	case "yahoo":
		return "Yahoo"
	case "apple":
		return "Apple"
	}
	return "Other"
}
