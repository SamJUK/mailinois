package capture

import (
	"bytes"
	"errors"
	"io"
	"mime"
	"strings"

	"github.com/emersion/go-message"
	"github.com/emersion/go-message/charset"
	"github.com/emersion/go-message/textproto"
	"github.com/emersion/go-msgauth/dkim"
)

const (
	maxText       = 200 << 10
	maxHTML       = 1 << 20
	maxDepth      = 20 // multipart nesting
	maxSignatures = 5  // each one costs a DNS lookup
)

var words = mime.WordDecoder{CharsetReader: charset.Reader}

type Header struct{ Name, Value string }

type Attachment struct {
	Name, Type string
	Size       int
}

type Parsed struct {
	Headers     []Header // unfolded, RFC 2047 words decoded
	Text        string   // the first text/plain part, decoded to UTF-8 where the charset is known, capped at 200 KB
	HTML        string   // the first text/html part, decoded, capped at 1 MB; never rendered by this package
	Attachments []Attachment
}

// Parse reads a capture. Headers-only captures give Headers and nothing else. Malformed MIME doesn't stop it:
// it keeps whatever it read before the damage.
func Parse(m Message) Parsed {
	var p Parsed
	var h textproto.Header
	for _, f := range fields(m.Header) {
		if !f.ok {
			continue
		}
		v := strings.TrimSpace(strings.ReplaceAll(f.value, "\r\n", ""))
		h.Add(f.name, v)
		if dec, err := words.DecodeHeader(v); err == nil {
			v = dec
		}
		p.Headers = append(p.Headers, Header{f.name, strings.ToValidUTF8(v, "�")})
	}
	if len(m.Body) == 0 {
		return p
	}
	// message.New's error only reports an unknown charset or transfer encoding: the body is then left as is.
	e, _ := message.New(message.Header{Header: h}, bytes.NewReader(m.Body))
	p.walk(e, 0)
	return p
}

func (p *Parsed) walk(e *message.Entity, depth int) {
	t, params, _ := mime.ParseMediaType(e.Header.Get("Content-Type"))
	if t == "" {
		t = "text/plain"
	}
	disp, dparams, _ := mime.ParseMediaType(e.Header.Get("Content-Disposition"))
	switch {
	case strings.HasPrefix(t, "multipart/"):
		if depth < maxDepth && params["boundary"] != "" {
			p.walkParts(e.Body, params["boundary"], depth)
		}
	case t == "text/plain" && disp != "attachment" && p.Text == "":
		p.Text = readText(e.Body, maxText)
	case t == "text/html" && disp != "attachment" && p.HTML == "":
		p.HTML = readText(e.Body, maxHTML)
	default:
		n, _ := io.Copy(io.Discard, e.Body)
		p.Attachments = append(p.Attachments, Attachment{Name: filename(dparams, params), Type: t, Size: int(n)})
	}
}

func (p *Parsed) walkParts(body io.Reader, boundary string, depth int) {
	mr := textproto.NewMultipartReader(body, boundary)
	for {
		part, err := mr.NextPart()
		if err != nil {
			return
		}
		e, _ := message.New(message.Header{Header: part.Header}, part)
		p.walk(e, depth+1)
	}
}

// readText reads up to limit bytes. A cut through a character, or bytes in a charset nobody could decode,
// come out as U+FFFD, so the result is always valid UTF-8.
func readText(r io.Reader, limit int) string {
	b, _ := io.ReadAll(io.LimitReader(r, int64(limit)))
	return strings.ToValidUTF8(string(b), "�")
}

func filename(disposition, contentType map[string]string) string {
	name := disposition["filename"]
	if name == "" {
		name = contentType["name"]
	}
	// Against RFC 2047, but it's how most mail clients encode non-ASCII names.
	if dec, err := words.DecodeHeader(name); err == nil {
		name = dec
	}
	return strings.ToValidUTF8(name, "�")
}

type field struct {
	name, value string // value as it was, folds and leading space kept
	ok          bool   // the line had a colon
}

// fields splits a header block the way go-msgauth's DKIM verifier does, so its signatures line up with ours.
func fields(raw []byte) []field {
	var fs []field
	for line := range strings.SplitSeq(string(raw), "\n") {
		line = strings.TrimSuffix(line, "\r")
		switch {
		case line == "":
			return fs
		case len(fs) > 0 && (line[0] == ' ' || line[0] == '\t'):
			fs[len(fs)-1].value += "\r\n" + line
		default:
			name, value, ok := strings.Cut(line, ":")
			fs = append(fs, field{strings.TrimSpace(name), value, ok})
		}
	}
	return fs
}

type DKIMResult struct {
	Domain, Selector string
	Status           string // "pass", "fail", "none" (no signature), "error" (couldn't check: lookup failed, truncated body)
	Reason           string
}

// VerifyDKIM checks the DKIM signatures on a full, untruncated capture. lookupTXT may be nil for real DNS.
// A broken signature or a missing key is "fail"; only a DNS failure that may pass is "error".
func VerifyDKIM(m Message, lookupTXT func(domain string) ([]string, error)) []DKIMResult {
	var sigs []DKIMResult
	for _, f := range fields(m.Header) {
		if strings.EqualFold(f.name, "DKIM-Signature") {
			tags := dkimTags(f.value)
			sigs = append(sigs, DKIMResult{Domain: tags["d"], Selector: tags["s"]})
		}
	}
	switch {
	case len(sigs) == 0:
		return []DKIMResult{{Status: "none", Reason: "no DKIM-Signature header"}}
	case m.Truncated:
		return []DKIMResult{{Status: "error", Reason: "the capture was cut at max_size, so the body hash can't be checked"}}
	case len(m.Body) == 0:
		return []DKIMResult{{Status: "error", Reason: "the capture has no body (headers mode), so the body hash can't be checked"}}
	}

	msg := io.MultiReader(bytes.NewReader(m.Header), bytes.NewReader(m.Body))
	verifs, err := dkim.VerifyWithOptions(msg, &dkim.VerifyOptions{LookupTXT: lookupTXT, MaxVerifications: maxSignatures})
	// Past maxSignatures the rest are skipped. Any other error comes with no usable verifications.
	if err != nil && !errors.Is(err, dkim.ErrTooManySignatures) {
		return []DKIMResult{{Status: "error", Reason: err.Error()}}
	}
	out := make([]DKIMResult, 0, len(verifs))
	for i, v := range verifs {
		r := DKIMResult{Domain: v.Domain, Status: "pass"}
		if i < len(sigs) {
			r.Selector = sigs[i].Selector
			if r.Domain == "" {
				r.Domain = sigs[i].Domain
			}
		}
		if v.Err != nil {
			r.Status, r.Reason = "fail", strings.TrimPrefix(v.Err.Error(), "dkim: ")
			if dkim.IsTempFail(v.Err) {
				r.Status = "error"
			}
		}
		out = append(out, r)
	}
	return out
}

func dkimTags(value string) map[string]string {
	tags := map[string]string{}
	for tag := range strings.SplitSeq(value, ";") {
		k, v, ok := strings.Cut(tag, "=")
		if ok {
			tags[strings.TrimSpace(k)] = strings.Join(strings.Fields(v), "")
		}
	}
	return tags
}
