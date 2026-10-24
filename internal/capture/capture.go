// Package capture copies the header, or the whole message, of each mail Postfix accepts through a milter, and
// reads those copies back: header fields, text and HTML parts, attachments and DKIM signatures.
//
// The milter never holds mail up. It continues at every step, never rejects, delays or changes a message, and
// hands each finished copy to a sink that must not block.
package capture

import (
	"context"
	"fmt"
	"net"
	"net/textproto"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-milter"
)

type Mode string

const (
	ModeOff     Mode = "off"
	ModeHeaders Mode = "headers"
	ModeFull    Mode = "full"
)

// DefaultMaxSize is what Serve keeps per message when it's given a max size of 0 or less.
const DefaultMaxSize = 1 << 20

// Message is one captured message, as Postfix handed it over after earlier milters (OpenDKIM) ran. Postfix
// never shows a milter its own Received header, so that one is missing.
type Message struct {
	QueueID   string    `json:"queue_id"`   // milter macro {i}
	MessageID string    `json:"message_id"` // from the Message-ID header, angle brackets stripped
	Server    string    `json:"server"`     // macro {j}: Postfix's myhostname
	At        time.Time `json:"received_at"`
	Header    []byte    `json:"header"`         // the raw header block, CRLF line endings, ending with the blank line
	Body      []byte    `json:"body,omitempty"` // full mode only, at most max_size bytes
	Size      int64     `json:"size"`           // header plus body as received, before any cut (headers mode: the header)
	Truncated bool      `json:"truncated"`
}

// Postfix sends the macros of skipped events anyway, so skipping costs nothing. Connect stays on: it's the
// only callback that sees j, because go-milter keeps just the latest macro packet. go-milter answers every
// header and body chunk, so the no-reply options would break the protocol and aren't asked for.
const skipEnvelope = milter.OptNoHelo | milter.OptNoMailFrom | milter.OptNoRcptTo | milter.OptNoUnknown | milter.OptNoData

// Serve runs a milter on addr until ctx ends, passing each finished capture to sink. sink must not block.
// In ModeOff it accepts every connection at once, so Postfix can keep the milter configured while capture is off.
func Serve(ctx context.Context, addr string, mode Mode, maxSize int64, sink func(Message)) error {
	var proto milter.OptProtocol
	switch mode {
	case ModeOff:
		proto = skipEnvelope | milter.OptNoHeaders | milter.OptNoEOH | milter.OptNoBody
	case ModeHeaders:
		proto = skipEnvelope | milter.OptNoEOH | milter.OptNoBody | milter.OptHeaderLeadingSpace
	case ModeFull:
		proto = skipEnvelope | milter.OptNoEOH | milter.OptHeaderLeadingSpace
	default:
		return fmt.Errorf("capture mode %q: want off, headers or full", mode)
	}
	if maxSize <= 0 {
		maxSize = DefaultMaxSize
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	tl := &listener{Listener: ln, conns: map[*conn]struct{}{}}
	srv := &milter.Server{
		Protocol:  proto,
		NewMilter: func() milter.Milter { return &session{mode: mode, maxSize: maxSize, sink: sink} },
	}
	stop := context.AfterFunc(ctx, tl.shut)
	defer stop()
	err = srv.Serve(tl)
	tl.shut()
	tl.wg.Wait()
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// session is one Postfix connection: go-milter keeps it for as long as every reply is Continue.
type session struct {
	mode    Mode
	maxSize int64
	sink    func(Message)

	server    string
	queueID   string
	messageID string
	header    []byte
	body      []byte
	ended     bool // the header block has its blank line
	size      int64
	cut       bool
}

func (s *session) Connect(_, _ string, _ uint16, _ net.IP, m *milter.Modifier) (milter.Response, error) {
	if s.mode == ModeOff {
		return milter.RespAccept, nil
	}
	s.server = macro(m, "j")
	return milter.RespContinue, nil
}

func (s *session) Helo(string, *milter.Modifier) (milter.Response, error) {
	return milter.RespContinue, nil
}

func (s *session) MailFrom(string, *milter.Modifier) (milter.Response, error) {
	return milter.RespContinue, nil
}

func (s *session) RcptTo(string, *milter.Modifier) (milter.Response, error) {
	return milter.RespContinue, nil
}

// Header gets the value as Postfix stored it: the space after the colon kept (OptHeaderLeadingSpace) and folded
// lines joined by bare newlines.
func (s *session) Header(name, value string, m *milter.Modifier) (milter.Response, error) {
	if s.queueID == "" {
		s.queueID = macro(m, "i")
	}
	if s.messageID == "" && strings.EqualFold(name, "Message-ID") {
		s.messageID = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(value), "<"), ">")
	}
	if strings.Contains(value, "\n") {
		value = strings.ReplaceAll(strings.ReplaceAll(value, "\r\n", "\n"), "\n", "\r\n")
	}
	field := name + ":" + value + "\r\n"
	s.size += int64(len(field))
	// Room is kept for the blank line that ends the block.
	if s.cut || int64(len(s.header)+len(field)+2) > s.maxSize {
		s.cut = true
		return milter.RespContinue, nil
	}
	s.header = append(s.header, field...)
	return milter.RespContinue, nil
}

