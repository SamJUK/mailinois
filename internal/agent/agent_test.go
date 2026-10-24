package agent

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SamJUK/mailinois/internal/capture"
	"github.com/emersion/go-milter"
)

func TestNewChecksConfig(t *testing.T) {
	good := Config{URL: "https://mailinois.example.com", Token: "secret", Files: []string{"/var/log/mail.log"}, SpoolDir: t.TempDir()}
	if _, err := New(good); err != nil {
		t.Fatalf("New(%+v): %v", good, err)
	}
	for name, change := range map[string]func(*Config){
		"no URL":       func(c *Config) { c.URL = "" },
		"not a URL":    func(c *Config) { c.URL = "mailinois.example.com" },
		"no token":     func(c *Config) { c.Token = "" },
		"no spool":     func(c *Config) { c.SpoolDir = "" },
		"no inputs":    func(c *Config) { c.Files = nil },
		"ftp, somehow": func(c *Config) { c.URL = "ftp://mailinois.example.com" },
	} {
		cfg := good
		change(&cfg)
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: New returned no error", name)
		}
	}
}

func TestFollowShipsNewLines(t *testing.T) {
	s := newServer(t)
	path := filepath.Join(t.TempDir(), "mail.log")
	write(t, path, "history, already imported\n")
	a, _ := start(t, config(t, s, path))
	write(t, path, "one\ntwo\npart")
	eventually(t, "the complete lines", func() bool { return slices.Equal(s.lines(), []string{"one", "two"}) })
	time.Sleep(10 * a.poll)
	if got := s.lines(); !slices.Equal(got, []string{"one", "two"}) {
		t.Fatalf("got %q before the last line ended, want only the complete lines", got)
	}
	write(t, path, "ial\r\n")
	eventually(t, "the finished line", func() bool { return slices.Equal(s.lines(), []string{"one", "two", "partial"}) })
	for _, b := range s.received() {
		if b.Server != "relay1" {
			t.Errorf("batch server %q, want relay1", b.Server)
		}
	}
	s.check(t)
}

func TestFollowSurvivesRenameRotation(t *testing.T) {
	s := newServer(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "mail.log")
	a, _ := start(t, config(t, s, path))
	write(t, path, "a1\na2\n") // the file appears after the start: all of it is new
	eventually(t, "lines before the rotation", func() bool { return len(s.lines()) == 2 })

	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	write(t, path+".1", "a3\n") // the writer hasn't reopened yet
	write(t, path, "b1\n")      // logrotate's create
	time.Sleep(3 * a.poll)
	write(t, path+".1", "a4\n") // still not reopened
	write(t, path, "b2\n")

	want := []string{"a1", "a2", "a3", "a4", "b1", "b2"}
	eventually(t, "every line once", func() bool { return sameSet(s.lines(), want) })
	time.Sleep(10 * a.poll)
	if got := s.lines(); !sameSet(got, want) {
		t.Errorf("got %q, want each of %q once", got, want)
	}
	s.check(t)
}

func TestFollowSurvivesCopytruncate(t *testing.T) {
	s := newServer(t)
	path := filepath.Join(t.TempDir(), "mail.log")
	a, _ := start(t, config(t, s, path))
	write(t, path, "c1 a longer line\nc2 a longer line\nc3 a longer line\n")
	eventually(t, "lines before the truncation", func() bool { return len(s.lines()) == 3 })

	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	write(t, path, "d1\n")

	want := []string{"c1 a longer line", "c2 a longer line", "c3 a longer line", "d1"}
	eventually(t, "the line after the truncation", func() bool { return slices.Equal(s.lines(), want) })
	time.Sleep(10 * a.poll)
	if got := s.lines(); !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
	s.check(t)
}

func TestSpoolsWhileDownThenSendsInOrder(t *testing.T) {
	s := newServer(t)
	s.status.Store(http.StatusServiceUnavailable)
	path := filepath.Join(t.TempDir(), "mail.log")
	cfg := config(t, s, path)
	a, _ := start(t, cfg)

	var want []string
	for i := range 4 {
		l1, l2 := fmt.Sprintf("line %d.1", i), fmt.Sprintf("line %d.2", i)
		write(t, path, l1+"\n"+l2+"\n")
		want = append(want, l1, l2)
		eventually(t, "the batch in the spool", func() bool { return slices.Contains(spooledLines(t, cfg.SpoolDir), l2) })
	}
	if n := len(s.received()); n != 0 {
		t.Fatalf("the server took %d batches while answering 503", n)
	}
	if n, _ := a.spool.stats(); n < 2 {
		t.Fatalf("%d batches spooled, want several", n)
	}

	s.status.Store(http.StatusAccepted)
	eventually(t, "every line, in order", func() bool { return slices.Equal(s.lines(), want) })
	eventually(t, "an empty spool", func() bool { return len(spoolFiles(t, cfg.SpoolDir)) == 0 })
	if a.BatchesDropped.Load() != 0 {
		t.Errorf("%d batches dropped", a.BatchesDropped.Load())
	}
	s.check(t)
}

