// Package agent runs on a Postfix host and ships its mail log lines, and the captures of a local milter, to a
// Mailinois server over HTTPS. Batches the server can't take wait in a spool directory and go oldest first, so
// an outage or a restart loses nothing until the spool is full.
//
// Nothing that Postfix waits on waits on the network: syslog frames and captures go into buffers that drop and
// count when full, and batches the server isn't ready for go to disk.
package agent

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SamJUK/mailinois/internal/capture"
	"github.com/SamJUK/mailinois/internal/syslog"
)

const (
	maxBatch = 8 << 20 // the server's limit, before compression
	flushAt  = 4 << 20 // estimated size that sends a batch early, leaving room for JSON escaping
)

// Batch is what the agent sends and the server's ingest endpoint reads.
type Batch struct {
	Server   string            `json:"server,omitempty"` // overrides the host field of every line (for logs without one, like docker logs)
	Lines    []string          `json:"lines,omitempty"`  // raw syslog frames or log file lines, as received
	Captures []capture.Message `json:"captures,omitempty"`
}

type Config struct {
	URL          string // Mailinois server, e.g. https://mailinois.example.com
	Token        string
	ServerName   string        // optional: Batch.Server
	Files        []string      // log files to follow, e.g. /var/log/mail.log
	SyslogListen string        // optional, e.g. 127.0.0.1:5514: take rsyslog's forward locally (TCP and UDP)
	MilterListen string        // optional, e.g. 127.0.0.1:8892
	CaptureMode  capture.Mode  // for the milter
	MaxSize      int64         // bytes kept per captured message
	SpoolDir     string        // e.g. /var/lib/mailinois-agent
	SpoolLimit   int64         // bytes; oldest spooled batches are dropped past it (default 256 MiB)
	FlushEvery   time.Duration // default 2s
	Client       *http.Client  // nil: a client with a 30s timeout
}

type Agent struct {
	cfg      Config
	endpoint string
	spool    *spool
	lines    chan string
	captures chan capture.Message
	direct   chan pending // unbuffered: a batch goes straight out only when the shipper is idle

	// Lines and Captures count what was taken in, Sent the batches the server accepted. The Dropped counters
	// are what was lost: syslog frames and captures that found the buffer full (those senders never wait), and
	// batches dropped to keep the spool under its limit, refused by the server for good, or unwritable.
	Lines, Captures, Sent                         atomic.Int64
	LinesDropped, CapturesDropped, BatchesDropped atomic.Int64

	poll, retryMin, retryMax time.Duration
}

type pending struct {
	name string // its spool file name, given when the batch was made so it keeps its place
	body []byte // gzipped JSON
}

// Run is New, then the agent's Run.
func Run(ctx context.Context, cfg Config) error {
	a, err := New(cfg)
	if err != nil {
		return err
	}
	return a.Run(ctx)
}

// New checks cfg, fills in the defaults and opens the spool.
func New(cfg Config) (*Agent, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("agent: server URL %q: want https://host", cfg.URL)
	}
	switch {
	case cfg.Token == "":
		return nil, errors.New("agent: no token")
	case cfg.SpoolDir == "":
		return nil, errors.New("agent: no spool directory")
	case len(cfg.Files) == 0 && cfg.SyslogListen == "" && cfg.MilterListen == "":
		return nil, errors.New("agent: nothing to ship: give it log files, a syslog address or a milter address")
	}
	if u.Scheme == "http" {
		slog.Warn("the agent's server URL is plain HTTP: the token and the mail data travel unencrypted", "url", cfg.URL)
	}
	if cfg.SpoolLimit <= 0 {
		cfg.SpoolLimit = 256 << 20
	}
	if cfg.FlushEvery <= 0 {
		cfg.FlushEvery = 2 * time.Second
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 30 * time.Second}
	}
	sp, err := openSpool(cfg.SpoolDir, cfg.SpoolLimit)
	if err != nil {
		return nil, fmt.Errorf("agent: spool: %w", err)
	}
	return &Agent{
		cfg:      cfg,
		endpoint: strings.TrimRight(cfg.URL, "/") + "/api/v1/ingest",
		spool:    sp,
		lines:    make(chan string, 50_000),
		captures: make(chan capture.Message, 128),
		direct:   make(chan pending),
		poll:     500 * time.Millisecond,
		retryMin: time.Second,
		retryMax: time.Minute,
	}, nil
}

