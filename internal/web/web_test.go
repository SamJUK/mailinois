package web

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/SamJUK/mailinois/internal/alerts"
	"github.com/SamJUK/mailinois/internal/auth"
	"github.com/SamJUK/mailinois/internal/config"
	"github.com/SamJUK/mailinois/internal/ingest"
	"github.com/SamJUK/mailinois/internal/postfix"
	"github.com/SamJUK/mailinois/internal/store"
	"github.com/SamJUK/mailinois/internal/store/storetest"
	"github.com/SamJUK/mailinois/internal/syslog"
)

type fixture struct {
	srv         *httptest.Server
	st          *store.Store
	readToken   string
	ingestToken string
	server      *Server
}

func newFixture(t *testing.T, mode string) fixture {
	st := storetest.SQLite(t)
	storetest.Load(t, st, "../store/testdata/scenarios.log")
	hash, _ := bcrypt.GenerateFromPassword([]byte("s3cret password"), bcrypt.MinCost)
	read, readHash := auth.NewToken()
	ing, ingHash := auth.NewToken()
	cfg := &config.Config{
		Sites: []config.Site{{Name: "Acme", Slug: "acme", Domains: []string{"acme.test"}}, {Name: "Local", Slug: "local", Domains: []string{"example"}}},
		Auth: config.Auth{Mode: mode, Users: []config.User{{Name: "sam", PasswordHash: string(hash)}, {Name: "ann", PasswordHash: string(hash), Sites: []string{"local"}}},
			Tokens: []config.Token{{Name: "read", Hash: readHash, Scopes: []string{"read"}}, {Name: "agent", Hash: ingHash, Scopes: []string{"ingest"}}}},
		QueueLifetime: config.Duration(100 * 365 * 24 * time.Hour),
		Location:      time.UTC,
	}
	cfg.Checks.SendingIPs = []string{"203.0.113.10"}
	p := ingest.New(st, time.UTC, 0)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go p.Run(ctx)
	s := &Server{Store: st, Config: cfg, Auth: auth.New(cfg.Auth, "secret"), Pipeline: p, Version: "test",
		Alerts:    &alerts.Evaluator{Store: st, Config: cfg},
		LookupTXT: func(string) ([]string, error) { return nil, &net404{} }}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return fixture{srv, st, read, ing, s}
}

type net404 struct{}

func (*net404) Error() string { return "no such host" }

func get(t *testing.T, c *http.Client, url string, header ...string) (int, string, http.Header) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(body), res.Header
}

func noRedirects() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (f fixture) messageID(t *testing.T, queueID string) string {
	t.Helper()
	rows, err := f.st.Messages(context.Background(), store.Filter{Since: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Until: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
		Terms: []store.Term{{Field: "qid", Value: queueID}}}, 0, 1)
	if err != nil || len(rows) == 0 {
		t.Fatalf("%s: %v", queueID, err)
	}
	return strconv.FormatInt(rows[0].ID, 10)
}

func TestPages(t *testing.T) {
	f := newFixture(t, "off")
	c := http.DefaultClient
	for path, want := range map[string]string{
		"/":                                    "Live tail",
		"/?period=7d&server=relay-1&site=acme": "Outcomes",
		"/messages?period=30d&q=to:yahoo.com":  "Search tips",
		"/queue":                               "In the queue now",
		"/providers?period=30d":                "Microsoft",
		"/domains":                             "No DNS checks yet",
		"/servers":                             "203.0.113.10",
		"/alerts":                              "No alerts configured",
		"/metrics":                             `mailinois_queue_messages{server="relay-1"}`,
		"/healthz":                             "ok",
		"/static/app.css":                      "--accent",
		"/static/app.js":                       "liveTail",
		"/messages/" + f.messageID(t, "4D5E6F7081"):                  "erin@icloud.example",
		"/messages/" + f.messageID(t, "4D5E6F7081") + "?tab=log":     "postfix/qmgr: 4D5E6F7081: from=",
		"/messages/" + f.messageID(t, "4D5E6F7081") + "?tab=headers": "Not captured",
	} {
		code, body, _ := get(t, c, f.srv.URL+path)
		if code != 200 || !strings.Contains(body, want) {
			t.Errorf("GET %s = %d, want 200 containing %q", path, code, want)
		}
	}
	for _, path := range []string{"/messages/999999", "/messages/abc", "/nope"} {
		if code, _, _ := get(t, c, f.srv.URL+path); code != 404 {
			t.Errorf("GET %s = %d, want 404", path, code)
		}
	}
	// The site picker narrows: relay-2's message is outside the acme site's domains only when it isn't from acme.test.
	code, body, _ := get(t, c, f.srv.URL+"/messages?period=30d&site=local")
	if code != 200 || !strings.Contains(body, "7A8B9C0D1E") || strings.Contains(body, "4D5E6F7081") {
		t.Errorf("site=local: %d, want the www-data pickup and not acme.test's messages", code)
	}
}

