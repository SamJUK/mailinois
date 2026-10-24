package store_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SamJUK/mailinois/internal/postfix"
	"github.com/SamJUK/mailinois/internal/store"
	"github.com/SamJUK/mailinois/internal/syslog"
)

// The sizing numbers in the docs come from these:
//
//	go test ./internal/store -run '^$' -bench . -benchtime 1x
//
// MAILINOIS_BENCH_MESSAGES sets the size (default 20,000 messages, five lines each), and
// MAILINOIS_TEST_DATABASE_URL runs them on PostgreSQL instead of SQLite.

func benchMessages(b *testing.B) int {
	n, err := strconv.Atoi(os.Getenv("MAILINOIS_BENCH_MESSAGES"))
	if err != nil || n <= 0 {
		return 20_000
	}
	return n
}

// traffic is n messages spread over the 30 days before now, as Postfix logs them: client, message-id, from,
// one delivery attempt (one in ten deferred first), removed.
func traffic(n int, now time.Time) []postfix.Event {
	providers := []string{"gmail.com", "outlook.com", "yahoo.com", "icloud.com", "example.org"}
	var events []postfix.Event
	add := func(at time.Time, program, msg string) {
		e, ok := postfix.Parse(syslog.Line{Host: "relay-1", Time: at, Program: program, Msg: msg})
		if !ok {
			panic("unparsed: " + msg)
		}
		events = append(events, e)
	}
	step := 30 * 24 * time.Hour / time.Duration(n)
	for i := range n {
		at := now.Add(-30 * 24 * time.Hour).Add(time.Duration(i) * step)
		qid := fmt.Sprintf("%010X", i+1)
		to := fmt.Sprintf("user%d@%s", i%5000, providers[i%len(providers)])
		add(at, "postfix/smtpd", qid+": client=app-1[10.1.0.4]")
		add(at, "postfix/cleanup", qid+": message-id=<"+qid+"@acme.test>")
		add(at, "postfix/qmgr", qid+": from=<hello@acme.test>, size=4200, nrcpt=1 (queue active)")
		if i%10 == 0 {
			add(at.Add(time.Second), "postfix/smtp", qid+": to=<"+to+">, relay=mx."+providers[i%len(providers)]+"[198.51.100.7]:25, delay=1, delays=0/0/0/1, dsn=4.7.650, status=deferred (451 4.7.650 temporarily rate limited)")
		}
		add(at.Add(2*time.Second), "postfix/smtp", qid+": to=<"+to+">, relay=mx."+providers[i%len(providers)]+"[198.51.100.7]:25, delay=2, delays=0/0/1/1, dsn=2.0.0, status=sent (250 2.0.0 OK)")
		add(at.Add(2*time.Second), "postfix/qmgr", qid+": removed")
	}
	return events
}

// benchStore opens a fresh database, and returns the SQLite file's path ("" on PostgreSQL).
func benchStore(b *testing.B) (*store.Store, string) {
	b.Helper()
	dsn, path := os.Getenv("MAILINOIS_TEST_DATABASE_URL"), ""
	if dsn == "" {
		path = filepath.Join(b.TempDir(), "bench.db")
		dsn = "sqlite:" + path
	}
	st, err := store.Open(context.Background(), dsn)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(st.Close)
	return st, path
}

// ingest applies events in the pipeline's batches of 500.
func ingest(b *testing.B, st *store.Store, events []postfix.Event) {
	b.Helper()
	for i := 0; i < len(events); i += 500 {
		if _, err := st.Ingest(context.Background(), events[i:min(i+500, len(events))]); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkIngest(b *testing.B) {
	events := traffic(benchMessages(b), time.Now())
	var size int64
	for b.Loop() {
		b.StopTimer()
		st, path := benchStore(b)
		b.StartTimer()
		ingest(b, st, events)
		b.StopTimer()
		st.Close() // the last connection closing folds the WAL into the file
		if fi, err := os.Stat(path); err == nil && path != "" {
			size = fi.Size()
		}
		b.StartTimer()
	}
	b.ReportMetric(float64(len(events)*b.N)/b.Elapsed().Seconds(), "lines/s")
	if size > 0 {
		b.ReportMetric(float64(size)/1e6, "MB")
	}
}

func BenchmarkQueries(b *testing.B) {
	now := time.Now()
	st, _ := benchStore(b)
	ingest(b, st, traffic(benchMessages(b), now))
	if err := st.Optimize(context.Background()); err != nil { // as the server does every hour
		b.Fatal(err)
	}
	month := store.Filter{Since: now.Add(-30 * 24 * time.Hour), Until: now}
	b.Run("dashboard-30d", func(b *testing.B) {
		for b.Loop() {
			if _, err := st.Dashboard(context.Background(), month, 24*time.Hour, 120*time.Hour); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("dashboard-30d-one-server", func(b *testing.B) {
		f := month
		f.Server = "relay-1"
		for b.Loop() {
			if _, err := st.Dashboard(context.Background(), f, 24*time.Hour, 120*time.Hour); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("dashboard-24h", func(b *testing.B) {
		day := store.Filter{Since: now.Add(-24 * time.Hour), Until: now}
		for b.Loop() {
			if _, err := st.Dashboard(context.Background(), day, time.Hour, 120*time.Hour); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("search-30d", func(b *testing.B) {
		f := month
		f.Terms = store.ParseSearch("to:outlook.com status:deferred", now).Terms
		for b.Loop() {
			if _, err := st.Messages(context.Background(), f, 0, 50); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkPurge times the hourly purge's worst case, a day of thirty, while ingest keeps running, and reports
// how long the slowest ingest batch waited meanwhile.
func BenchmarkPurge(b *testing.B) {
	ctx := context.Background()
	now := time.Now()
	st, _ := benchStore(b)
	ingest(b, st, traffic(benchMessages(b), now))
	if err := st.Optimize(ctx); err != nil {
		b.Fatal(err)
	}
	var stop atomic.Bool
	var worst atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		for n := 0; !stop.Load(); n++ {
			batch := make([]postfix.Event, 0, 500)
			for j := range 500 {
				e, _ := postfix.Parse(syslog.Line{Host: "relay-2", Time: time.Now(), Program: "postfix/qmgr", Msg: fmt.Sprintf("%010X: removed", 0xF000000000+n*500+j)})
				batch = append(batch, e)
			}
			start := time.Now()
			if _, err := st.Ingest(ctx, batch); err != nil {
				b.Error(err)
				return
			}
			if d := int64(time.Since(start)); d > worst.Load() {
				worst.Store(d)
			}
		}
	}()
	b.ResetTimer()
	purged, err := st.Purge(ctx, now.Add(-29*24*time.Hour), now.Add(-29*24*time.Hour))
	b.StopTimer()
	stop.Store(true)
	<-done
	if err != nil {
		b.Fatal(err)
	}
	b.ReportMetric(float64(purged), "messages")
	b.ReportMetric(float64(worst.Load())/1e6, "ms-slowest-ingest-batch")
}
