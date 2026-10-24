// Package postfix turns Postfix (and OpenDKIM) log lines into events about queue IDs.
package postfix

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/SamJUK/mailinois/internal/syslog"
)

type Kind string

const (
	Client    Kind = "client"    // smtpd took a message: client=host[ip]
	Pickup    Kind = "pickup"    // local submission through sendmail
	Cleanup   Kind = "cleanup"   // message-id=<...>
	Queued    Kind = "queued"    // qmgr: from=<...>, size=, nrcpt=
	Delivery  Kind = "delivery"  // to=<...>, relay=, dsn=, status=
	Expired   Kind = "expired"   // qmgr gave up: status=expired, returned to sender
	Cancelled Kind = "cancelled" // SMTP transaction abandoned before acceptance
	Removed   Kind = "removed"
	Reject    Kind = "reject" // reject, milter-reject or discard, with or without a queue ID
	Header    Kind = "header" // header_checks INFO/WARN on Subject
	Notice    Kind = "notice" // anything else about a queue ID (DKIM signing, bounce notifications, ...)
)

type Event struct {
	Server   string
	At       time.Time
	Program  string
	QueueID  string // empty for NOQUEUE rejections
	Kind     Kind
	Client   string // the client's name, or uid=N for local submissions
	ClientIP string
	SASLUser string
	Sender   string // set on Queued and Reject; empty is the null sender <>
	MsgID    string
	Size     int64
	Nrcpt    int
	To       string
	OrigTo   string
	Relay    string
	RelayIP  string
	Delay    float64
	DSN      string
	Status   string // sent, deferred, bounced, expired, rejected, ...
	Response string
	Subject  string
	Raw      string // the message text after program[pid]:

	DKIMDomain, DKIMSelector string // from OpenDKIM's "DKIM-Signature field added (s=..., d=...)"
}

// Short queue IDs are hex; long ones (enable_long_queue_ids) are base 51 without vowels, with a z separator.
var queueID = regexp.MustCompile(`^([0-9A-F]{6,}|[0-9B-DF-HJ-NP-TV-Zb-df-hj-np-tv-y]{6,}z[0-9B-DF-HJ-NP-TV-Zb-df-hj-np-tv-y]+): (.*)$`)

var (
	reTo       = regexp.MustCompile(`(?:^|, )to=<([^>]*)>`)
	reOrigTo   = regexp.MustCompile(`, orig_to=<([^>]*)>`)
	reRelay    = regexp.MustCompile(`, relay=([^,]+)`)
	reDelay    = regexp.MustCompile(`, delay=([0-9.]+)`)
	reDSN      = regexp.MustCompile(`, dsn=([0-9.]+)`)
	reStatus   = regexp.MustCompile(`, status=(\S+)(?: \((.*)\))?$`)
	reFrom     = regexp.MustCompile(`(?:^|[ ,;])from=<([^>]*)>`)
	reTrailTo  = regexp.MustCompile(` to=<([^>]*)>`)
	reSize     = regexp.MustCompile(`, size=(\d+)`)
	reNrcpt    = regexp.MustCompile(`, nrcpt=(\d+)`)
	reClient   = regexp.MustCompile(`client=([^,\s]+)`)
	reSASL     = regexp.MustCompile(`sasl_username=([^,\s]+)`)
	reUID      = regexp.MustCompile(`^uid=(\d+)`)
	reRejected = regexp.MustCompile(`^(reject|milter-reject|discard): (\S+) from (\S+?\[[^\]]*\])(?::\d+)?(?::|;) (.*)$`)
	reDSNInMsg = regexp.MustCompile(`\b([245]\.\d{1,3}\.\d{1,3})\b`)
	reDKIM     = regexp.MustCompile(`^DKIM-Signature field added \(s=([^,]+), d=([^)]+)\)`)
)

// Parse returns the event a line describes, or false for lines that are about no message (connects,
// warnings, statistics) or come from another program.
func Parse(l syslog.Line) (Event, bool) {
	if !strings.HasPrefix(l.Program, "postfix") && l.Program != "opendkim" && l.Program != "opendmarc" {
		return Event{}, false
	}
	e := Event{Server: l.Host, At: l.Time, Program: l.Program, Raw: l.Msg}

	if rest, ok := strings.CutPrefix(l.Msg, "NOQUEUE: "); ok {
		return e, parseReject(&e, rest)
	}
	m := queueID.FindStringSubmatch(l.Msg)
	if m == nil {
		return Event{}, false
	}
	e.QueueID = m[1]
	msg := m[2]
	service := l.Program[strings.LastIndexByte(l.Program, '/')+1:]

	switch {
	case strings.HasPrefix(msg, "client="):
		e.Kind = Client
		e.Client, e.ClientIP = splitHost(find(reClient, msg))
		e.SASLUser = find(reSASL, msg)
	case service == "pickup" && reUID.MatchString(msg):
		e.Kind = Pickup
		e.Client = "uid=" + find(reUID, msg)
	case strings.HasPrefix(msg, "message-id="):
		e.Kind = Cleanup
		e.MsgID = strings.Trim(strings.TrimPrefix(msg, "message-id="), "<>")
	case strings.HasPrefix(msg, "to=<"):
		e.Kind = Delivery
		e.To = strings.ToLower(find(reTo, msg))
		e.OrigTo = strings.ToLower(find(reOrigTo, msg))
		e.Relay, e.RelayIP = splitHost(find(reRelay, msg))
		e.Delay, _ = strconv.ParseFloat(find(reDelay, msg), 64)
		e.DSN = find(reDSN, msg)
		if s := reStatus.FindStringSubmatch(msg); s != nil {
			e.Status, e.Response = s[1], s[2]
		}
	case strings.HasPrefix(msg, "from=<") && strings.Contains(msg, "status=expired"):
		e.Kind = Expired
		e.Sender = find(reFrom, msg)
		e.Status = "expired"
		e.Response = "Gave up after the maximal queue lifetime"
	case strings.HasPrefix(msg, "from=<"):
		e.Kind = Queued
		e.Sender = find(reFrom, msg)
		e.Size, _ = strconv.ParseInt(find(reSize, msg), 10, 64)
		e.Nrcpt, _ = strconv.Atoi(find(reNrcpt, msg))
	case msg == "removed (canceled)":
		e.Kind, e.Status = Cancelled, "cancelled"
	case msg == "removed":
		e.Kind = Removed
	case strings.HasPrefix(msg, "reject: ") || strings.HasPrefix(msg, "milter-reject: ") || strings.HasPrefix(msg, "discard: "):
		if !parseReject(&e, msg) {
			e.Kind = Notice
		}
	case strings.HasPrefix(msg, "info: header Subject: ") || strings.HasPrefix(msg, "warning: header Subject: "):
		e.Kind = Header
		e.Subject = subject(msg)
	default:
		e.Kind = Notice
		if m := reDKIM.FindStringSubmatch(msg); m != nil {
			e.DKIMSelector, e.DKIMDomain = m[1], strings.ToLower(m[2])
		}
	}
	return e, true
}