func TestPagesVersionTheirAssets(t *testing.T) {
	for _, page := range []struct{ mode, path string }{{"off", "/domains"}, {"password", "/login"}} {
		t.Run(page.path, func(t *testing.T) {
			f := newFixture(t, page.mode)
			code, body, _ := get(t, http.DefaultClient, f.srv.URL+page.path)
			if code != 200 {
				t.Fatalf("page returned %d, want 200", code)
			}
			seen := map[string]bool{}
			for _, match := range regexp.MustCompile(`(?:src|href)="(/static/[^"]+)"`).FindAllStringSubmatch(body, -1) {
				u, err := url.Parse(match[1])
				if err != nil {
					t.Fatal(err)
				}
				code, asset, _ := get(t, http.DefaultClient, f.srv.URL+u.String())
				if code != 200 {
					t.Fatalf("asset %s returned %d, want 200", u.Path, code)
				}
				if want := fmt.Sprintf("%x", sha256.Sum256([]byte(asset))); u.Query().Get("v") != want {
					t.Errorf("%s lacks its current content hash", u.Path)
				}
				seen[u.Path] = true
			}
			for _, path := range []string{"/static/app.css", "/static/app.js", "/static/logo.svg"} {
				if !seen[path] {
					t.Errorf("page did not load %s", path)
				}
			}
		})
	}
}

