package store_test

import (
	"database/sql"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/SamJUK/mailinois/internal/store"
)

func TestParseSearch(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	s := store.ParseSearch(`from:store-a.example -status:sent,deferred subject:"order confirmed" Bob after:2d before:2026-10-18 nope:x`, now)
	want := []store.Term{
		{Field: "from", Value: "store-a.example"},
		{Field: "status", Value: "sent,deferred", Not: true},
		{Field: "subject", Value: "order confirmed"},
		{Value: "Bob"},
		{Value: "nope:x"},
	}
	if !slices.EqualFunc(s.Terms, want, func(a, b store.Term) bool { return a.Field == b.Field && a.Value == b.Value && a.Not == b.Not }) {
		t.Errorf("terms = %+v\nwant    %+v", s.Terms, want)
	}
	if s.After == nil || !s.After.Equal(now.Add(-48*time.Hour)) || s.Before == nil || s.Before.Format("2006-01-02") != "2026-10-18" {
		t.Errorf("after %v, before %v", s.After, s.Before)
	}
}

// queueIDs runs a search over the scenarios and returns the queue IDs found (NOQUEUE for rejected attempts).
func queueIDs(t *testing.T, st *store.Store, f store.Filter) []string {
	t.Helper()
	rows, err := st.Messages(ctx, f, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range rows {
		if r.QueueID == "" {
			r.QueueID = "NOQUEUE"
		}
		out = append(out, r.QueueID)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func TestSearch(t *testing.T) {
	each(t, func(t *testing.T, st *store.Store) {
		for q, want := range map[string][]string{
			"to:yahoo.example":                 {"4D5E6F7081"},
			"to:erin@":                         {"4D5E6F7081"},
			"from:example.org":                 {"NOQUEUE"},
			"provider:microsoft":               {"1A2B3C4D5E"},
			"dsn:4.7.650":                      {"1A2B3C4D5E"},
			"dsn:5.1":                          {"0A1B2C3D4E", "2B3C4D5E6F"},
			"status:expired,deleted":           {"5E6F708192", "9C0D1E2F3A"},
			"subject:sign-in":                  {"0DFD162666"},
			"client:10.1.0.9":                  {"NOQUEUE"},
			"relay:icloud":                     {"4D5E6F7081"},
			"qid:7A8B9C0D1E":                   {"7A8B9C0D1E"},
			"msgid:<g7@acme.test>":             {"0DFD162666"},
			"gina":                             {"0DFD162666"},
			"server:relay-2":                   {"1A2B3C4D5E"},
			"-from:acme.test -status:rejected": {"3C4D5E6F70", "6F708192A3", "7A8B9C0D1E"},
		} {
			f := all
			f.Terms = store.ParseSearch(q, time.Now()).Terms
			if got := queueIDs(t, st, f); !slices.Equal(got, want) {
				t.Errorf("%s: got %v, want %v", q, got, want)
			}
		}
	})
}

func TestSitesAndAccess(t *testing.T) {
	each(t, func(t *testing.T, st *store.Store) {
		// relay-1's pickup (www-data@relay-1.example) and the bounce notifications to hello@acme.test
		// show which domain each message counts under.
		acme := store.Scope{Domains: []string{"acme.test"}}
		local := store.Scope{Domains: []string{"example"}, Servers: []string{"relay-2"}}

		f := all
		f.Sites = []store.Scope{local}
		if got, want := queueIDs(t, st, f), []string{"1A2B3C4D5E", "7A8B9C0D1E"}; !slices.Equal(got, want) {
			t.Errorf("site by subdomain and server: got %v, want %v", got, want)
		}

		f = all
		f.Allowed = []store.Scope{acme}
		got := queueIDs(t, st, f)
		if slices.Contains(got, "7A8B9C0D1E") || slices.Contains(got, "NOQUEUE") || !slices.Contains(got, "3C4D5E6F70") || len(got) < 10 {
			t.Errorf("allowed acme.test: got %v (bounce notifications should count, www-data and example.org shouldn't)", got)
		}

		// A message outside what the viewer may see is not found, rather than shown.
		pickup := one(t, st, "relay-1", "7A8B9C0D1E")
		if _, err := st.Message(ctx, f, pickup.ID); !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("message outside the allowed sites: err %v, want sql.ErrNoRows", err)
		}
		if m, err := st.Message(ctx, all, pickup.ID); err != nil || m.QueueID != "7A8B9C0D1E" {
			t.Errorf("the same message unrestricted: %v, %v", m, err)
		}

		f.Sites = []store.Scope{local}
		if got := queueIDs(t, st, f); !slices.Equal(got, []string{"1A2B3C4D5E"}) {
			t.Errorf("picker and restriction both apply: got %v, want relay-2's message only", got)
		}
		f = all
		f.Sites = []store.Scope{{}}
		if got := queueIDs(t, st, f); len(got) != 0 {
			t.Errorf("an empty site matches nothing: got %v", got)
		}
	})
}

func TestCapture(t *testing.T) {
	each(t, func(t *testing.T, st *store.Store) {
		m := one(t, st, "relay-1", "4D5E6F7081")
		header := []byte("DKIM-Signature: v=1; a=rsa-sha256; d=acme.test;\r\n s=demo2026; h=From:To; b=abc\r\nMessage-ID: <e5@acme.test>\r\nSubject: Report\r\n\r\n")
		err := st.SaveCapture(ctx, store.Capture{QueueID: "4D5E6F7081", MessageID: "<e5@acme.test>", Server: "mail.example",
			At: m.ReceivedAt.Add(time.Second), Header: header, Body: []byte("Hello\r\n"), Size: 99})
		if err != nil {
			t.Fatal(err)
		}
		c, err := st.CaptureFor(ctx, m)
		if err != nil || c == nil || string(c.Header) != string(header) || string(c.Body) != "Hello\r\n" || c.MessageID != "e5@acme.test" {
			t.Fatalf("capture = %+v, %v", c, err)
		}
		if other, _ := st.CaptureFor(ctx, one(t, st, "relay-1", "1A2B3C4D5E")); other != nil {
			t.Errorf("another message got the capture: %+v", other)
		}
		f := all
		f.Terms = store.ParseSearch("has:capture", time.Now()).Terms
		if got := queueIDs(t, st, f); !slices.Equal(got, []string{"4D5E6F7081"}) {
			t.Errorf("has:capture = %v", got)
		}
		f.Terms = store.ParseSearch("subject:report", time.Now()).Terms
		if got := queueIDs(t, st, f); !slices.Equal(got, []string{"4D5E6F7081"}) {
			t.Errorf("subject:report (only in the capture) = %v", got)
		}
		if again := one(t, st, "relay-1", "4D5E6F7081"); again.Subject != "Report" {
			t.Errorf("subject = %q, want the captured one", again.Subject)
		}
		sel, err := st.Selectors(ctx, time.Time{})
		if err != nil || !slices.Contains(sel["acme.test"], "demo2026") {
			t.Errorf("selectors = %v, %v (from the capture's DKIM-Signature and OpenDKIM's line)", sel, err)
		}
	})
}