// reject: RCPT from unknown[10.1.0.4]: 554 5.7.1 <a@example.org>: Sender address rejected: Access denied; from=<a@example.org> to=<b@gmail.example> proto=ESMTP helo=<app>
// reject: header Subject: hi from unknown[10.1.0.4]; from=<a@example.org> to=<b@gmail.example> proto=ESMTP helo=<app>: 5.7.1 content rejected
func parseReject(e *Event, msg string) bool {
	if strings.HasPrefix(msg, "reject: header ") || strings.HasPrefix(msg, "reject: body ") {
		e.Kind, e.Status = Reject, "rejected"
		e.Sender = find(reFrom, msg)
		e.To = strings.ToLower(find(reTrailTo, msg))
		if i := strings.LastIndex(msg, ">: "); i >= 0 {
			e.Response = msg[i+3:]
		}
		e.DSN = find(reDSNInMsg, e.Response)
		return true
	}
	m := reRejected.FindStringSubmatch(msg)
	if m == nil {
		return false
	}
	e.Kind, e.Status = Reject, "rejected"
	if m[1] == "discard" {
		e.Status = "discarded"
	}
	e.Client, e.ClientIP = splitHost(m[3])
	resp, trailer := m[4], ""
	if i := strings.LastIndex(resp, "; from=<"); i >= 0 {
		resp, trailer = resp[:i], resp[i+1:]
	}
	e.Response = resp
	e.DSN = find(reDSNInMsg, resp)
	e.Sender = find(reFrom, trailer)
	e.To = strings.ToLower(find(reTrailTo, trailer))
	return true
}

// info: header Subject: Your code from unknown[10.1.0.4]; from=<a@b> to=<c@d> proto=ESMTP helo=<app>
func subject(msg string) string {
	_, s, _ := strings.Cut(msg, "header Subject: ")
	if i := strings.LastIndex(s, "; from=<"); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndex(s, " from "); i >= 0 {
		s = s[:i]
	}
	return s
}

// gmail-smtp-in.l.google.com[173.194.220.26]:25 → gmail-smtp-in.l.google.com, 173.194.220.26
func splitHost(s string) (string, string) {
	if s == "none" {
		return "", ""
	}
	host, rest, ok := strings.Cut(s, "[")
	if !ok {
		return s, ""
	}
	ip, _, _ := strings.Cut(rest, "]")
	return host, ip
}

func find(re *regexp.Regexp, s string) string {
	if m := re.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}

// Domain is the part of an address after the last @, lower case.
func Domain(addr string) string {
	return strings.ToLower(addr[strings.LastIndexByte(addr, '@')+1:])
}

// Providers is the display order; Provider returns one of these keys.
var Providers = []string{"google", "microsoft", "yahoo", "apple", "other"}

// Provider says whose servers handle a recipient, from the MX that answered when there is one (Google
// Workspace and Microsoft 365 domains look like any other domain) and from the domain when there isn't.
func Provider(domain, relay string) string {
	relay = strings.ToLower(relay)
	for _, p := range []struct{ key, suffix string }{
		{"google", "google.com"}, {"google", "googlemail.com"},
		{"microsoft", "outlook.com"}, {"microsoft", "hotmail.com"},
		{"yahoo", "yahoodns.net"}, {"yahoo", "yahoo.com"},
		{"apple", "icloud.com"}, {"apple", "apple.com"},
	} {
		if relay == p.suffix || strings.HasSuffix(relay, "."+p.suffix) {
			return p.key
		}
	}
	switch {
	case domain == "gmail.com" || domain == "googlemail.com":
		return "google"
	case domain == "msn.com" || hasLabel(domain, "outlook", "hotmail", "live"):
		return "microsoft"
	case domain == "ymail.com" || domain == "aol.com" || hasLabel(domain, "yahoo"):
		return "yahoo"
	case domain == "icloud.com" || domain == "me.com" || domain == "mac.com":
		return "apple"
	}
	return "other"
}

// hasLabel matches consumer domains across country variants: hotmail.co.uk, yahoo.fr, live.com.
func hasLabel(domain string, names ...string) bool {
	first, _, _ := strings.Cut(domain, ".")
	for _, n := range names {
		if first == n {
			return true
		}
	}
	return false
}
