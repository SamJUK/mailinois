package report

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SamJUK/mailinois/internal/postfix"
	"github.com/SamJUK/mailinois/internal/store"
	"github.com/SamJUK/mailinois/internal/syslog"
)

// hostile is a remote server's answer written to break out of every format the report writes.
const hostile = "550 </script><script>alert(1)</script> <img src=x onerror=alert(2)> ![t](http://tracker.example/p.png) `x` | y"

func build(t *testing.T, search string, limit int) *Report {
	t.Helper()
	time.Local = time.UTC // the report command sets it to the log's zone; the store hands times back in it
	ctx := context.Background()
	st, err := store.Open(ctx, "sqlite:"+filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	f, err := os.Open("../store/testdata/scenarios.log")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var events []postfix.Event
	add := func(text string) {
		if l, ok := syslog.Parse(text, time.UTC, time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)); ok {
			if e, ok := postfix.Parse(l); ok {
				events = append(events, e)
			}
		}
	}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		add(sc.Text())
	}
	add("2026-10-18T12:00:00Z relay-1 postfix/smtpd[9]: E0E0E00001: client=app-1[10.1.0.4]")
	add("2026-10-18T12:00:01Z relay-1 postfix/qmgr[9]: E0E0E00001: from=<hello@acme.test>, size=1, nrcpt=1 (queue active)")
	add("2026-10-18T12:00:02Z relay-1 postfix/smtp[9]: E0E0E00001: to=<victim@example.net>, relay=mx.example.net[198.51.100.7]:25, delay=1, delays=0/0/0/1, dsn=5.0.0, status=bounced (host said: " + hostile + ")")
	add("2026-10-18T12:00:03Z relay-1 postfix/qmgr[9]: E0E0E00001: removed")
	if _, err := st.Ingest(ctx, events); err != nil {
		t.Fatal(err)
	}
	filter := store.Filter{Since: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Until: time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC),
		Terms: store.ParseSearch(search, time.Now()).Terms}
	r, err := Build(ctx, st, filter, limit)
	if err != nil {
		t.Fatal(err)
	}
	r.Timezone, r.Version = "UTC", "test"
	return r
}

func TestBuild(t *testing.T) {
	r := build(t, "", 1000)
	d := r.Summary
	if d.Received != 15 || d.Sent != 10 || d.Deferrals != 6 || d.Bounced != 3 || d.Expired != 1 || d.Queue != 1 {
		t.Errorf("summary = %+v", d)
	}
	if len(r.Messages) != 16 || r.More || len(r.Messages[0].Events) == 0 || len(r.Messages[0].Recipients) == 0 {
		t.Errorf("%d messages (more %v), want 16, each with its events and recipients", len(r.Messages), r.More)
	}
	if len(r.Queue) != 1 || r.Queue[0].QueueID != "4D5E6F7081" {
		t.Errorf("still queued = %+v, want 4D5E6F7081, whose log ends deferred", r.Queue)
	}

	r = build(t, "to:carol@gmail.example", 1000)
	if len(r.Messages) != 1 || r.Messages[0].QueueID != "4D5E6F7081" {
		t.Errorf("to:carol@gmail.example: %d messages, want only 4D5E6F7081", len(r.Messages))
	}
	if r = build(t, "", 3); len(r.Messages) != 3 || !r.More {
		t.Errorf("limit 3: %d messages, more %v", len(r.Messages), r.More)
	}
}

func TestHTMLContainsUntrustedText(t *testing.T) {
	r := build(t, "", 1000)
	var b bytes.Buffer
	if err := r.HTML(&b); err != nil {
		t.Fatal(err)
	}
	html := b.String()
	if !strings.Contains(html, "E0E0E00001") || !strings.Contains(html, "\\u003c/script\\u003e\\u003cscript\\u003ealert(1)") {
		t.Fatal("the hostile message is missing, or not in its JSON-escaped form")
	}
	if strings.Contains(html, "<script>alert") || strings.Contains(html, "<img src=x") || strings.Count(html, "</script>") != 2 {
		t.Errorf("hostile text reached the page as markup (%d </script> tags, want the page's own 2)", strings.Count(html, "</script>"))
	}
	hash := func(s string) string {
		sum := sha256.Sum256([]byte(s))
		return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
	}
	if !strings.Contains(html, "script-src "+hash(js)+";") || !strings.Contains(html, "<script>"+js+"</script>") || !strings.Contains(html, "default-src 'none'") {
		t.Error("the CSP doesn't allow exactly the inline script, or lets other sources load")
	}
}

func TestMarkdownContainsUntrustedText(t *testing.T) {
	r := build(t, "", 1000)
	var b bytes.Buffer
	if err := r.Markdown(&b); err != nil {
		t.Fatal(err)
	}
	md := b.String()
	if !strings.Contains(md, "### 2026-10-18 12:00:00, E0E0E00001, bounced") {
		t.Fatalf("the hostile message's heading is missing:\n%s", md[:min(len(md), 600)])
	}
	for _, line := range strings.Split(md, "\n") {
		if strings.HasPrefix(line, "| `") && strings.Contains(line, "victim@example.net") {
			// Its answer sits in a code span opened by more backticks than it contains, with the pipe escaped.
			if !strings.Contains(line, "`` host said: 550 </script>") || !strings.Contains(line, "\\| y ``") {
				t.Errorf("recipient row isn't contained: %s", line)
			}
		}
	}
	if code("a`b") != "`` a`b ``" || code("x|y\nz") != "` x\\|y z `" {
		t.Errorf("code() = %q, %q", code("a`b"), code("x|y\nz"))
	}
	if fenceFor([]string{"plain", "has ```` four"}) != "`````" {
		t.Error("fence not longer than the backticks inside")
	}
}
