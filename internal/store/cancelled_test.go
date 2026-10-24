package store_test

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SamJUK/mailinois/internal/postfix"
	"github.com/SamJUK/mailinois/internal/store"
	"github.com/SamJUK/mailinois/internal/store/storetest"
	"github.com/SamJUK/mailinois/internal/syslog"
)

func cancelledEvents(t *testing.T) []postfix.Event {
	t.Helper()
	var events []postfix.Event
	for _, line := range [][2]string{
		{"postfix/smtpd", "6E59B63605: client=unknown[10.1.0.2]"},
		{"postfix/cleanup", "6E59B63605: removed (canceled)"},
		{"postfix/qmgr", "ABCDEF1234: from=<>, size=2900, nrcpt=1 (queue active)"},
	} {
		e, ok := postfix.Parse(syslog.Line{Host: "relay", Time: all.Since, Program: line[0], Msg: line[1]})
		if !ok {
			t.Fatal(line)
		}
		events = append(events, e)
	}
	return events
}

func checkCancelled(t *testing.T, st *store.Store) {
	t.Helper()
	cancelled := one(t, st, "relay", "6E59B63605")
	bounce := one(t, st, "relay", "ABCDEF1234")
	if cancelled.Status != "cancelled" || cancelled.RemovedAt == nil || cancelled.SenderKnown {
		t.Errorf("cancelled transaction: %+v", cancelled)
	}
	if bounce.Status != "queued" || bounce.RemovedAt != nil || !bounce.SenderKnown || bounce.Sender != "" {
		t.Errorf("real null-sender message: %+v", bounce)
	}
	queued, err := st.Queue(ctx, all, forever, 100)
	if err != nil || len(queued) != 1 || queued[0].QueueID != bounce.QueueID {
		t.Fatalf("queue = %+v, %v; want only the real bounce", queued, err)
	}
	d, err := st.Dashboard(ctx, all, forever/100, forever)
	if err != nil || d.Received != 1 || d.Queue != 1 {
		t.Fatalf("dashboard = %+v, %v; want one accepted and queued message", d, err)
	}
}

func TestCancelledSMTPTransaction(t *testing.T) {
	storetest.Each(t, func(t *testing.T, st *store.Store) {
		if _, err := st.Ingest(ctx, cancelledEvents(t)); err != nil {
			t.Fatal(err)
		}
		checkCancelled(t, st)
	})
}

func TestCancelledMigration(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "old.db")
		checkCancelledMigration(t, "sqlite:"+path, "sqlite", path)
	})
	if url := os.Getenv("MAILINOIS_TEST_DATABASE_URL"); url != "" {
		t.Run("postgres", func(t *testing.T) {
			db, err := sql.Open("pgx", url)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			schema := fmt.Sprintf("test_cancelled_%d", time.Now().UnixNano())
			if _, err := db.Exec("CREATE SCHEMA " + schema); err != nil {
				t.Fatal(err)
			}
			defer db.Exec("DROP SCHEMA " + schema + " CASCADE")
			sep := "?"
			if strings.Contains(url, "?") {
				sep = "&"
			}
			dsn := url + sep + "search_path=" + schema
			checkCancelledMigration(t, dsn, "pgx", dsn)
		})
	}
}

func checkCancelledMigration(t *testing.T, url, driver, dsn string) {
	t.Helper()
	st, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	events := cancelledEvents(t)
	events[1].Kind, events[1].Status = postfix.Notice, ""
	if _, err := st.Ingest(ctx, events); err != nil {
		t.Fatal(err)
	}
	st.Close()
	db, err := sql.Open(driver, dsn)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"DELETE FROM schema_migrations WHERE name = 'schema/004_cancelled_transactions.sql'", "DROP INDEX messages_cancelled"} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	st, err = store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	checkCancelled(t, st)
}