func TestPasswordLoginAndSiteRestriction(t *testing.T) {
	f := newFixture(t, "password")
	c := noRedirects()
	if code, _, h := get(t, c, f.srv.URL+"/queue"); code != 303 || !strings.HasPrefix(h.Get("Location"), "/login?next=") {
		t.Fatalf("signed out: %d %s, want a redirect to the login form", code, h.Get("Location"))
	}

	login := func(name, password, next string, headers ...string) *http.Response {
		req, _ := http.NewRequest("POST", f.srv.URL+"/login", strings.NewReader(url.Values{"name": {name}, "password": {password}, "next": {next}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		for i := 0; i < len(headers); i += 2 {
			req.Header.Set(headers[i], headers[i+1])
		}
		res, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res
	}
	if res := login("ann", "wrong", "/queue", "Origin", f.srv.URL); res.StatusCode != 401 {
		t.Errorf("wrong password: %d, want 401", res.StatusCode)
	}
	if res := login("ann", "s3cret password", "/queue", "Origin", "https://evil.example"); res.StatusCode != 403 {
		t.Errorf("a login posted from another origin: %d, want 403", res.StatusCode)
	}
	if res := login("ann", "s3cret password", "/queue", "Origin", "null", "Sec-Fetch-Site", "cross-site"); res.StatusCode != 403 {
		t.Errorf("a cross-site login with no origin: %d, want 403", res.StatusCode)
	}
	// What Chrome sends for the login form itself: no-referrer pages post with Origin: null.
	if res := login("ann", "s3cret password", "/queue", "Origin", "null", "Sec-Fetch-Site", "same-origin"); res.StatusCode != 303 {
		t.Errorf("the login page's own form, as a browser posts it: %d, want 303", res.StatusCode)
	}
	for _, next := range []string{"//evil.example", "/\\evil.example", "https://evil.example"} {
		if res := login("ann", "s3cret password", next, "Origin", f.srv.URL); res.Header.Get("Location") != "/" {
			t.Errorf("next=%s: sent to %q, want /", next, res.Header.Get("Location"))
		}
	}
	res := login("ann", "s3cret password", "/queue", "Origin", f.srv.URL)
	if res.StatusCode != 303 || res.Header.Get("Location") != "/queue" || len(res.Cookies()) == 0 {
		t.Fatalf("right password: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	jar, _ := cookiejar.New(nil)
	u, _ := url.Parse(f.srv.URL)
	jar.SetCookies(u, res.Cookies())
	ann := &http.Client{Jar: jar}

	if code, body, _ := get(t, ann, f.srv.URL+"/messages?period=30d"); code != 200 || strings.Contains(body, "4D5E6F7081") || !strings.Contains(body, "7A8B9C0D1E") {
		t.Errorf("ann (local only): %d; sees acme.test: %v, sees the local pickup: %v", code, strings.Contains(body, "4D5E6F7081"), strings.Contains(body, "7A8B9C0D1E"))
	}
	if code, _, _ := get(t, ann, f.srv.URL+"/messages/"+f.messageID(t, "4D5E6F7081")); code != 404 {
		t.Errorf("ann opening a acme.test message: %d, want 404", code)
	}
	if code, _, _ := get(t, ann, f.srv.URL+"/messages/"+f.messageID(t, "7A8B9C0D1E")); code != 200 {
		t.Errorf("ann opening a local message: %d, want 200", code)
	}
}

func TestSignInRateLimit(t *testing.T) {
	f := newFixture(t, "password")
	c := noRedirects()
	login := func(password string) int {
		req, _ := http.NewRequest("POST", f.srv.URL+"/login", strings.NewReader(url.Values{"name": {"sam"}, "password": {password}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", f.srv.URL)
		res, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if code := login("s3cret password"); code != 303 {
		t.Fatalf("right password first: %d, want 303", code)
	}
	for i := range 5 {
		if code := login("guess"); code != 401 {
			t.Fatalf("wrong password %d: %d, want 401", i+1, code)
		}
	}
	if code := login("s3cret password"); code != 429 {
		t.Errorf("right password after 5 failures: %d, want 429 until the window passes", code)
	}
}

func TestAPI(t *testing.T) {
	f := newFixture(t, "password")
	c := http.DefaultClient
	bearer := func(tok string) []string { return []string{"Authorization", "Bearer " + tok} }

	if code, _, _ := get(t, c, f.srv.URL+"/api/v1/summary"); code != 401 {
		t.Errorf("no token: %d, want 401", code)
	}
	if code, _, _ := get(t, c, f.srv.URL+"/api/v1/summary", bearer(f.ingestToken)...); code != 403 {
		t.Errorf("ingest token reading: %d, want 403", code)
	}
	code, body, _ := get(t, c, f.srv.URL+"/api/v1/messages?period=30d&q=provider:microsoft", bearer(f.readToken)...)
	var list struct {
		Messages []struct {
			QueueID string `json:"queue_id"`
			Server  string `json:"server"`
		} `json:"messages"`
	}
	json.Unmarshal([]byte(body), &list)
	if code != 200 || len(list.Messages) != 2 || list.Messages[0].QueueID != "1A2B3C4D5E" {
		t.Errorf("messages q=provider:microsoft: %d %+v", code, list.Messages)
	}
	for _, path := range []string{"/api/v1/summary?period=30d", "/api/v1/queue", "/api/v1/providers", "/api/v1/servers", "/api/v1/ips", "/api/v1/domains", "/api/v1/alerts", "/api/v1/tail", "/api/v1/messages/" + f.messageID(t, "4D5E6F7081")} {
		code, body, h := get(t, c, f.srv.URL+path, bearer(f.readToken)...)
		if code != 200 || !strings.HasPrefix(h.Get("Content-Type"), "application/json") || !json.Valid([]byte(body)) {
			t.Errorf("GET %s = %d %s", path, code, h.Get("Content-Type"))
		}
	}
}

func TestIngestAndCapture(t *testing.T) {
	f := newFixture(t, "password")
	at := time.Now().UTC()
	stamp := at.Format(time.RFC3339Nano)
	batch := map[string]any{
		"server": "agent-host",
		"lines": []string{
			stamp + " INFO    postfix/smtpd[1]: ABCDEF1234: client=app[10.0.0.1]",
			stamp + " INFO    postfix/cleanup[2]: ABCDEF1234: message-id=<cap@acme.test>",
			stamp + " INFO    postfix/qmgr[3]: ABCDEF1234: from=<hello@acme.test>, size=500, nrcpt=1 (queue active)",
			stamp + ` INFO    postfix/smtp[4]: ABCDEF1234: to=<x@example.net>, relay=mx.example.net[198.51.100.7]:25, delay=1, delays=0/0/0/1, dsn=5.0.0, status=bounced (host said: 550 <script>alert(1)</script>)`,
		},
		"captures": []map[string]any{{
			"queue_id": "ABCDEF1234", "message_id": "cap@acme.test", "server": "mail.example", "received_at": at,
			"header": []byte("From: hello@acme.test\r\nSubject: Captured\r\nMessage-ID: <cap@acme.test>\r\nMIME-Version: 1.0\r\nContent-Type: multipart/alternative; boundary=b\r\n\r\n"),
			"body":   []byte("--b\r\nContent-Type: text/plain\r\n\r\nPlain words\r\n--b\r\nContent-Type: text/html\r\n\r\n<p>Rich <script>alert(2)</script>words</p>\r\n--b--\r\n"),
			"size":   200,
		}},
	}
	raw, _ := json.Marshal(batch)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(raw)
	zw.Close()
	post := func(tok string) int {
		req, _ := http.NewRequest("POST", f.srv.URL+"/api/v1/ingest", bytes.NewReader(gz.Bytes()))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Encoding", "gzip")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if code := post(f.readToken); code != 403 {
		t.Errorf("ingest with a read token: %d, want 403", code)
	}
	if code := post(f.ingestToken); code != 202 {
		t.Fatalf("ingest: %d, want 202", code)
	}

	var id string
	for range 50 {
		rows, _ := f.st.Messages(context.Background(), store.Filter{Since: at.Add(-time.Hour), Until: at.Add(time.Hour), Terms: []store.Term{{Field: "has", Value: "capture"}}}, 0, 1)
		if len(rows) == 1 && rows[0].Server == "agent-host" && rows[0].Status == "bounced" {
			id = strconv.FormatInt(rows[0].ID, 10)
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if id == "" {
		t.Fatal("the shipped lines and capture never became a captured, bounced message from agent-host")
	}

	c := http.DefaultClient
	bearer := []string{"Authorization", "Bearer " + f.readToken}
	_, body, _ := get(t, c, f.srv.URL+"/api/v1/messages/"+id, bearer...)
	if !strings.Contains(body, `"Plain words`) || !strings.Contains(body, `"eml": "/api/v1/messages/`+id+`/eml"`) {
		t.Errorf("API message lacks the capture: %s", body)
	}
	code, eml, h := get(t, c, f.srv.URL+"/api/v1/messages/"+id+"/eml", bearer...)
	if code != 200 || h.Get("Content-Type") != "message/rfc822" || !strings.HasPrefix(eml, "From: hello@acme.test") || !strings.Contains(eml, "Plain words") {
		t.Errorf("eml: %d %s %q", code, h.Get("Content-Type"), eml)
	}
}

// Log shippers (Vector, Fluent Bit) post plain lines or JSON lines rather than the agent's batch.
func TestIngestFromLogShippers(t *testing.T) {
	f := newFixture(t, "password")
	at := time.Now().UTC()
	line := func(qid string) string {
		return at.Format(time.RFC3339Nano) + " INFO    postfix/smtp[4]: " + qid + ": to=<x@example.net>, relay=mx.example.net[198.51.100.7]:25, delay=1, delays=0/0/0/1, dsn=2.0.0, status=sent (250 ok)"
	}
	post := func(tok, contentType, server string, body []byte, gzipped bool) (int, string) {
		if gzipped {
			var gz bytes.Buffer
			zw := gzip.NewWriter(&gz)
			zw.Write(body)
			zw.Close()
			body = gz.Bytes()
		}
		req, _ := http.NewRequest("POST", f.srv.URL+"/api/v1/ingest?server="+server, bytes.NewReader(body))
		req.Header.Set("Content-Type", contentType)
		if gzipped {
			req.Header.Set("Content-Encoding", "gzip")
		}
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}

	text := []byte(line("A1A1A1A1A1") + "\n\n" + line("A2A2A2A2A2") + "\n")
	if code, _ := post("", "text/plain", "k8s-postfix", text, false); code != 401 {
		t.Errorf("plain lines without a token: %d, want 401", code)
	}
	if code, body := post(f.ingestToken, "text/plain; charset=utf-8", "k8s-postfix", text, true); code != 202 || !strings.Contains(body, `"lines": 2`) {
		t.Errorf("plain lines, gzipped: %d %s, want 202 and 2 lines", code, body)
	}
	ndjson := []byte(`{"message":` + strconv.Quote(line("B1B1B1B1B1")) + `,"host":"node-1"}` + "\n" +
		`not json` + "\n" +
		`{"log":` + strconv.Quote(line("B2B2B2B2B2")) + `,"stream":"stdout"}` + "\n" +
		`{"other":"field"}` + "\n")
	if code, body := post(f.ingestToken, "application/x-ndjson", "k8s-postfix", ndjson, false); code != 202 || !strings.Contains(body, `"lines": 2`) || !strings.Contains(body, `"skipped": 2`) {
		t.Errorf("JSON lines: %d %s, want 202, 2 lines and 2 skipped", code, body)
	}

	// Over 8 MiB once decompressed is refused, not cut short: a 202 would tell the shipper the rest arrived.
	big := bytes.Repeat([]byte(line("C1C1C1C1C1")+"\n"), 9<<20/len(line("C1C1C1C1C1")))
	if code, body := post(f.ingestToken, "text/plain", "k8s-postfix", big, true); code != 413 {
		t.Errorf("9 MiB of lines, gzipped: %d %s, want 413", code, body)
	}

	for _, qid := range []string{"A1A1A1A1A1", "A2A2A2A2A2", "B1B1B1B1B1", "B2B2B2B2B2"} {
		var rows []store.MessageRow
		for range 50 {
			rows, _ = f.st.Messages(context.Background(), store.Filter{Since: at.Add(-time.Hour), Until: at.Add(time.Hour), Terms: []store.Term{{Field: "qid", Value: qid}}}, 0, 1)
			if len(rows) == 1 {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if len(rows) != 1 || rows[0].Server != "k8s-postfix" || rows[0].Status != "sent" {
			t.Errorf("%s: %+v, want one sent message from k8s-postfix", qid, rows)
		}
	}
}

// A full queue answers 503, so the agent and log shippers keep the lines and send them again, rather than 202
// for lines that were thrown away.
func TestIngestWhenTheQueueIsFull(t *testing.T) {
	f := newFixture(t, "off")
	f.server.Pipeline = ingest.New(f.st, time.UTC, 2) // nothing drains it
	f.server.IngestWait = 50 * time.Millisecond
	at := time.Now().UTC().Format(time.RFC3339Nano)
	post := func(lines ...string) (int, string, http.Header) {
		req, _ := http.NewRequest("POST", f.srv.URL+"/api/v1/ingest?server=k8s", strings.NewReader(strings.Join(lines, "\n")))
		req.Header.Set("Content-Type", "text/plain")
		req.Header.Set("Authorization", "Bearer "+f.ingestToken)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b), res.Header
	}
	line := func(qid string) string { return at + " INFO    postfix/qmgr[1]: " + qid + ": removed" }
	if code, body, _ := post(line("A0A0A0A001"), line("A0A0A0A002")); code != 202 {
		t.Fatalf("two lines into a queue of two: %d %s, want 202", code, body)
	}
	code, body, h := post(line("A0A0A0A003"))
	if code != 503 || h.Get("Retry-After") == "" || !strings.Contains(body, `"lines": 0`) {
		t.Errorf("a line into a full queue: %d %q, Retry-After %q; want 503 with Retry-After and 0 lines taken", code, body, h.Get("Retry-After"))
	}
}

// Text from other mail servers and from captured messages must reach the page as text, and captured HTML
// only inside a sandbox.
func TestUntrustedTextIsContained(t *testing.T) {
	f := newFixture(t, "off")
	at := time.Now().UTC()
	e, _ := postfix.Parse(syslog.Line{Host: "relay-1", Time: at, Program: "postfix/smtp",
		Msg: `FEDCBA4321: to=<x@example.net>, relay=mx.example.net[198.51.100.7]:25, delay=1, delays=0/0/0/1, dsn=5.0.0, status=bounced (host said: 550 <script>alert(1)</script>)`})
	f.st.Ingest(context.Background(), []postfix.Event{e})
	f.st.SaveCapture(context.Background(), store.Capture{QueueID: "FEDCBA4321", At: at,
		Header: []byte("Subject: <img src=x onerror=alert(3)>\r\nContent-Type: text/html\r\n\r\n"), Body: []byte("<p>hi</p><script>alert(4)</script>")})
	id := f.messageID(t, "FEDCBA4321")
	c := http.DefaultClient

	code, body, _ := get(t, c, f.srv.URL+"/messages/"+id)
	if code != 200 || !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") || strings.Contains(body, "<script>alert") {
		t.Errorf("message page: %d; escaped response present: %v; raw script present: %v", code, strings.Contains(body, "&lt;script&gt;"), strings.Contains(body, "<script>alert"))
	}
	code, body, _ = get(t, c, f.srv.URL+"/messages/"+id+"?tab=headers")
	if code != 200 || !strings.Contains(body, "&lt;img src=x onerror=alert(3)&gt;") || strings.Contains(body, "<img src=x") {
		t.Errorf("headers tab: %d, escaped: %v", code, strings.Contains(body, "&lt;img"))
	}
	code, body, _ = get(t, c, f.srv.URL+"/messages/"+id+"?tab=body")
	if code != 200 || !strings.Contains(body, `<iframe class="htmlpreview" sandbox=""`) {
		t.Errorf("body tab: %d, sandboxed frame present: %v", code, strings.Contains(body, "sandbox"))
	}
	code, body, h := get(t, c, f.srv.URL+"/messages/"+id+"/html")
	if code != 200 || !strings.Contains(body, "<p>hi</p>") || !strings.HasPrefix(h.Get("Content-Security-Policy"), "sandbox;") {
		t.Errorf("html part: %d, CSP %q", code, h.Get("Content-Security-Policy"))
	}
}

func TestSharedDomainSenderAccess(t *testing.T) {
	f := newFixture(t, "password")
	f.server.Config.Sites[1].Domains = nil
	f.server.Config.Sites[1].Senders = []string{"status@widgets.example"}
	events := []postfix.Event{
		{Server: "relay", At: time.Now(), QueueID: "ABCDEF0001", Kind: postfix.Queued, Sender: "status@widgets.example", Raw: "status sender"},
		{Server: "relay", At: time.Now(), QueueID: "ABCDEF0002", Kind: postfix.Queued, Sender: "forms@widgets.example", Raw: "forms sender"},
	}
	if _, err := f.st.Ingest(context.Background(), events); err != nil {
		t.Fatal(err)
	}
	c := noRedirects()
	res, err := c.PostForm(f.srv.URL+"/login", url.Values{"name": {"ann"}, "password": {"s3cret password"}})
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatal(res.StatusCode)
	}
	jar, _ := cookiejar.New(nil)
	u, _ := url.Parse(f.srv.URL)
	jar.SetCookies(u, res.Cookies())
	c.Jar = jar
	for qid, expected := range map[string]int{"ABCDEF0001": 200, "ABCDEF0002": 404} {
		code, _, _ := get(t, c, f.srv.URL+"/api/v1/messages/"+f.messageID(t, qid))
		if code != expected {
			t.Errorf("%s status %d, want %d", qid, code, expected)
		}
	}
	code, body, _ := get(t, c, f.srv.URL+"/api/v1/messages")
	if code != 200 || !strings.Contains(body, "status@widgets.example") || strings.Contains(body, "forms@widgets.example") {
		t.Fatalf("scoped API: %d %s", code, body)
	}
}

func TestDomainSummaries(t *testing.T) {
	f := newFixture(t, "off")
	for _, kind := range []string{"spf", "dkim", "dmarc"} {
		if err := f.st.SaveDNS(context.Background(), store.DNSResult{Domain: "shared.example", Kind: kind, Status: "pass", Record: "<script>untrusted</script>", CheckedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	code, body, _ := get(t, http.DefaultClient, f.srv.URL+"/domains")
	if code != 200 || strings.Count(body, `class="domain-row"`) != 1 || !strings.Contains(body, `class="domain-details scroll"`) || !strings.Contains(body, "&lt;script&gt;") || strings.Contains(body, "<script>untrusted") {
		t.Fatalf("domain summary: %d %s", code, body)
	}
}
