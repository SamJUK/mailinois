// Package syslog receives log lines over UDP and TCP and splits them into their parts.
package syslog

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"
)

const maxFrame = 64 << 10

type Line struct {
	Host    string
	Time    time.Time
	Program string // postfix/smtp
	PID     int
	Msg     string
}

// Parse reads one syslog frame or log file line: RFC 5424, RFC 3164 with or without a priority, rsyslog's
// RFC 3339 file format, and boky/postfix's stdout format (a severity word where the host goes). Timestamps
// without a zone are read in loc, and without a year as the most recent one that isn't in the future.
func Parse(s string, loc *time.Location, now time.Time) (Line, bool) {
	s = strings.TrimRight(s, "\r\n\x00")
	if strings.HasPrefix(s, "<") {
		if i := strings.IndexByte(s, '>'); i > 1 && i <= 4 {
			s = s[i+1:]
		}
	}
	if rest, ok := strings.CutPrefix(s, "1 "); ok {
		return parse5424(rest)
	}

	var l Line
	var rest string
	if len(s) > 16 && isMonth(s[:3]) {
		t, err := time.ParseInLocation(time.Stamp, s[:15], loc)
		if err != nil {
			return Line{}, false
		}
		l.Time = withYear(t, now)
		rest = s[16:]
	} else {
		ts, r, _ := strings.Cut(s, " ")
		t, err := time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			return Line{}, false
		}
		l.Time, rest = t, r
	}

	host, rest, _ := strings.Cut(rest, " ")
	if isSeverity(host) {
		host, rest = "", strings.TrimLeft(rest, " ")
	}
	tag, msg, ok := strings.Cut(rest, ": ")
	if !ok {
		return Line{}, false
	}
	l.Host, l.Msg = host, msg
	l.Program, l.PID = splitTag(tag)
	return l, true
}

func parse5424(s string) (Line, bool) {
	f := strings.SplitN(s, " ", 6)
	if len(f) < 6 {
		return Line{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, f[0])
	if err != nil {
		return Line{}, false
	}
	l := Line{Time: t, Host: nil5424(f[1]), Program: nil5424(f[2])}
	l.PID, _ = strconv.Atoi(f[3])
	msg, ok := skipStructuredData(f[5])
	if !ok {
		return Line{}, false
	}
	// rsyslog's %msg% keeps the space that followed the tag's colon.
	l.Msg = strings.TrimPrefix(strings.TrimPrefix(msg, "\ufeff"), " ")
	return l, true
}

func skipStructuredData(s string) (string, bool) {
	if rest, ok := strings.CutPrefix(s, "-"); ok {
		return strings.TrimPrefix(rest, " "), true
	}
	inQuote, escaped := false, false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case escaped:
			escaped = false
		case c == '\\':
			escaped = true
		case c == '"':
			inQuote = !inQuote
		case c == ']' && !inQuote && (i+1 == len(s) || s[i+1] != '['):
			return strings.TrimPrefix(s[i+1:], " "), true
		}
	}
	return "", false
}

func nil5424(s string) string {
	if s == "-" {
		return ""
	}
	return s
}

func splitTag(tag string) (string, int) {
	name, pid, ok := strings.Cut(tag, "[")
	if !ok {
		return tag, 0
	}
	n, _ := strconv.Atoi(strings.TrimSuffix(pid, "]"))
	return name, n
}

func withYear(t, now time.Time) time.Time {
	t = t.AddDate(now.Year(), 0, 0)
	if t.After(now.Add(24 * time.Hour)) {
		t = t.AddDate(-1, 0, 0)
	}
	return t
}

func isMonth(s string) bool {
	i := strings.Index("JanFebMarAprMayJunJulAugSepOctNovDec", s)
	return len(s) == 3 && i >= 0 && i%3 == 0
}

func isSeverity(s string) bool {
	switch s {
	case "EMERG", "ALERT", "CRIT", "ERR", "ERROR", "WARNING", "WARN", "NOTICE", "INFO", "DEBUG":
		return true
	}
	return false
}

// ListenUDP calls handle with every datagram until ctx ends.
func ListenUDP(ctx context.Context, addr string, handle func(string)) error {
	conn, err := net.ListenPacket("udp", addr)
	if err != nil {
		return err
	}
	context.AfterFunc(ctx, func() { conn.Close() })
	buf := make([]byte, maxFrame)
	for {
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		handle(string(buf[:n]))
	}
}

// ListenTCP calls handle with every frame, newline-delimited or octet-counted (RFC 6587), until ctx ends.
func ListenTCP(ctx context.Context, addr string, handle func(string)) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	context.AfterFunc(ctx, func() { ln.Close() })
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go func() {
			defer conn.Close()
			stop := context.AfterFunc(ctx, func() { conn.Close() })
			defer stop()
			if err := ReadFrames(conn, handle); err != nil && ctx.Err() == nil {
				slog.Warn("syslog connection", "remote", conn.RemoteAddr(), "err", err)
			}
		}()
	}
}

func ReadFrames(r io.Reader, handle func(string)) error {
	br := bufio.NewReaderSize(r, maxFrame)
	for {
		if _, err := br.Peek(1); err != nil {
			return eofIsFine(err)
		}
		if octetCounted(br) {
			size, err := br.ReadString(' ')
			if err != nil {
				return eofIsFine(err)
			}
			n, err := strconv.Atoi(strings.TrimSuffix(size, " "))
			if err != nil || n > maxFrame {
				return errors.New("bad octet count " + strconv.Quote(size))
			}
			frame := make([]byte, n)
			if _, err := io.ReadFull(br, frame); err != nil {
				return eofIsFine(err)
			}
			handle(string(frame))
			continue
		}
		line, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			return errors.New("frame over 64 KiB")
		}
		if len(line) > 1 {
			handle(string(line))
		}
		if err != nil {
			return eofIsFine(err)
		}
	}
}

// octetCounted reports whether the next frame starts with a length: digits, then a space.
func octetCounted(br *bufio.Reader) bool {
	head, _ := br.Peek(min(br.Buffered(), 7))
	for i, c := range head {
		switch {
		case c == ' ':
			return i > 0
		case c < '0' || c > '9' || (i == 0 && c == '0'):
			return false
		}
	}
	return false
}

func eofIsFine(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}