func TestRestartSendsTheSpoolFirst(t *testing.T) {
	s := newServer(t)
	s.status.Store(http.StatusServiceUnavailable)
	path := filepath.Join(t.TempDir(), "mail.log")
	cfg := config(t, s, path)
	_, stop := start(t, cfg)
	for _, l := range []string{"old 1", "old 2", "old 3"} {
		write(t, path, l+"\n")
		eventually(t, "the batch in the spool", func() bool { return slices.Contains(spooledLines(t, cfg.SpoolDir), l) })
	}
	stop()
	if n := len(spoolFiles(t, cfg.SpoolDir)); n != 3 {
		t.Fatalf("%d files in the spool after the stop, want 3", n)
	}

	s.status.Store(http.StatusAccepted)
	start(t, cfg)
	write(t, path, "new 1\n")
	eventually(t, "the spool, then the new line", func() bool {
		return slices.Equal(s.lines(), []string{"old 1", "old 2", "old 3", "new 1"})
	})
	s.check(t)
}

func TestSpoolLimitDropsTheOldest(t *testing.T) {
	s := newServer(t)
	s.status.Store(http.StatusServiceUnavailable)
	path := filepath.Join(t.TempDir(), "mail.log")
	lines := make([]string, 5)
	sizes := make([]int64, 5)
	for i := range lines {
		lines[i] = fmt.Sprintf("%d %s", i, strings.Repeat("abc", 100))
		one, err := encode(Batch{Server: "relay1", Lines: lines[i : i+1]})
		if err != nil {
			t.Fatal(err)
		}
		sizes[i] = int64(len(one[0]))
	}
	cfg := config(t, s, path)
	cfg.SpoolLimit = sizes[3] + sizes[4] + sizes[0]/2 // room for two batches, not three

	a, stop := startWith(t, cfg, time.Hour, time.Hour) // one failed send, then quiet: nothing is in flight
	for _, l := range lines {
		write(t, path, l+"\n")
		eventually(t, "the batch in the spool", func() bool { return slices.Contains(spooledLines(t, cfg.SpoolDir), l) })
	}
	if got := spooledLines(t, cfg.SpoolDir); !slices.Equal(got, lines[3:]) {
		t.Errorf("spooled %d lines %.20q, want the newest two", len(got), got)
	}
	if n := a.BatchesDropped.Load(); n != 3 {
		t.Errorf("%d batches dropped, want 3", n)
	}
	stop()

	s.status.Store(http.StatusAccepted)
	cfg.SpoolLimit = 0
	start(t, cfg)
	eventually(t, "the newest two", func() bool { return slices.Equal(s.lines(), lines[3:]) })
	s.check(t)
}

