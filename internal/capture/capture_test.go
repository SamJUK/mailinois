package capture

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-milter"
	"github.com/emersion/go-msgauth/dkim"
)

const first = "From: Alice <alice@example.com>\r\n" +
	"To: bob@example.net\r\n" +
	"Subject: A folded\r\n\tsubject\r\n" +
	"Message-ID: <m1@example.com>\r\n" +
	"X-Tight:no space after the colon\r\n" +
	"X-Empty:\r\n" +
	"\r\n" +
	"Hello Bob.\r\n" +
	"This body is long enough to arrive in several chunks.\r\n"

const second = "From: Carol <carol@example.com>\r\n" +
	"To: bob@example.net\r\n" +
	"Message-ID: <m2@example.com>\r\n" +
	"\r\n" +
	"Short.\r\n"

func headerOf(raw string) string {
	i := strings.Index(raw, "\r\n\r\n")
	return raw[:i+4]
}

func TestHeadersModeKeepsOnlyHeaders(t *testing.T) {
	addr, got := serve(t, ModeHeaders, 0)
	s := dial(t, addr)
	if !s.ProtocolOption(milter.OptNoBody) || !s.ProtocolOption(milter.OptHeaderLeadingSpace) {
		t.Errorf("negotiated %#x: want no body and the leading space kept", s.ProtocolOpts)
	}
	if s.ProtocolOption(milter.OptNoConnect) || s.ProtocolOption(milter.OptNoHeaders) {
		t.Errorf("negotiated %#x: connect and headers must be sent", s.ProtocolOpts)
	}
	send(t, s, "4AB1C2D3E4", first)
	send(t, s, "5FF0011223", second)

	for _, want := range []struct{ queueID, messageID, raw string }{
		{"4AB1C2D3E4", "m1@example.com", first},
		{"5FF0011223", "m2@example.com", second},
	} {
		m := receive(t, got)
		if m.QueueID != want.queueID || m.MessageID != want.messageID || m.Server != "mx.example.com" {
			t.Errorf("got queue ID %q, Message-ID %q, server %q; want %q, %q, mx.example.com", m.QueueID, m.MessageID, m.Server, want.queueID, want.messageID)
		}
		if string(m.Header) != headerOf(want.raw) {
			t.Errorf("header:\n%q\nwant\n%q", m.Header, headerOf(want.raw))
		}
		if m.Body != nil || m.Truncated || m.Size != int64(len(m.Header)) {
			t.Errorf("body %q, truncated %v, size %d: want no body, not truncated, size %d", m.Body, m.Truncated, m.Size, len(m.Header))
		}
		if m.At.IsZero() {
			t.Error("received_at not set")
		}
	}
}

func TestFullModeKeepsHeaderAndBody(t *testing.T) {
	addr, got := serve(t, ModeFull, 0)
	s := dial(t, addr)
	if s.ProtocolOption(milter.OptNoBody) {
		t.Errorf("negotiated %#x: full mode needs the body", s.ProtocolOpts)
	}
	send(t, s, "4AB1C2D3E4", first)
	send(t, s, "5FF0011223", second)

	for _, want := range []struct{ queueID, raw string }{{"4AB1C2D3E4", first}, {"5FF0011223", second}} {
		m := receive(t, got)
		if m.QueueID != want.queueID || m.Server != "mx.example.com" {
			t.Errorf("queue ID %q, server %q: want %q, mx.example.com", m.QueueID, m.Server, want.queueID)
		}
		if string(m.Header)+string(m.Body) != want.raw {
			t.Errorf("captured\n%q\nwant\n%q", string(m.Header)+string(m.Body), want.raw)
		}
		if m.Truncated || m.Size != int64(len(want.raw)) {
			t.Errorf("truncated %v, size %d: want false, %d", m.Truncated, m.Size, len(want.raw))
		}
	}
}

func TestOverMaxSizeIsCutAndMarked(t *testing.T) {
	maxSize := int64(len(headerOf(first)) + 10)
	addr, got := serve(t, ModeFull, maxSize)
	s := dial(t, addr)
	send(t, s, "4AB1C2D3E4", first)
	send(t, s, "5FF0011223", second)

	m := receive(t, got)
	if !m.Truncated || m.Size != int64(len(first)) {
		t.Errorf("truncated %v, size %d: want true, %d", m.Truncated, m.Size, len(first))
	}
	if kept := string(m.Header) + string(m.Body); kept != first[:maxSize] {
		t.Errorf("kept %q, want the first %d bytes %q", kept, maxSize, first[:maxSize])
	}

	// The next message on the connection fits, and isn't marked.
	m = receive(t, got)
	if m.Truncated || string(m.Header)+string(m.Body) != second {
		t.Errorf("second message: truncated %v, %q; want whole and unmarked", m.Truncated, string(m.Header)+string(m.Body))
	}
}

