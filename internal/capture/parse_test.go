package capture

import (
	"net"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func capture(raw string) Message {
	head, body, _ := strings.Cut(raw, "\r\n\r\n")
	return Message{Header: []byte(head + "\r\n\r\n"), Body: []byte(body), Size: int64(len(raw))}
}

func crlf(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }

var multipartMessage = crlf(`From: =?ISO-8859-1?Q?Andr=E9?= <andre@example.com>
To: bob@example.net
Subject: =?UTF-8?B?Q2Fmw6k=?= menu
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="outer"

--outer
Content-Type: multipart/alternative; boundary="inner"

--inner
Content-Type: text/plain; charset=ISO-8859-1
Content-Transfer-Encoding: quoted-printable

Caf=E9 cr=E8me br=FBl=E9e, a very long line that quoted-printable soft-=
breaks.
--inner
Content-Type: text/html; charset=UTF-8

<p>Café</p>
--inner--
--outer
Content-Type: application/pdf; name="menu.pdf"
Content-Disposition: attachment; filename="menu.pdf"
Content-Transfer-Encoding: base64

JVBERi0xLjQK
--outer
Content-Type: image/png
Content-Disposition: attachment; filename*=UTF-8''men%C3%BC.png

PNG
--outer--
`)

func TestParseMultipart(t *testing.T) {
	p := Parse(capture(multipartMessage))

	wantHeaders := []Header{
		{"From", "André <andre@example.com>"},
		{"To", "bob@example.net"},
		{"Subject", "Café menu"},
		{"MIME-Version", "1.0"},
		{"Content-Type", `multipart/mixed; boundary="outer"`},
	}
	if !reflect.DeepEqual(p.Headers, wantHeaders) {
		t.Errorf("headers %q, want %q", p.Headers, wantHeaders)
	}
	if want := "Café crème brûlée, a very long line that quoted-printable soft-breaks."; p.Text != want {
		t.Errorf("text %q, want %q", p.Text, want)
	}
	if p.HTML != "<p>Café</p>" {
		t.Errorf("html %q, want <p>Café</p>", p.HTML)
	}
	wantAttachments := []Attachment{{"menu.pdf", "application/pdf", 9}, {"menü.png", "image/png", 3}}
	if !reflect.DeepEqual(p.Attachments, wantAttachments) {
		t.Errorf("attachments %+v, want %+v", p.Attachments, wantAttachments)
	}
}

func TestParseHeadersOnly(t *testing.T) {
	m := capture(multipartMessage)
	m.Body = nil
	p := Parse(m)
	if len(p.Headers) != 5 || p.Text != "" || p.HTML != "" || p.Attachments != nil {
		t.Errorf("headers-only capture: %d headers, text %q, html %q, attachments %+v; want 5 headers and nothing else",
			len(p.Headers), p.Text, p.HTML, p.Attachments)
	}
}

func TestParseMalformed(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		text      string
		headers   int
	}{
		{"cut mid-part, no closing boundary", crlf(`Subject: cut
Content-Type: multipart/mixed; boundary="b"

--b
Content-Type: text/plain

kept
--b
Content-Type: application/octet-stream
Content-Transfer-Encoding: base64

AAAA!!!!not base64`), "kept", 2},
		{"multipart without a boundary", crlf(`Subject: no boundary
Content-Type: multipart/mixed

--b
Content-Type: text/plain

lost
--b--
`), "", 2},
		{"a header line with no colon", crlf(`Subject: before
this line is not a header
To: after@example.com

body`), "body", 2},
		{"broken part header", crlf(`Subject: broken part
Content-Type: multipart/mixed; boundary="b"

--b
Content-Type: text/plain

first
--b
not a header line

second
--b--
`), "first", 2},
		{"unknown charset and invalid UTF-8", "Subject: \xff\xfe\r\nContent-Type: text/plain; charset=x-made-up\r\n\r\ncaf\xe9", "caf�", 2},
		{"bad transfer encoding", "Subject: x\r\nContent-Transfer-Encoding: x-uuencode\r\n\r\nraw", "raw", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := Parse(capture(tc.raw))
			if p.Text != tc.text || len(p.Headers) != tc.headers {
				t.Errorf("text %q with %d headers, want %q with %d", p.Text, len(p.Headers), tc.text, tc.headers)
			}
			for _, h := range p.Headers {
				if !utf8.ValidString(h.Value) {
					t.Errorf("header %s is not valid UTF-8: %q", h.Name, h.Value)
				}
			}
		})
	}
}

func TestParseCapsText(t *testing.T) {
	raw := "Content-Type: text/plain; charset=utf-8\r\n\r\n" + strings.Repeat("é", maxText)
	p := Parse(capture(raw))
	if len(p.Text) > maxText+3 || len(p.Text) < maxText-3 || !utf8.ValidString(p.Text) {
		t.Errorf("text is %d bytes (valid UTF-8: %v), want about %d", len(p.Text), utf8.ValidString(p.Text), maxText)
	}
}

func TestVerifyDKIM(t *testing.T) {
	signed, lookup := sign(t, first)

	if res := VerifyDKIM(capture(signed), lookup); len(res) != 1 || res[0] != (DKIMResult{"example.com", "s1", "pass", ""}) {
		t.Fatalf("signed message: %+v, want one pass", res)
	}

	for _, tc := range []struct {
		name   string
		m      func() Message
		lookup func(string) ([]string, error)
		status string
	}{
		{"tampered body", func() Message {
			m := capture(signed)
			m.Body = []byte(strings.Replace(string(m.Body), "Bob", "Eve", 1))
			return m
		}, lookup, "fail"},
		{"tampered header", func() Message {
			return capture(strings.Replace(signed, "To: bob@example.net", "To: eve@example.net", 1))
		}, lookup, "fail"},
		{"no key published", func() Message { return capture(signed) }, func(d string) ([]string, error) {
			return nil, &net.DNSError{Err: "no such host", Name: d, IsNotFound: true}
		}, "fail"},
		{"DNS timeout", func() Message { return capture(signed) }, func(d string) ([]string, error) {
			return nil, &net.DNSError{Err: "i/o timeout", Name: d, IsTimeout: true}
		}, "error"},
		{"truncated", func() Message {
			m := capture(signed)
			m.Body, m.Truncated = m.Body[:5], true
			return m
		}, lookup, "error"},
		{"headers only", func() Message {
			m := capture(signed)
			m.Body = nil
			return m
		}, lookup, "error"},
		{"unsigned", func() Message { return capture(first) }, lookup, "none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := VerifyDKIM(tc.m(), tc.lookup)
			if len(res) != 1 || res[0].Status != tc.status || res[0].Reason == "" {
				t.Errorf("VerifyDKIM = %+v, want one %s with a reason", res, tc.status)
			}
		})
	}
}

func TestVerifyDKIMTwoSignatures(t *testing.T) {
	inner, lookup := sign(t, first)
	outer, _ := sign(t, inner) // a second key the lookup doesn't know: s1 for example.com is the first one's
	res := VerifyDKIM(capture(outer), lookup)
	if len(res) != 2 || res[0].Status != "fail" || res[1].Status != "pass" || res[1].Selector != "s1" {
		t.Errorf("VerifyDKIM = %+v, want the outer signature to fail and the inner to pass", res)
	}
}
