// Package storetest gives tests a store of their own: SQLite in a temporary directory, and PostgreSQL in a
// schema of its own when MAILINOIS_TEST_DATABASE_URL is set.
package storetest

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/SamJUK/mailinois/internal/postfix"
	"github.com/SamJUK/mailinois/internal/store"
	"github.com/SamJUK/mailinois/internal/syslog"
)

// Each runs fn as a subtest against every database available.
func Each(t *testing.T, fn func(t *testing.T, st *store.Store)) {
	t.Run("sqlite", func(t *testing.T) { fn(t, SQLite(t)) })
	if os.Getenv("MAILINOIS_TEST_DATABASE_URL") != "" {
		t.Run("postgres", func(t *testing.T) { fn(t, Postgres(t)) })
	}
}

func SQLite(t testing.TB) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), "sqlite:"+filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return st
}

func Postgres(t testing.TB) *store.Store {
	t.Helper()
	url := os.Getenv("MAILINOIS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("MAILINOIS_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("test_%d", time.Now().UnixNano())
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	st, err := store.Open(ctx, url+sep+"search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		st.Close()
		conn.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
		conn.Close(ctx)
	})
	return st
}

// Load ingests a log file in file order and returns how many lines were new.
func Load(t testing.TB, st *store.Store, path string) int {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var events []postfix.Event
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l, ok := syslog.Parse(sc.Text(), time.UTC, time.Now())
		if !ok {
			t.Fatalf("not syslog: %q", sc.Text())
		}
		if e, ok := postfix.Parse(l); ok {
			events = append(events, e)
		}
	}
	n, err := st.Ingest(context.Background(), events)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