func TestHeaderOverMaxSizeKeepsWholeFields(t *testing.T) {
	addr, got := serve(t, ModeHeaders, 60)
	s := dial(t, addr)
	send(t, s, "4AB1C2D3E4", first)

	m := receive(t, got)
	want := "From: Alice <alice@example.com>\r\nTo: bob@example.net\r\n\r\n"
	if string(m.Header) != want || !m.Truncated || m.Size != int64(len(headerOf(first))) {
		t.Errorf("header %q, truncated %v, size %d; want %q, true, %d", m.Header, m.Truncated, m.Size, want, len(headerOf(first)))
	}
}

func TestAbortDropsThePartialMessage(t *testing.T) {
	addr, got := serve(t, ModeFull, 0)
	s := dial(t, addr)
	check(t, s.Macros(milter.CodeHeader, "i", "AAAAAAAAAA"))
	accepted(t)(s.HeaderField("Subject", " never finished"))
	check(t, s.Abort())
	send(t, s, "5FF0011223", second)

	m := receive(t, got)
	if m.QueueID != "5FF0011223" || string(m.Header)+string(m.Body) != second {
		t.Errorf("got %q %q, want only the message after the abort", m.QueueID, string(m.Header)+string(m.Body))
	}
	select {
	case m := <-got:
		t.Errorf("unexpected capture %q", m.QueueID)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestOffModeAcceptsAtConnect(t *testing.T) {
	addr, got := serve(t, ModeOff, 0)
	s, err := openSession(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	check(t, s.Macros(milter.CodeConn, "j", "mx.example.com"))
	a, err := s.Conn("client.example.net", milter.FamilyInet, 49152, "192.0.2.1")
	if err != nil || a.Code != milter.ActAccept {
		t.Fatalf("connect: %+v, %v; want accept", a, err)
	}
	select {
	case m := <-got:
		t.Errorf("off mode captured %q", m.QueueID)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestServeRejectsUnknownMode(t *testing.T) {
	if err := Serve(context.Background(), freeAddr(t), "bodies", 0, func(Message) {}); err == nil {
		t.Error("Serve with mode \"bodies\" returned nil, want an error")
	}
}

func TestServeEndsOpenSessions(t *testing.T) {
	addr := freeAddr(t)
	got := make(chan Message, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, addr, ModeFull, 0, func(m Message) { got <- m }) }()

	s := dial(t, addr)
	send(t, s, "4AB1C2D3E4", first)
	receive(t, got) // the session works before the context ends

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve didn't return with a session open")
	}
	if _, err := s.HeaderField("Subject", " after shutdown"); err == nil {
		t.Error("the session still answers after Serve returned")
	}
}

// A message signed with simple/simple canonicalisation still verifies after the milter rebuilt it, so the
// rebuilt header and body are byte for byte what Postfix had.
func TestCaptureKeepsDKIMIntact(t *testing.T) {
	signed, lookup := sign(t, first)
	addr, got := serve(t, ModeFull, 0)
	s := dial(t, addr)
	send(t, s, "4AB1C2D3E4", signed)

	m := receive(t, got)
	res := VerifyDKIM(m, lookup)
	if len(res) != 1 || res[0].Status != "pass" || res[0].Domain != "example.com" || res[0].Selector != "s1" {
		t.Fatalf("VerifyDKIM = %+v, want one pass for example.com s1", res)
	}

	m.Body = bytes.Replace(m.Body, []byte("Hello Bob."), []byte("Hello Eve."), 1)
	if res := VerifyDKIM(m, lookup); len(res) != 1 || res[0].Status != "fail" {
		t.Errorf("tampered body: VerifyDKIM = %+v, want one fail", res)
	}
}

func serve(t *testing.T, mode Mode, maxSize int64) (string, chan Message) {
	t.Helper()
	addr := freeAddr(t)
	got := make(chan Message, 10)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, addr, mode, maxSize, func(m Message) { got <- m }) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Serve didn't return after its context ended")
		}
	})
	return addr, got
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

