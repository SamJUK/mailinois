package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/SamJUK/mailinois/internal/store"
	"github.com/SamJUK/mailinois/internal/store/storetest"
)

var (
	ctx = context.Background()
	all = store.Filter{Since: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Until: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	// Long enough that "still in the queue" doesn't depend on when the tests run.
	forever = 100 * 365 * 24 * time.Hour
)

// each runs a test against every database, with the scenarios loaded.
func each(t *testing.T, fn func(t *testing.T, st *store.Store)) {
	storetest.Each(t, func(t *testing.T, st *store.Store) {
		storetest.Load(t, st, "testdata/scenarios.log")
		fn(t, st)
	})
}

// messages returns the messages with a queue ID on a server, newest first.
func messages(t *testing.T, st *store.Store, server, queueID string) []*store.Message {
	t.Helper()
	f := all
	f.Server = server
	if queueID != "" {
		f.Terms = []store.Term{{Field: "qid", Value: queueID}}
	}
	rows, err := st.Messages(ctx, f, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []*store.Message
	for _, row := range rows {
		m, err := st.Message(ctx, all, row.ID)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func one(t *testing.T, st *store.Store, server, queueID string) *store.Message {
	t.Helper()
	ms := messages(t, st, server, queueID)
	if len(ms) != 1 {
		t.Fatalf("%s %s: %d messages, want 1", server, queueID, len(ms))
	}
	return ms[0]
}

func recipient(t *testing.T, m *store.Message, address string) store.Recipient {
	t.Helper()
	for _, r := range m.Recipients {
		if r.Address == address {
			return r
		}
	}
	t.Fatalf("%s has no recipient %s: %+v", m.QueueID, address, m.Recipients)
	return store.Recipient{}
}

func TestSent(t *testing.T) {
	each(t, func(t *testing.T, st *store.Store) {
		ms := messages(t, st, "relay-1", "0DFD162666")
		if len(ms) != 2 {
			t.Fatalf("0DFD162666 on relay-1: %d messages, want 2 (the queue ID is reused at 10:00)", len(ms))
		}
		m := ms[1]
		if m.Status != "sent" || m.Sender != "hello@acme.test" || m.Size != 673 || m.MessageID != "a1@acme.test" ||
			m.Subject != "Your sign-in code" || m.ClientHost != "app-1" || m.ClientIP != "10.1.0.4" || m.SASLUser != "acme" || m.RemovedAt == nil {
			t.Errorf("message = %+v", m)
		}
		r := recipient(t, m, "ann@gmail.example")
		if r.Status != "sent" || r.Provider != "google" || r.DSN != "2.0.0" || r.Attempts != 1 || r.Delay == nil || *r.Delay != 0.78 ||
			r.Relay != "gmail-smtp-in.l.google.com" || r.RelayIP != "173.194.220.26" {
			t.Errorf("recipient = %+v", r)
		}
		if len(m.Events) != 7 {
			t.Errorf("%d events, want 7 (client, message-id, subject, DKIM, queued, delivery, removed)", len(m.Events))
		}
	})
}

func TestDeferredThenSent(t *testing.T) {
	each(t, func(t *testing.T, st *store.Store) {
		m := one(t, st, "relay-1", "1A2B3C4D5E")
		r := recipient(t, m, "bob@outlook.example")
		if m.Status != "sent" || r.Status != "sent" || r.Attempts != 3 || r.Provider != "microsoft" || r.DSN != "2.6.0" {
			t.Errorf("status %s, recipient %+v", m.Status, r)
		}
		deferred := 0
		for _, e := range m.Events {
			if e.Kind == "delivery" && e.Status == "deferred" {
				deferred++
			}
		}
		if deferred != 2 {
			t.Errorf("%d deferred attempts in the timeline, want 2", deferred)
		}
	})
}

func TestBounceAndItsNotification(t *testing.T) {
	each(t, func(t *testing.T, st *store.Store) {
		ms := messages(t, st, "relay-1", "2B3C4D5E6F")
		if len(ms) != 2 {
			t.Fatalf("2B3C4D5E6F: %d messages, want 2 (reused at 11:00, its client line missed)", len(ms))
		}
		if m := ms[0]; m.ClientHost != "" || len(m.Recipients) != 1 || recipient(t, m, "lee@gmail.example").Status != "sent" {
			t.Errorf("reuse without a client line = %+v", m)
		}
		m := ms[1]
		if r := recipient(t, m, "nobody@example.net"); m.Status != "bounced" || r.Status != "bounced" || r.DSN != "5.1.1" || r.Provider != "other" {
			t.Errorf("status %s, recipient %+v", m.Status, r)
		}
		n := one(t, st, "relay-1", "3C4D5E6F70")
		if n.Sender != "" || n.Status != "sent" || recipient(t, n, "hello@acme.test").Status != "sent" {
			t.Errorf("notification = %+v", n)
		}
	})
}

func TestRejections(t *testing.T) {
	each(t, func(t *testing.T, st *store.Store) {
		f := all
		f.Terms = []store.Term{{Field: "status", Value: "rejected"}}
		rows, err := st.Messages(ctx, f, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 2 {
			t.Fatalf("%d rejected messages, want 2 (NOQUEUE and milter-reject)", len(rows))
		}
		noqueue, err := st.Message(ctx, all, rows[1].ID) // the older
		if err != nil {
			t.Fatal(err)
		}
		r := recipient(t, noqueue, "kim@gmail.example")
		if noqueue.QueueID != "" || noqueue.Sender != "billing@example.org" || noqueue.ClientIP != "10.1.0.9" || noqueue.RemovedAt == nil ||
			r.Status != "rejected" || r.DSN != "5.7.1" || r.Response != "554 5.7.1 <billing@example.org>: Sender address rejected: Access denied" {
			t.Errorf("NOQUEUE message %+v, recipient %+v", noqueue, r)
		}
		milter := one(t, st, "relay-1", "8B9C0D1E2F")
		if r := recipient(t, milter, "ivy@gmail.example"); milter.Status != "rejected" || milter.RemovedAt == nil || r.DSN != "5.7.1" {
			t.Errorf("milter-reject message %+v, recipient %+v", milter, r)
		}
	})
}

func TestMultipleRecipients(t *testing.T) {
	each(t, func(t *testing.T, st *store.Store) {
		m := one(t, st, "relay-1", "4D5E6F7081")
		if m.Status != "deferred" || m.RemovedAt != nil || len(m.Recipients) != 3 {
			t.Fatalf("message = %+v", m)
		}
		for addr, want := range map[string]string{"carol@gmail.example": "sent", "dave@yahoo.example": "deferred", "erin@icloud.example": "sent"} {
			if got := recipient(t, m, addr).Status; got != want {
				t.Errorf("%s: %s, want %s", addr, got, want)
			}
		}
		if r := recipient(t, m, "erin@icloud.example"); r.OrigTo != "erin@example.com" || r.Provider != "apple" {
			t.Errorf("erin = %+v", r)
		}
	})
}

func TestExpired(t *testing.T) {
	each(t, func(t *testing.T, st *store.Store) {
		m := one(t, st, "relay-1", "5E6F708192")
		r := recipient(t, m, "frank@example.org")
		if m.Status != "expired" || r.Status != "expired" || r.Attempts != 2 || m.RemovedAt == nil {
			t.Errorf("status %s, recipient %+v", m.Status, r)
		}
		for _, e := range m.Events {
			if e.Kind == "expired" && e.Recipient == "frank@example.org" {
				return
			}
		}
		t.Error("no expired event for frank@example.org")
	})
}

func TestReusedQueueIDAndLateLine(t *testing.T) {
	each(t, func(t *testing.T, st *store.Store) {
		ms := messages(t, st, "relay-1", "0DFD162666")
		newer := ms[0]
		// Its removed line arrived a millisecond before its delivery line, and the delivery still joins it.
		if newer.MessageID != "g7@acme.test" || newer.Status != "sent" || newer.RemovedAt == nil ||
			len(newer.Recipients) != 1 || recipient(t, newer, "gina@gmail.example").Status != "sent" {
			t.Errorf("newer = %+v", newer)
		}
	})
}

func TestLineAfterRemovalJoinsTheMessage(t *testing.T) {
	each(t, func(t *testing.T, st *store.Store) {
		m := one(t, st, "relay-1", "0A1B2C3D4E")
		if m.Status != "bounced" || m.Events[len(m.Events)-1].Kind != "notice" {
			t.Errorf("status %s, last event %+v", m.Status, m.Events[len(m.Events)-1])
		}
	})
}

func TestServersAreKeptApart(t *testing.T) {
	each(t, func(t *testing.T, st *store.Store) {
		if m := one(t, st, "relay-2", "1A2B3C4D5E"); len(m.Recipients) != 1 || m.Recipients[0].Address != "hal@outlook.example" {
			t.Errorf("relay-2 = %+v", m.Recipients)
		}
		if m := one(t, st, "relay-1", "1A2B3C4D5E"); len(m.Recipients) != 1 || m.Recipients[0].Address != "bob@outlook.example" {
			t.Errorf("relay-1 = %+v", m.Recipients)
		}
	})
}

func TestPickupAndPostsuper(t *testing.T) {
	each(t, func(t *testing.T, st *store.Store) {
		if m := one(t, st, "relay-1", "7A8B9C0D1E"); m.ClientHost != "uid=33" || m.Status != "sent" {
			t.Errorf("pickup = %+v", m)
		}
		m := one(t, st, "relay-1", "9C0D1E2F3A")
		if r := recipient(t, m, "jo@tailspin.example"); m.Status != "deleted" || m.RemovedAt == nil || r.Status != "deleted" {
			t.Errorf("postsuper -d: status %s, recipient %+v", m.Status, r)
		}
	})
}

func TestDashboard(t *testing.T) {
	each(t, func(t *testing.T, st *store.Store) {
		d, err := st.Dashboard(ctx, all, 24*time.Hour, forever)
		if err != nil {
			t.Fatal(err)
		}
		want := store.Dashboard{Received: 14, Sent: 10, Bounced: 2, Expired: 1, Rejected: 2, Deferrals: 6, Queue: 1}
		if d.Received != want.Received || d.Sent != want.Sent || d.Bounced != want.Bounced || d.Expired != want.Expired ||
			d.Rejected != want.Rejected || d.Deferrals != want.Deferrals || d.Queue != want.Queue {
			t.Errorf("dashboard = %+v\nwant      %+v", d, want)
		}
		sent := 0
		for _, b := range d.Buckets {
			sent += b.Sent
		}
		if len(d.Buckets) != 31 || sent != 10 {
			t.Errorf("%d buckets with %d sent, want 31 and 10", len(d.Buckets), sent)
		}
		if d.P50 == nil || len(d.Failures) == 0 {
			t.Errorf("p50 %v, %d failures", d.P50, len(d.Failures))
		}
		if d.P50 == nil || d.P95 == nil || *d.P50 != 0.7 || *d.P95 != 1.3 {
			t.Errorf("delivery delay p50 %v, p95 %v, want 0.7 and 1.3 (nearest rank over the 10 sent)", d.P50, d.P95)
		}

		if err := st.Optimize(ctx); err != nil {
			t.Fatal(err)
		}
		after, err := st.Dashboard(ctx, all, 24*time.Hour, forever)
		if err != nil || after.Received != d.Received || after.Sent != d.Sent || after.Deferrals != d.Deferrals || len(after.Failures) != len(d.Failures) {
			t.Errorf("dashboard changed after Optimize: %+v, %v", after, err)
		}

		r := all
		r.Server = "relay-2"
		d, err = st.Dashboard(ctx, r, 24*time.Hour, forever)
		if err != nil {
			t.Fatal(err)
		}
		if d.Received != 1 || d.Sent != 1 || d.Queue != 0 {
			t.Errorf("relay-2 dashboard = %+v", d)
		}
	})
}

func TestProvidersAndReasons(t *testing.T) {
	each(t, func(t *testing.T, st *store.Store) {
		stats, err := st.Providers(ctx, all)
		if err != nil {
			t.Fatal(err)
		}
		var ms store.ProviderStats
		for _, s := range stats {
			if s.Provider == "microsoft" {
				ms = s
			}
		}
		if ms.Attempts != 4 || ms.Sent != 2 || ms.Deferred != 2 || len(ms.Responses) != 1 || ms.Responses[0].DSN != "4.7.650" {
			t.Errorf("microsoft = %+v", ms)
		}
		reasons, err := st.DeferralReasons(ctx, all)
		if err != nil {
			t.Fatal(err)
		}
		if len(reasons) == 0 || reasons[0].Count < 2 {
			t.Errorf("reasons = %+v", reasons)
		}
		queue, err := st.Queue(ctx, all, forever, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(queue) != 1 || queue[0].QueueID != "4D5E6F7081" || queue[0].Pending != 1 || queue[0].DSN != "4.7.0" {
			t.Errorf("queue = %+v", queue)
		}
	})
}

func TestImportingAgainChangesNothing(t *testing.T) {
	each(t, func(t *testing.T, st *store.Store) {
		before := len(messages(t, st, "", ""))
		if before != 15 {
			t.Fatalf("%d messages after the first load, want 15", before)
		}
		if n := storetest.Load(t, st, "testdata/scenarios.log"); n != 0 {
			t.Errorf("second load added %d lines, want 0", n)
		}
		if after := len(messages(t, st, "", "")); after != before {
			t.Errorf("%d messages after loading again, want %d", after, before)
		}
		if r := recipient(t, one(t, st, "relay-1", "1A2B3C4D5E"), "bob@outlook.example"); r.Attempts != 3 {
			t.Errorf("bob has %d attempts after loading again, want 3", r.Attempts)
		}
	})
}

func TestPurge(t *testing.T) {
	each(t, func(t *testing.T, st *store.Store) {
		n, err := st.Purge(ctx, time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC))
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 || len(messages(t, st, "relay-1", "5E6F708192")) != 0 || len(messages(t, st, "", "")) != 14 {
			t.Errorf("purged %d, want the one message received on 13th October", n)
		}
	})
}
