package store_test

import (
	"testing"
	"time"

	"github.com/SamJUK/mailinois/internal/postfix"
	"github.com/SamJUK/mailinois/internal/store"
	"github.com/SamJUK/mailinois/internal/store/storetest"
	"github.com/SamJUK/mailinois/internal/syslog"
)

// rsyslog's disk queue replayed a message's lines with its pickup line last. The pickup was logged before the
// removal, so it joins the message; a pickup logged after the removal is the queue ID being reused.
func TestStartLineArrivingLate(t *testing.T) {
	t0 := all.Since.Add(time.Hour)
	var events []postfix.Event
	for _, line := range []struct {
		at      time.Duration
		program string
		msg     string
	}{
		{100 * time.Millisecond, "postfix/cleanup", "5E6F7A8B9C: message-id=<late@relay.example>"},
		{200 * time.Millisecond, "postfix/qmgr", "5E6F7A8B9C: from=<www-data@relay.example>, size=900, nrcpt=1 (queue active)"},
		{600 * time.Millisecond, "postfix/local", "5E6F7A8B9C: to=<ops@relay.example>, relay=local, delay=0.5, delays=0.1/0/0/0.4, dsn=2.0.0, status=sent (delivered to mailbox)"},
		{610 * time.Millisecond, "postfix/qmgr", "5E6F7A8B9C: removed"},
		{0, "postfix/pickup", "5E6F7A8B9C: uid=33 from=<www-data>"},
		{time.Hour, "postfix/pickup", "5E6F7A8B9C: uid=33 from=<www-data>"},
	} {
		e, ok := postfix.Parse(syslog.Line{Host: "relay", Time: t0.Add(line.at), Program: line.program, Msg: line.msg})
		if !ok {
			t.Fatal(line.msg)
		}
		events = append(events, e)
	}
	storetest.Each(t, func(t *testing.T, st *store.Store) {
		if _, err := st.Ingest(ctx, events); err != nil {
			t.Fatal(err)
		}
		ms := messages(t, st, "relay", "5E6F7A8B9C")
		if len(ms) != 2 {
			t.Fatalf("%d messages, want the late-started one and the reused queue ID", len(ms))
		}
		if late := ms[1]; late.ClientHost != "uid=33" || late.Status != "sent" || late.RemovedAt == nil {
			t.Errorf("late-started message = %+v", late)
		}
		if reused := ms[0]; reused.Status != "queued" || reused.RemovedAt != nil {
			t.Errorf("reused queue ID = %+v", reused)
		}
	})
}