// Run ships until ctx ends or a listener fails, then spools whatever it still holds for the next start.
func (a *Agent) Run(parent context.Context) error {
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	if n, size := a.spool.stats(); n > 0 {
		slog.Info("sending the spool first", "batches", n, "bytes", size)
	}

	var inputs, rest sync.WaitGroup
	for _, path := range a.cfg.Files {
		inputs.Go(func() { a.follow(ctx, path) })
	}
	listen := func(name string, serve func() error) {
		inputs.Go(func() {
			if err := serve(); err != nil {
				cancel(fmt.Errorf("agent: %s: %w", name, err))
			}
		})
	}
	if addr := a.cfg.SyslogListen; addr != "" {
		listen("syslog tcp", func() error { return syslog.ListenTCP(ctx, addr, a.frame) })
		listen("syslog udp", func() error { return syslog.ListenUDP(ctx, addr, a.frame) })
	}
	if addr := a.cfg.MilterListen; addr != "" {
		listen("milter", func() error { return capture.Serve(ctx, addr, a.cfg.CaptureMode, a.cfg.MaxSize, a.captured) })
	}
	rest.Go(func() { a.ship(ctx) })
	rest.Go(func() { a.reportDrops(ctx) })
	a.batch(ctx, &inputs)
	rest.Wait()

	if parent.Err() != nil {
		return nil
	}
	return context.Cause(ctx)
}

// frame takes a syslog frame. It never waits: rsyslog's forward must not back up.
func (a *Agent) frame(s string) {
	select {
	case a.lines <- s:
		a.Lines.Add(1)
	default:
		a.LinesDropped.Add(1)
	}
}

// fileLine waits for room, since the file keeps what isn't read yet. It reports false once ctx has ended.
func (a *Agent) fileLine(ctx context.Context, s string) bool {
	select {
	case a.lines <- s:
		a.Lines.Add(1)
		return true
	case <-ctx.Done():
		return false
	}
}

// captured is the milter's sink, so it must never wait.
func (a *Agent) captured(m capture.Message) {
	select {
	case a.captures <- m:
		a.Captures.Add(1)
	default:
		a.CapturesDropped.Add(1)
	}
}

// batch collects lines and captures and hands a batch on every FlushEvery, or sooner when it nears the size
// cap. Once ctx ends it waits for the inputs to stop, and spools what they left.
func (a *Agent) batch(ctx context.Context, inputs *sync.WaitGroup) {
	b, size := Batch{Server: a.cfg.ServerName}, 0
	flush := func(final bool) {
		if len(b.Lines)+len(b.Captures) > 0 {
			a.hand(b, final)
		}
		b, size = Batch{Server: a.cfg.ServerName}, 0
	}
	tick := time.NewTicker(a.cfg.FlushEvery)
	defer tick.Stop()
	for {
		select {
		case l := <-a.lines:
			b.Lines = append(b.Lines, l)
			size += len(l) + 8
		case m := <-a.captures:
			b.Captures = append(b.Captures, m)
			size += (len(m.Header)+len(m.Body))*4/3 + 512
		case <-tick.C:
			flush(false)
			continue
		case <-ctx.Done():
			inputs.Wait()
			for len(a.lines) > 0 {
				b.Lines = append(b.Lines, <-a.lines)
			}
			for len(a.captures) > 0 {
				b.Captures = append(b.Captures, <-a.captures)
			}
			flush(true)
			return
		}
		if size >= flushAt {
			flush(false)
		}
	}
}

// hand passes each encoded batch to an idle shipper, or else to the spool. Once anything is spooled, new
// batches queue behind it, so the server gets them in order.
func (a *Agent) hand(b Batch, final bool) {
	bodies, err := encode(b)
	if err != nil {
		slog.Error("encode batch", "err", err)
		a.BatchesDropped.Add(1)
		return
	}
	for _, body := range bodies {
		p := pending{name: a.spool.newName(), body: body}
		if final || a.spool.len() > 0 {
			a.store(p)
			continue
		}
		select {
		case a.direct <- p:
		default:
			a.store(p)
		}
	}
}

func (a *Agent) store(p pending) {
	dropped, err := a.spool.put(p.name, p.body)
	if err != nil {
		slog.Error("can't spool a batch, dropping it", "err", err)
		a.BatchesDropped.Add(1)
	}
	if len(dropped) > 0 {
		var size int64
		for _, f := range dropped {
			size += f.size
		}
		slog.Warn("spool over its limit, dropped the oldest batches", "batches", len(dropped), "bytes", size,
			"oldest", dropped[0].name, "newest", dropped[len(dropped)-1].name, "limit", a.cfg.SpoolLimit)
		a.BatchesDropped.Add(int64(len(dropped)))
	}
}

// ship sends the spool oldest first, then batches as they come, backing off from 1s to 1m after a failure.
func (a *Agent) ship(ctx context.Context) {
	wait := a.retryMin
	failed := func() {
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		}
		wait = min(wait*2, a.retryMax)
	}
	for ctx.Err() == nil {
		if name, ok := a.spool.oldest(); ok {
			progressed := a.sendSpooled(ctx, name)
			a.spool.release()
			if progressed {
				wait = a.retryMin
			} else {
				failed()
			}
			continue
		}
		select {
		case <-ctx.Done():
		case <-a.spool.wake:
		case p := <-a.direct:
			switch a.send(ctx, p.body) {
			case sent:
				wait = a.retryMin
			case refused:
				a.BatchesDropped.Add(1)
			case tooBig:
				a.store(p) // the spool splits it
			case retry:
				a.store(p)
				failed()
			}
		}
	}
}