func openSession(addr string) (*milter.ClientSession, error) {
	c := milter.NewClientWithOptions("tcp", addr, milter.ClientOptions{ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second})
	var err error
	for range 50 {
		var s *milter.ClientSession
		if s, err = c.Session(); err == nil {
			return s, nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil, err
}

// dial opens a session and sends what Postfix sends when a client connects: the connect macros and event, then
// the HELO macros (sent even though the event is skipped, which replaces go-milter's macro map).
func dial(t *testing.T, addr string) *milter.ClientSession {
	t.Helper()
	s, err := openSession(addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	check(t, s.Macros(milter.CodeConn, "j", "mx.example.com", "{daemon_name}", "smtpd", "_", "client.example.net [192.0.2.1]"))
	accepted(t)(s.Conn("client.example.net", milter.FamilyInet, 49152, "192.0.2.1"))
	check(t, s.Macros(milter.CodeHelo, "{tls_version}", "TLSv1.3"))
	accepted(t)(s.Helo("client.example.net"))
	return s
}

// send drives one message through the session as Postfix does: envelope macros (the events themselves are
// skipped), header values with the space after the colon and folds as bare newlines, each with the end-of-header
// macros, the body in small CRLF chunks, the end of the message, then the abort Postfix sends to reset.
func send(t *testing.T, s *milter.ClientSession, queueID, raw string) {
	t.Helper()
	head, body, _ := strings.Cut(raw, "\r\n\r\n")
	check(t, s.Macros(milter.CodeMail, "{mail_addr}", "alice@example.com"))
	accepted(t)(s.Mail("alice@example.com", nil))
	check(t, s.Macros(milter.CodeRcpt, "i", queueID, "{rcpt_addr}", "bob@example.net"))
	accepted(t)(s.Rcpt("bob@example.net", nil))

	var fields []string
	for line := range strings.SplitSeq(head, "\r\n") {
		if line[0] == ' ' || line[0] == '\t' {
			fields[len(fields)-1] += "\n" + line
		} else {
			fields = append(fields, line)
		}
	}
	for _, f := range fields {
		name, value, _ := strings.Cut(f, ":")
		check(t, s.Macros(milter.CodeHeader, "i", queueID))
		accepted(t)(s.HeaderField(name, value))
	}
	if !s.ProtocolOption(milter.OptNoBody) {
		for chunk := range slices(body, 16) {
			check(t, s.Macros(milter.CodeBody, "i", queueID))
			accepted(t)(s.BodyChunk([]byte(chunk)))
		}
	}
	check(t, s.Macros(milter.CodeEOB, "i", queueID))
	mods, a, err := s.End()
	accepted(t)(a, err)
	if len(mods) > 0 {
		t.Errorf("the milter asked for changes: %+v", mods)
	}
	check(t, s.Abort())
}

func slices(s string, n int) func(func(string) bool) {
	return func(yield func(string) bool) {
		for len(s) > 0 {
			k := min(n, len(s))
			if !yield(s[:k]) {
				return
			}
			s = s[k:]
		}
	}
}

func receive(t *testing.T, got chan Message) Message {
	t.Helper()
	select {
	case m := <-got:
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("no capture")
		return Message{}
	}
}

func accepted(t *testing.T) func(*milter.Action, error) {
	return func(a *milter.Action, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if a.Code != milter.ActContinue && a.Code != milter.ActAccept {
			t.Errorf("the milter answered %q, want continue or accept", a.Code)
		}
	}
}

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// sign signs raw with a new ed25519 key as example.com, selector s1, and returns the signed message and a TXT
// lookup that knows the key.
func sign(t *testing.T, raw string) (string, func(string) ([]string, error)) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := dkim.Sign(&out, strings.NewReader(raw), &dkim.SignOptions{Domain: "example.com", Selector: "s1", Signer: priv}); err != nil {
		t.Fatal(err)
	}
	record := "v=DKIM1; k=ed25519; p=" + base64.StdEncoding.EncodeToString(pub)
	return out.String(), func(domain string) ([]string, error) {
		if domain == "s1._domainkey.example.com" {
			return []string{record}, nil
		}
		return nil, &net.DNSError{Err: "no such host", Name: domain, IsNotFound: true}
	}
}
