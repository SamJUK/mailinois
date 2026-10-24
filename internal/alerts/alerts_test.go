package alerts

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/SamJUK/mailinois/internal/config"
	"github.com/SamJUK/mailinois/internal/postfix"
	"github.com/SamJUK/mailinois/internal/store"
	"github.com/SamJUK/mailinois/internal/store/storetest"
	"github.com/SamJUK/mailinois/internal/syslog"
)

// deliver logs n messages to recipients at domain, each ending with status.
func deliver(t *testing.T, st *store.Store, at time.Time, n int, status, dsn string) {
	t.Helper()
	var events []postfix.Event
	for i := range n {
		qid := fmt.Sprintf("%X", at.UnixNano()+int64(i))[:12]
		for j, msg := range []string{
			qid + ": client=app[10.0.0.1]",
			qid + ": from=<hello@store-a.example>, size=100, nrcpt=1 (queue active)",
			fmt.Sprintf("%s: to=<u%d@gmail.example>, relay=gmail-smtp-in.l.google.com[1.2.3.4]:25, delay=1, delays=0/0/0/1, dsn=%s, status=%s (x)", qid, i, dsn, status),
			qid + ": removed",
		} {
			e, ok := postfix.Parse(syslog.Line{Host: "relay", Time: at.Add(time.Duration(j) * time.Millisecond), Program: []string{"postfix/smtpd", "postfix/qmgr", "postfix/smtp", "postfix/qmgr"}[j], Msg: msg})
			if !ok {
				t.Fatalf("unparsed: %s", msg)
			}
			events = append(events, e)
		}
	}
	if _, err := st.Ingest(context.Background(), events); err != nil {
		t.Fatal(err)
	}
}

func TestFiringResolvingAndSignature(t *testing.T) {
	var mu sync.Mutex
	var got []Payload
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		m := hmac.New(sha256.New, []byte("shh"))
		m.Write(body)
		if r.Header.Get("X-Mailinois-Signature") != "sha256="+hex.EncodeToString(m.Sum(nil)) {
			t.Errorf("bad signature %q", r.Header.Get("X-Mailinois-Signature"))
		}
		var p Payload
		json.Unmarshal(body, &p)
		mu.Lock()
		got = append(got, p)
		mu.Unlock()
	}))
	defer hook.Close()

	st := storetest.SQLite(t)
	cfg := &config.Config{
		Sites:    []config.Site{{Slug: "storea", Domains: []string{"store-a.example"}}},
		Webhooks: []config.Webhook{{Name: "ops", URL: hook.URL, Format: "json", Secret: "shh"}},
		Alerts: []config.Alert{
			{Name: "Bounces", Metric: "bounce_rate", Above: 0.1, Window: config.Duration(time.Hour), MinMessages: 5, Site: "storea", Notify: []string{"ops"}},
			{Name: "Other site", Metric: "bounce_rate", Above: 0.1, Window: config.Duration(time.Hour), Site: "storeb", Notify: []string{"ops"}},
		},
	}
	cfg.Sites = append(cfg.Sites, config.Site{Slug: "storeb", Domains: []string{"store-b.example"}})
	e := &Evaluator{Store: st, Config: cfg, Lifetime: 120 * time.Hour}
	ctx := context.Background()
	now := time.Now()

	deliver(t, st, now.Add(-30*time.Minute), 2, "sent", "2.0.0")
	deliver(t, st, now.Add(-20*time.Minute), 1, "bounced", "5.1.1")
	if err := e.Check(ctx, now); err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("fired on 3 messages, under min_messages: %+v", got)
	}

	deliver(t, st, now.Add(-10*time.Minute), 3, "sent", "2.0.0")
	if err := e.Check(ctx, now); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].State != "firing" || got[0].Alert != "Bounces" || got[0].Scope.Site != "storea" || got[0].Value < 0.16 {
		t.Fatalf("after 1 bounce in 6: %+v", got)
	}
	if err := e.Check(ctx, now.Add(time.Second)); err != nil || len(got) != 1 {
		t.Fatalf("still firing notified again without repeat: %+v %v", got, err)
	}

	deliver(t, st, now.Add(-5*time.Minute), 10, "sent", "2.0.0")
	if err := e.Check(ctx, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1].State != "resolved" {
		t.Fatalf("after 1 bounce in 16: %+v", got)
	}
	rules, _ := e.Rules(ctx)
	if rules[0].State.State != "ok" || rules[1].State.State != "ok" {
		t.Errorf("states = %+v", rules)
	}
	events, _ := st.AlertEvents(ctx, 10)
	if len(events) != 2 {
		t.Errorf("history = %+v, want firing then resolved", events)
	}
}