func TestSendsGzipWithBearerToken(t *testing.T) {
	s := newServer(t)
	path := filepath.Join(t.TempDir(), "mail.log")

	wrong := config(t, s, path)
	wrong.Token = "not the token"
	a, stop := start(t, wrong)
	write(t, path, "while the token is wrong\n")
	eventually(t, "a refused request", func() bool { return s.refused.Load() > 0 })
	eventually(t, "the batch kept in the spool", func() bool {
		return slices.Equal(spooledLines(t, wrong.SpoolDir), []string{"while the token is wrong"})
	})
	if len(s.received()) != 0 || a.BatchesDropped.Load() != 0 {
		t.Fatalf("with a wrong token: %d batches taken, %d dropped; want none", len(s.received()), a.BatchesDropped.Load())
	}
	stop()

	right := wrong
	right.Token = "secret"
	start(t, right)
	write(t, path, "with the right token\n")
	eventually(t, "both lines", func() bool {
		return slices.Equal(s.lines(), []string{"while the token is wrong", "with the right token"})
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.requests {
		if r.auth != "Bearer secret" || r.encoding != "gzip" || r.contentType != "application/json" || !bytes.HasPrefix(r.body, []byte{0x1f, 0x8b}) {
			t.Errorf("accepted request: Authorization %q, Content-Encoding %q, Content-Type %q, body starts %x",
				r.auth, r.encoding, r.contentType, r.body[:min(2, len(r.body))])
		}
	}
}

func TestSplitsBatchesTheServerFindsTooBig(t *testing.T) {
	s := newServer(t)
	s.maxItems.Store(2)
	path := filepath.Join(t.TempDir(), "mail.log")
	cfg := config(t, s, path)
	cfg.FlushEvery = 300 * time.Millisecond
	start(t, cfg)
	want := []string{"x1", "x2", "x3", "x4", "x5"}
	write(t, path, strings.Join(want, "\n")+"\n")

	eventually(t, "every line, in order", func() bool { return slices.Equal(s.lines(), want) })
	if s.tooBig.Load() == 0 {
		t.Error("the server never answered 413, so nothing was split")
	}
	eventually(t, "an empty spool", func() bool { return len(spoolFiles(t, cfg.SpoolDir)) == 0 })
	s.check(t)
}

func TestEncodeKeepsBatchesUnderTheCap(t *testing.T) {
	line := strings.Repeat("x", 100_000)
	b := Batch{Server: "relay1"}
	for range 100 {
		b.Lines = append(b.Lines, line)
	}
	bodies, err := encode(b)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, body := range bodies {
		zr, _ := gzip.NewReader(bytes.NewReader(body))
		js, _ := io.ReadAll(zr)
		if len(js) > maxBatch {
			t.Errorf("a batch of %d bytes, over the %d cap", len(js), maxBatch)
		}
		got, err := decode(body)
		if err != nil || got.Server != "relay1" {
			t.Fatalf("decode: %v, server %q", err, got.Server)
		}
		lines = append(lines, got.Lines...)
	}
	if len(bodies) < 2 || !slices.Equal(lines, b.Lines) {
		t.Errorf("%d batches with %d lines, want at least 2 with all %d", len(bodies), len(lines), len(b.Lines))
	}
}

func TestShipsSyslogAndCaptures(t *testing.T) {
	s := newServer(t)
	cfg := config(t, s)
	cfg.SyslogListen, cfg.MilterListen, cfg.CaptureMode = freeAddr(t), freeAddr(t), capture.ModeHeaders
	start(t, cfg)

	tcp, err := dialRetry("tcp", cfg.SyslogListen)
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	fmt.Fprint(tcp, "<22>Oct 18 19:39:48 relay postfix/qmgr[1042]: 0DFD162667: removed\n")
	eventually(t, "the TCP frame", func() bool { return len(s.lines()) == 1 })
	udp, err := net.Dial("udp", cfg.SyslogListen)
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	fmt.Fprint(udp, "<22>Oct 18 19:39:47 relay postfix/qmgr[1042]: 0DFD162666: removed")

	m := milter.NewClientWithOptions("tcp", cfg.MilterListen, milter.ClientOptions{ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second})
	var ms *milter.ClientSession
	for range 50 {
		if ms, err = m.Session(); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer ms.Close()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(ms.Macros(milter.CodeConn, "j", "mx.example.com"))
	_, err = ms.Conn("client.example.net", milter.FamilyInet, 49152, "192.0.2.1")
	must(err)
	must(ms.Macros(milter.CodeHeader, "i", "4AB1C2D3E4"))
	_, err = ms.HeaderField("Message-ID", " <m1@example.com>")
	must(err)
	must(ms.Macros(milter.CodeEOB, "i", "4AB1C2D3E4"))
	_, _, err = ms.End()
	must(err)

	eventually(t, "both frames and the capture", func() bool {
		var caps []capture.Message
		for _, b := range s.received() {
			caps = append(caps, b.Captures...)
		}
		return len(s.lines()) == 2 && len(caps) == 1 && caps[0].QueueID == "4AB1C2D3E4" && caps[0].Server == "mx.example.com" &&
			string(caps[0].Header) == "Message-ID: <m1@example.com>\r\n\r\n"
	})
	s.check(t)
}

func TestBuffersDropWhenFull(t *testing.T) {
	a, err := New(Config{URL: "https://mailinois.example.com", Token: "secret", SyslogListen: "127.0.0.1:0", SpoolDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	a.lines, a.captures = make(chan string, 1), make(chan capture.Message, 1)
	a.frame("first")
	a.frame("second")
	a.captured(capture.Message{QueueID: "A"})
	a.captured(capture.Message{QueueID: "B"})
	if a.Lines.Load() != 1 || a.LinesDropped.Load() != 1 || a.Captures.Load() != 1 || a.CapturesDropped.Load() != 1 {
		t.Errorf("lines %d, dropped %d; captures %d, dropped %d; want one of each taken and one dropped",
			a.Lines.Load(), a.LinesDropped.Load(), a.Captures.Load(), a.CapturesDropped.Load())
	}
	if l, m := <-a.lines, <-a.captures; l != "first" || m.QueueID != "A" {
		t.Errorf("kept %q and %q, want the first of each", l, m.QueueID)
	}
}

type request struct {
	auth, encoding, contentType string
	body                        []byte
}

type fakeServer struct {
	*httptest.Server
	status   atomic.Int64
	maxItems atomic.Int64 // 413 past this many lines and captures; 0: no limit
	refused  atomic.Int64
	tooBig   atomic.Int64

	mu       sync.Mutex
	batches  []Batch
	requests []request
	problems []string
}

func newServer(t *testing.T) *fakeServer {
	s := &fakeServer{}
	s.status.Store(http.StatusAccepted)
	s.Server = httptest.NewTLSServer(http.HandlerFunc(s.ingest))
	t.Cleanup(s.Close)
	return s
}

func (s *fakeServer) ingest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/api/v1/ingest" {
		s.problem("%s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("Authorization") != "Bearer secret" {
		s.refused.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if code := int(s.status.Load()); code != http.StatusAccepted {
		w.WriteHeader(code)
		return
	}
	body, _ := io.ReadAll(r.Body)
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		s.problem("body isn't gzip: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var b Batch
	if err := json.NewDecoder(zr).Decode(&b); err != nil {
		s.problem("body isn't a batch: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if n := s.maxItems.Load(); n > 0 && int64(len(b.Lines)+len(b.Captures)) > n {
		s.tooBig.Add(1)
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}
	s.mu.Lock()
	s.batches = append(s.batches, b)
	s.requests = append(s.requests, request{r.Header.Get("Authorization"), r.Header.Get("Content-Encoding"), r.Header.Get("Content-Type"), body})
	s.mu.Unlock()
	w.WriteHeader(http.StatusAccepted)
}

func (s *fakeServer) problem(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.problems = append(s.problems, fmt.Sprintf(format, args...))
}

func (s *fakeServer) check(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.problems {
		t.Error(p)
	}
}

func (s *fakeServer) received() []Batch {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.batches)
}

func (s *fakeServer) lines() []string {
	var lines []string
	for _, b := range s.received() {
		lines = append(lines, b.Lines...)
	}
	return lines
}

func config(t *testing.T, s *fakeServer, files ...string) Config {
	return Config{
		URL:        s.URL,
		Token:      "secret",
		ServerName: "relay1",
		Files:      files,
		SpoolDir:   t.TempDir(),
		FlushEvery: 20 * time.Millisecond,
		Client:     s.Client(),
	}
}

func start(t *testing.T, cfg Config) (*Agent, func()) {
	t.Helper()
	return startWith(t, cfg, 10*time.Millisecond, 100*time.Millisecond)
}

func startWith(t *testing.T, cfg Config, retryMin, retryMax time.Duration) (*Agent, func()) {
	t.Helper()
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	a.poll, a.retryMin, a.retryMax = 10*time.Millisecond, retryMin, retryMax
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	time.Sleep(100 * time.Millisecond) // the followers have taken their place at the end of their files
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("Run: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Error("Run didn't return after its context ended")
			}
		})
	}
	t.Cleanup(stop)
	return a, stop
}

func write(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func spoolFiles(t *testing.T, dir string) []string {
	t.Helper()
	names, err := filepath.Glob(filepath.Join(dir, "*"+spoolExt))
	if err != nil {
		t.Fatal(err)
	}
	return names
}

// spooledLines reads the spool oldest first. Files that vanish while it reads were just sent or dropped.
func spooledLines(t *testing.T, dir string) []string {
	var lines []string
	for _, name := range spoolFiles(t, dir) {
		body, err := os.ReadFile(name)
		if err != nil {
			continue
		}
		b, err := decode(body)
		if err != nil {
			t.Errorf("spool file %s: %v", filepath.Base(name), err)
			continue
		}
		lines = append(lines, b.Lines...)
	}
	return lines
}

func sameSet(got, want []string) bool {
	got, want = slices.Clone(got), slices.Clone(want)
	slices.Sort(got)
	slices.Sort(want)
	return reflect.DeepEqual(got, want)
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func dialRetry(network, addr string) (net.Conn, error) {
	var err error
	for range 50 {
		var c net.Conn
		if c, err = net.Dial(network, addr); err == nil {
			return c, nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil, err
}
