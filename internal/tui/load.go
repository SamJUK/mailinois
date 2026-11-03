package tui

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SamJUK/mailinois/internal/follow"
	"github.com/SamJUK/mailinois/internal/postfix"
	"github.com/SamJUK/mailinois/internal/store"
	"github.com/SamJUK/mailinois/internal/syslog"
)

// Loader reads log files into the store: every file but the last once, then the last as it grows.
type Loader struct {
	Store  *store.Store
	Loc    *time.Location
	Server string // for lines without a host, such as docker logs
	Follow bool
	Poll   time.Duration // 0: a second

	Lines, Unparsed atomic.Int64
	Version         atomic.Int64 // goes up after every write, so screens know to read again
	Following       atomic.Bool  // still reading as the file or pipe grows
	loaded          atomic.Bool

	mu          sync.Mutex
	file        string
	first, last time.Time
	hosts       map[string]bool
	err         error
	batch       []postfix.Event
}

// Progress is what the loading screen and the top line show.
type Progress struct {
	File        string
	Lines       int64
	Unparsed    int64
	Loaded      bool
	First, Last time.Time
	Err         error
}

func (l *Loader) Progress() Progress {
	l.mu.Lock()
	defer l.mu.Unlock()
	return Progress{File: l.file, Lines: l.Lines.Load(), Unparsed: l.Unparsed.Load(), Loaded: l.loaded.Load(),
		First: l.first, Last: l.last, Err: l.err}
}

// Hosts are the server names seen in the log so far.
func (l *Loader) Hosts() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for h := range l.hosts {
		out = append(out, h)
	}
	return out
}

// Run reads files in order. It returns once everything is read and, when following, once ctx ends.
func (l *Loader) Run(ctx context.Context, files []string) {
	err := l.run(ctx, files)
	l.mu.Lock()
	l.err = err
	l.mu.Unlock()
	l.loaded.Store(true)
	l.Version.Add(1)
}

func (l *Loader) run(ctx context.Context, files []string) error {
	for i, path := range files {
		l.setFile(path)
		last := i == len(files)-1
		switch {
		case path == "-":
			if err := l.stream(ctx, os.Stdin); err != nil {
				return err
			}
		case last && l.Follow && !gzipped(path):
			return l.follow(ctx, path)
		default:
			if err := l.readFile(ctx, path); err != nil {
				return err
			}
		}
		if ctx.Err() != nil {
			return nil
		}
	}
	return nil
}

func (l *Loader) setFile(path string) {
	l.mu.Lock()
	l.file = path
	l.mu.Unlock()
}

func gzipped(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	magic := make([]byte, 2)
	n, _ := io.ReadFull(f, magic)
	return n == 2 && bytes.Equal(magic, []byte{0x1f, 0x8b})
}

func (l *Loader) readFile(ctx context.Context, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	br := bufio.NewReader(f)
	if magic, _ := br.Peek(2); bytes.Equal(magic, []byte{0x1f, 0x8b}) {
		zr, err := gzip.NewReader(br)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		defer zr.Close()
		br = bufio.NewReader(zr)
	}
	sc := bufio.NewScanner(br)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for n := 0; sc.Scan(); n++ {
		if n%1000 == 0 && ctx.Err() != nil {
			return nil // quitting mid-load: nobody is left to look at the rest
		}
		if err := l.line(sc.Text()); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return l.flush()
}

// follow reads path from its start, says it has loaded once it reaches the end, then keeps reading.
func (l *Loader) follow(ctx context.Context, path string) error {
	t := follow.Open(path, false)
	defer t.Close()
	var err error
	emit := func(s string) bool {
		err = l.line(s)
		return err == nil && ctx.Err() == nil
	}
	t.Poll(emit)
	if err == nil {
		err = l.flush()
	}
	if err != nil || ctx.Err() != nil {
		return err
	}
	l.Following.Store(true)
	defer l.Following.Store(false)
	l.loaded.Store(true)
	tick := time.NewTicker(l.every())
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			if t.Poll(emit); err == nil {
				err = l.flush()
			}
			if err != nil {
				return err
			}
		}
	}
}

// stream reads r until it ends, writing what it has every poll, since a pipe like journalctl -f may never end.
// There's no end to wait for, so it counts as loaded from the start.
func (l *Loader) stream(ctx context.Context, r io.Reader) error {
	l.Following.Store(true)
	defer l.Following.Store(false)
	l.loaded.Store(true)
	lines := make(chan string, 1000)
	var readErr error
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			select {
			case lines <- sc.Text():
			case <-ctx.Done():
				return
			}
		}
		readErr = sc.Err()
	}()
	tick := time.NewTicker(l.every())
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case s, ok := <-lines:
			if !ok {
				if err := l.flush(); err != nil {
					return err
				}
				return readErr
			}
			if err := l.line(s); err != nil {
				return err
			}
		case <-tick.C:
			if err := l.flush(); err != nil {
				return err
			}
		}
	}
}

func (l *Loader) every() time.Duration {
	if l.Poll > 0 {
		return l.Poll
	}
	return time.Second
}

const batchSize = 500

func (l *Loader) line(s string) error {
	l.Lines.Add(1)
	p, ok := syslog.Parse(s, l.Loc, time.Now())
	if !ok {
		if strings.TrimSpace(s) != "" {
			l.Unparsed.Add(1)
		}
		return nil
	}
	if l.Server != "" {
		p.Host = l.Server
	}
	l.mu.Lock()
	if l.first.IsZero() || p.Time.Before(l.first) {
		l.first = p.Time
	}
	if p.Time.After(l.last) {
		l.last = p.Time
	}
	if l.hosts == nil {
		l.hosts = map[string]bool{}
	}
	l.hosts[p.Host] = true
	l.mu.Unlock()
	if e, ok := postfix.Parse(p); ok {
		if l.batch = append(l.batch, e); len(l.batch) >= batchSize {
			return l.flush()
		}
	}
	return nil
}

func (l *Loader) flush() error {
	if len(l.batch) == 0 {
		return nil
	}
	_, err := l.Store.Ingest(context.Background(), l.batch)
	l.batch = l.batch[:0]
	l.Version.Add(1)
	return err
}
