// Package follow reads lines appended to a log file, through renames (logrotate's create) and in-place
// truncation (copytruncate).
package follow

import (
	"bytes"
	"log/slog"
	"os"
	"strings"
	"time"
)

const (
	maxLine = 64 << 10 // longer lines are handed on in pieces
	// keepRotated is how long a file renamed away is still read: logrotate's "create" makes the new file before
	// the postrotate script tells the writer to reopen, so lines land in the old one meanwhile.
	keepRotated = 30 * time.Second
)

// Tail is one followed file. Poll hands on what was appended since the last Poll.
type Tail struct {
	path     string
	emit     func(string) bool // false: stop reading
	cur      *logFile
	old      *logFile // renamed away, read until oldUntil
	oldUntil time.Time
	warned   string
}

type logFile struct {
	f      *os.File
	offset int64 // just past the last line handed on
	buf    []byte
}

// Open starts following path: from its end when atEnd (only lines written from now on), otherwise from its
// start. A path that doesn't exist yet is retried on every Poll, and read from its start once it appears.
func Open(path string, atEnd bool) *Tail {
	t := &Tail{path: path}
	t.open(atEnd)
	return t
}

func (t *Tail) open(atEnd bool) {
	f, err := os.Open(t.path)
	if err != nil {
		if msg := err.Error(); msg != t.warned {
			slog.Warn("can't open log file, will keep trying", "path", t.path, "err", err)
			t.warned = msg
		}
		return
	}
	t.warned = ""
	t.cur = &logFile{f: f, buf: make([]byte, maxLine)}
	if atEnd {
		t.cur.offset = lastLineStart(f)
	}
}

// Poll hands each new complete line to emit, until emit reports false.
func (t *Tail) Poll(emit func(string) bool) {
	t.emit = emit
	t.poll()
}

func (t *Tail) poll() {
	if t.old != nil {
		t.old.read(t.emit)
		if time.Now().After(t.oldUntil) {
			t.old.f.Close()
			t.old = nil
		}
	}
	if t.cur != nil && replaced(t.path, t.cur.f) {
		t.cur.read(t.emit)
		if t.old != nil {
			t.old.f.Close()
		}
		t.old, t.oldUntil, t.cur = t.cur, time.Now().Add(keepRotated), nil
	}
	if t.cur == nil {
		if t.open(false); t.cur == nil {
			return
		}
	}
	t.cur.read(t.emit)
}

func (t *Tail) Close() {
	for _, l := range []*logFile{t.old, t.cur} {
		if l != nil {
			l.f.Close()
		}
	}
}

// replaced reports whether path now names a different file from f. A path that's gone isn't: the file was
// renamed and its replacement isn't there yet, so f is still the one being written.
func replaced(path string, f *os.File) bool {
	pi, err := os.Stat(path)
	if err != nil {
		return false
	}
	fi, err := f.Stat()
	return err == nil && !os.SameFile(pi, fi)
}

// read hands on every complete line past the offset. A partial last line waits for the rest.
func (l *logFile) read(emit func(string) bool) {
	fi, err := l.f.Stat()
	if err != nil {
		return
	}
	if fi.Size() < l.offset {
		// Truncated in place (copytruncate). Lines written between the last read and the cut went with the copy.
		l.offset = 0
	}
	for {
		n, err := l.f.ReadAt(l.buf, l.offset)
		data, used := l.buf[:n], 0
		for {
			i := bytes.IndexByte(data[used:], '\n')
			if i < 0 {
				break
			}
			line := strings.TrimSuffix(string(data[used:used+i]), "\r")
			used += i + 1
			if line != "" && !emit(line) {
				l.offset += int64(used)
				return
			}
		}
		if used == 0 && n == len(l.buf) {
			if !emit(string(data)) {
				return
			}
			used = n
		}
		l.offset += int64(used)
		if err != nil || used == 0 {
			return
		}
	}
}

// lastLineStart is the offset just past f's last newline, so a line still being written when the agent starts
// is shipped once it's whole.
func lastLineStart(f *os.File) int64 {
	fi, err := f.Stat()
	if err != nil {
		return 0
	}
	size := fi.Size()
	from := max(size-maxLine, 0)
	buf := make([]byte, size-from)
	n, _ := f.ReadAt(buf, from)
	if i := bytes.LastIndexByte(buf[:n], '\n'); i >= 0 {
		return from + int64(i) + 1
	}
	return size
}