// sendSpooled reports whether the spool moved on: sent, split, or dropped for good.
func (a *Agent) sendSpooled(ctx context.Context, name string) bool {
	body, err := a.spool.read(name)
	if err != nil {
		slog.Error("unreadable spooled batch, dropping it", "file", name, "err", err)
		a.spool.remove(name)
		a.BatchesDropped.Add(1)
		return true
	}
	switch a.send(ctx, body) {
	case sent:
		a.spool.remove(name)
	case refused:
		a.spool.remove(name)
		a.BatchesDropped.Add(1)
	case tooBig:
		a.split(name, body)
	case retry:
		return false
	}
	return true
}

// split replaces a spooled batch the server found too big with its two halves, in its place in the queue.
func (a *Agent) split(name string, body []byte) {
	b, err := decode(body)
	halves := halve(b)
	if err != nil || halves == nil {
		slog.Error("the server refused a batch as too big and it can't be split, dropping it", "file", name, "bytes", len(body), "err", err)
		a.spool.remove(name)
		a.BatchesDropped.Add(1)
		return
	}
	for i, h := range halves {
		bodies, err := encode(h)
		if err != nil {
			slog.Error("encode batch", "err", err)
			a.BatchesDropped.Add(1)
			continue
		}
		for j, body := range bodies {
			a.store(pending{name: fmt.Sprintf("%s-%d-%d%s", strings.TrimSuffix(name, spoolExt), i+1, j+1, spoolExt), body: body})
		}
	}
	a.spool.remove(name)
}

type outcome int

const (
	sent    outcome = iota
	retry           // network, 5xx, 401/403, 408, 429
	tooBig          // 413
	refused         // any other 4xx: sending it again won't help
)

func (a *Agent) send(ctx context.Context, body []byte) outcome {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint, bytes.NewReader(body))
	if err != nil {
		slog.Error("ingest request", "err", err)
		return retry
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	resp, err := a.cfg.Client.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("can't reach the server, spooling", "err", err)
		}
		return retry
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	switch c := resp.StatusCode; {
	case c >= 200 && c < 300:
		a.Sent.Add(1)
		return sent
	case c == http.StatusUnauthorized || c == http.StatusForbidden:
		slog.Error("the server refused the agent's token: check it, and that it has the ingest scope. Batches wait in the spool meanwhile",
			"status", c, "spooled", a.spool.len())
		return retry
	case c == http.StatusRequestEntityTooLarge:
		return tooBig
	case c == http.StatusRequestTimeout || c == http.StatusTooManyRequests || c >= 500:
		slog.Warn("the server can't take batches right now, spooling", "status", c)
		return retry
	default:
		slog.Error("the server refused a batch, dropping it", "status", c, "response", string(msg))
		return refused
	}
}

// reportDrops logs a summary each minute in which something was lost.
func (a *Agent) reportDrops(ctx context.Context) {
	var lines, captures, batches int64
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		l, c, b := a.LinesDropped.Load(), a.CapturesDropped.Load(), a.BatchesDropped.Load()
		if l != lines || c != captures || b != batches {
			slog.Warn("agent dropped data in the last minute", "lines", l-lines, "captures", c-captures, "batches", b-batches)
			lines, captures, batches = l, c, b
		}
	}
}

// encode gzips b's JSON, split into as many batches as it takes to keep each under maxBatch.
func encode(b Batch) ([][]byte, error) {
	var js bytes.Buffer
	enc := json.NewEncoder(&js)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(b); err != nil {
		return nil, err
	}
	if js.Len() > maxBatch {
		if halves := halve(b); halves != nil {
			x, err := encode(halves[0])
			if err != nil {
				return nil, err
			}
			y, err := encode(halves[1])
			return append(x, y...), err
		}
	}
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	w.Write(js.Bytes())
	if err := w.Close(); err != nil {
		return nil, err
	}
	return [][]byte{gz.Bytes()}, nil
}

func decode(body []byte) (Batch, error) {
	var b Batch
	r, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return b, err
	}
	return b, json.NewDecoder(r).Decode(&b)
}

// halve splits b into two batches of about the same number of items, keeping their order, or returns nil
// when b holds fewer than two.
func halve(b Batch) []Batch {
	n := len(b.Lines) + len(b.Captures)
	if n < 2 {
		return nil
	}
	k := n / 2
	x, y := Batch{Server: b.Server}, Batch{Server: b.Server}
	if k <= len(b.Lines) {
		x.Lines, y.Lines, y.Captures = b.Lines[:k], b.Lines[k:], b.Captures
	} else {
		k -= len(b.Lines)
		x.Lines, x.Captures, y.Captures = b.Lines, b.Captures[:k], b.Captures[k:]
	}
	return []Batch{x, y}
}
