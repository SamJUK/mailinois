// Package ingest batches parsed log lines into the store. Nothing here ever blocks a sender: when the
// buffer is full, or the database is gone for longer than the buffer lasts, lines are dropped and counted.
package ingest

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SamJUK/mailinois/internal/postfix"
	"github.com/SamJUK/mailinois/internal/store"
	"github.com/SamJUK/mailinois/internal/syslog"
)

const batchSize = 500

type Pipeline struct {
	store    *store.Store
	loc      *time.Location
	ch       chan postfix.Event
	captures chan store.Capture

	Frames, Unparsed, Dropped, Errors, Captured atomic.Int64

	mu       sync.Mutex
	outcomes map[[2]string]int64 // server, status
}

// New makes a pipeline that holds up to buffer lines waiting for the database (0: 50,000).
func New(st *store.Store, loc *time.Location, buffer int) *Pipeline {
	if buffer <= 0 {
		buffer = 50_000
	}
	return &Pipeline{store: st, loc: loc, ch: make(chan postfix.Event, buffer), captures: make(chan store.Capture, 1000), outcomes: map[[2]string]int64{}}
}

// Frame takes one syslog frame.
func (p *Pipeline) Frame(frame string) { p.FrameFrom(frame, "") }

// FrameFrom takes a frame, naming its server when server isn't empty (lines without a host, like docker logs).
func (p *Pipeline) FrameFrom(frame, server string) {
	p.Frames.Add(1)
	l, ok := syslog.Parse(frame, p.loc, time.Now())
	if !ok {
		p.Unparsed.Add(1)
		return
	}
	if server != "" {
		l.Host = server
	}
	p.Line(l)
}

// FrameWait is FrameFrom for senders that can send again later, the agent and log shippers: it waits for room
// until ctx ends, and reports false if the line wasn't queued, so they keep it rather than lose it. Syslog and
// the milter never wait, since nothing behind them may back up.
func (p *Pipeline) FrameWait(ctx context.Context, frame, server string) bool {
	p.Frames.Add(1)
	l, ok := syslog.Parse(frame, p.loc, time.Now())
	if !ok {
		p.Unparsed.Add(1)
		return true
	}
	if server != "" {
		l.Host = server
	}
	e, ok := postfix.Parse(l)
	if !ok {
		return true
	}
	select {
	case p.ch <- e:
		return true
	case <-ctx.Done():
		return false
	}
}

// CaptureWait is Capture for the agent's captures: it waits for room until ctx ends.
func (p *Pipeline) CaptureWait(ctx context.Context, c store.Capture) bool {
	select {
	case p.captures <- c:
		return true
	case <-ctx.Done():
		return false
	}
}

// Capture queues a captured message for saving.
func (p *Pipeline) Capture(c store.Capture) {
	select {
	case p.captures <- c:
	default:
		p.Dropped.Add(1)
	}
}

func (p *Pipeline) Line(l syslog.Line) {
	e, ok := postfix.Parse(l)
	if !ok {
		return
	}
	select {
	case p.ch <- e:
	default:
		p.Dropped.Add(1)
	}
}

func (p *Pipeline) Run(ctx context.Context) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case c := <-p.captures:
				if err := p.store.SaveCapture(ctx, c); err != nil {
					p.Errors.Add(1)
					slog.Error("save capture", "queue_id", c.QueueID, "err", err)
				} else {
					p.Captured.Add(1)
				}
			}
		}
	}()
	batch := make([]postfix.Event, 0, batchSize)
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			if len(batch) > 0 {
				done, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				p.flush(done, batch)
				cancel()
			}
			return
		case e := <-p.ch:
			batch = append(batch, e)
			if len(batch) < batchSize {
				continue
			}
		case <-tick.C:
			if len(batch) == 0 {
				continue
			}
		}
		p.flush(ctx, batch)
		batch = batch[:0]
	}
}

// flush waits out a database outage (the channel buffers meanwhile), and when the database is up but the
// batch still fails, applies the lines one at a time so one bad line costs only itself.
func (p *Pipeline) flush(ctx context.Context, batch []postfix.Event) {
	for wait := time.Second; ; wait = min(wait*2, 30*time.Second) {
		_, err := p.store.Ingest(ctx, batch)
		if err == nil {
			p.count(batch)
			return
		}
		p.Errors.Add(1)
		if ctx.Err() != nil {
			p.Dropped.Add(int64(len(batch)))
			return
		}
		if p.store.Ping(ctx) == nil {
			slog.Warn("ingest batch failed, retrying line by line", "err", err)
			for _, e := range batch {
				if _, err := p.store.Ingest(ctx, []postfix.Event{e}); err != nil {
					p.Errors.Add(1)
					p.Dropped.Add(1)
					slog.Error("ingest line dropped", "err", err)
				} else {
					p.count([]postfix.Event{e})
				}
			}
			return
		}
		slog.Error("database unavailable, holding lines", "err", err, "buffered", len(p.ch), "retry_in", wait)
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		}
	}
}

func (p *Pipeline) count(batch []postfix.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range batch {
		if e.Kind == postfix.Delivery || e.Kind == postfix.Reject || e.Kind == postfix.Expired {
			p.outcomes[[2]string{e.Server, e.Status}]++
		}
	}
}

// Outcomes is a copy of the delivery outcome counters since start, by server and status.
func (p *Pipeline) Outcomes() map[[2]string]int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[[2]string]int64, len(p.outcomes))
	for k, v := range p.outcomes {
		out[k] = v
	}
	return out
}

func (p *Pipeline) Buffered() int { return len(p.ch) }
