package store_test

import (
	"slices"
	"testing"

	"github.com/SamJUK/mailinois/internal/postfix"
	"github.com/SamJUK/mailinois/internal/store"
	"github.com/SamJUK/mailinois/internal/store/storetest"
)

func TestExactSenderSites(t *testing.T) {
	storetest.Each(t, func(t *testing.T, st *store.Store) {
		var events []postfix.Event
		for i, sender := range []string{"STATUS@WIDGETS.EXAMPLE", "forms@widgets.example", "not-status@widgets.example", "status@widgets.example.attacker.invalid", ""} {
			qid := []string{"AAAAAA01", "AAAAAA02", "AAAAAA03", "AAAAAA04", "AAAAAA05"}[i]
			events = append(events, postfix.Event{Server: "relay", At: all.Since, QueueID: qid, Kind: postfix.Queued, Sender: sender, Raw: qid + ": from=<" + sender + ">"})
			if sender == "" {
				events = append(events, postfix.Event{Server: "relay", At: all.Since, QueueID: qid, Kind: postfix.Delivery, To: "status@widgets.example", Status: "sent", Raw: qid + ": to=<status@widgets.example>"})
			}
		}
		if _, err := st.Ingest(ctx, events); err != nil {
			t.Fatal(err)
		}
		f := all
		f.Allowed = []store.Scope{{Senders: []string{"status@widgets.example"}}}
		if got, want := queueIDs(t, st, f), []string{"AAAAAA01", "AAAAAA05"}; !slices.Equal(got, want) {
			t.Fatalf("Status scope = %v, want %v (including its bounce, excluding shared-domain neighbours)", got, want)
		}
		f.Allowed = []store.Scope{{Senders: []string{"forms@widgets.example"}}}
		if got := queueIDs(t, st, f); !slices.Equal(got, []string{"AAAAAA02"}) {
			t.Fatalf("Forms scope = %v", got)
		}
	})
}