func (s *session) Headers(textproto.MIMEHeader, *milter.Modifier) (milter.Response, error) {
	return milter.RespContinue, nil
}

func (s *session) BodyChunk(chunk []byte, _ *milter.Modifier) (milter.Response, error) {
	s.endHeader()
	s.size += int64(len(chunk))
	if s.cut {
		return milter.RespContinue, nil
	}
	if room := s.maxSize - int64(len(s.header)+len(s.body)); int64(len(chunk)) > room {
		chunk, s.cut = chunk[:max(room, 0)], true
	}
	s.body = append(s.body, chunk...)
	return milter.RespContinue, nil
}

// Body ends the message. It answers Continue rather than Accept, which Postfix reads the same way at the end
// of a message, because go-milter swaps in a fresh session after any other answer and the next message on
// this connection would lose j.
func (s *session) Body(m *milter.Modifier) (milter.Response, error) {
	if id := macro(m, "i"); id != "" {
		s.queueID = id
	}
	s.endHeader()
	s.sink(Message{
		QueueID:   s.queueID,
		MessageID: s.messageID,
		Server:    s.server,
		At:        time.Now(),
		Header:    s.header,
		Body:      s.body,
		Size:      s.size,
		Truncated: s.cut,
	})
	s.reset()
	return milter.RespContinue, nil
}

func (s *session) Abort(*milter.Modifier) error {
	s.reset()
	return nil
}

func (s *session) endHeader() {
	if !s.ended {
		s.ended = true
		s.size += 2
		s.header = append(s.header, '\r', '\n')
	}
}

func (s *session) reset() {
	*s = session{mode: s.mode, maxSize: s.maxSize, sink: s.sink, server: s.server}
}

// macro reads a macro by its bare name or in braces, whichever way main.cf lists it.
func macro(m *milter.Modifier, name string) string {
	if v := m.Macros[name]; v != "" {
		return v
	}
	return m.Macros["{"+name+"}"]
}

// listener keeps track of open sessions so Serve can end them, and return only once they're gone.
type listener struct {
	net.Listener
	wg     sync.WaitGroup
	mu     sync.Mutex
	conns  map[*conn]struct{}
	closed bool
}

func (l *listener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		c.Close()
		return nil, net.ErrClosed
	}
	tc := &conn{Conn: c, l: l}
	l.conns[tc] = struct{}{}
	l.wg.Add(1)
	return tc, nil
}

func (l *listener) shut() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	l.Listener.Close()
	for c := range l.conns {
		c.Conn.Close()
	}
}

type conn struct {
	net.Conn
	l    *listener
	once sync.Once
}

// Close is go-milter's last call on a session, after its last callback has returned.
func (c *conn) Close() error {
	c.once.Do(func() {
		c.l.mu.Lock()
		delete(c.l.conns, c)
		c.l.mu.Unlock()
		c.l.wg.Done()
	})
	return c.Conn.Close()
}
