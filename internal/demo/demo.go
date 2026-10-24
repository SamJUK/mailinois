// Package demo makes up Postfix traffic, as log lines, for trying the dashboard without a mail server.
package demo

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"strings"
	"time"

	"github.com/SamJUK/mailinois/internal/store"
	"github.com/SamJUK/mailinois/internal/syslog"
)

// Servers are the made-up relays and their sending IPs (documentation ranges).
var Servers = []struct{ Name, IP, PTR string }{
	{"relay-hel-01", "203.0.113.10", "mail.hel.example.net"},
	{"relay-fsn-01", "198.51.100.20", "mail.fsn.example.net"},
}

type destination struct {
	domain, relay, relayIP, provider string
	weight                           int
}

var destinations = []destination{
	{"gmail.example", "gmail-smtp-in.l.google.com", "142.250.27.26", "google", 40},
	{"northwind.example", "aspmx.l.google.com", "142.250.153.27", "google", 6},
	{"outlook.example", "outlook-com.olc.protection.outlook.com", "52.101.68.4", "microsoft", 10},
	{"hotmail.example", "hotmail-co-uk.olc.protection.outlook.com", "52.101.73.18", "microsoft", 8},
	{"contoso.example", "contoso-example.mail.protection.outlook.com", "52.101.41.9", "microsoft", 6},
	{"yahoo.example", "mta5.am0.yahoodns.net", "67.195.204.72", "yahoo", 7},
	{"icloud.example", "mx01.mail.icloud.com", "17.42.251.62", "apple", 7},
	{"fabrikam.example", "mx.fabrikam.example", "198.51.100.25", "other", 8},
	{"tailspin.example", "mail.tailspin.example", "203.0.113.40", "other", 8},
}

var (
	names    = []string{"amelia", "oliver", "isla", "harry", "ava", "jack", "mia", "noah", "ella", "leo", "grace", "arthur", "freya", "theo", "poppy", "finn", "accounts", "info", "orders", "support"}
	senders  = []string{"hello@acme.example", "hello@acme.example", "orders@shop.example", "no-reply@shop.example"}
	subjects = []string{"Your sign-in code", "Reset your password", "Your weekly report", "Order #%d confirmed", "Your order #%d has shipped", "Your data is ready"}
	clients  = [][2]string{{"web-1", "10.0.1.11"}, {"web-2", "10.0.1.12"}, {"worker-1", "10.0.1.21"}}
)

type outcome struct {
	dsn, status, response string
}

func sent(d destination, r *rand.Rand) outcome {
	switch d.provider {
	case "google":
		return outcome{"2.0.0", "sent", fmt.Sprintf("250 2.0.0 OK  %d %s - gsmtp", 1792000000+r.IntN(999999), token(r, 24))}
	case "microsoft":
		return outcome{"2.6.0", "sent", fmt.Sprintf("250 2.6.0 <%s@%s> [InternalId=%d, Hostname=AM7PR02MB%d.eurprd02.prod.outlook.com] Queued mail for delivery", token(r, 16), d.domain, r.Int64N(1e13), 1000+r.IntN(8999))}
	case "yahoo":
		return outcome{"2.0.0", "sent", "250 ok dirdel"}
	case "apple":
		return outcome{"2.0.0", "sent", fmt.Sprintf("250 2.0.0 Ok: queued as %s", token(r, 12))}
	}
	return outcome{"2.0.0", "sent", fmt.Sprintf("250 2.0.0 Ok: queued as %s", strings.ToUpper(token(r, 10)))}
}

func deferral(d destination, ip string) outcome {
	switch d.provider {
	case "google":
		return outcome{"4.7.28", "deferred", fmt.Sprintf("host %s[%s] said: 421-4.7.28 Gmail has detected an unusual rate of unsolicited mail originating from your IP address. To protect our users from spam, mail sent from your IP address has been temporarily rate limited. (in reply to end of DATA command)", d.relay, d.relayIP)}
	case "microsoft":
		return outcome{"4.7.650", "deferred", fmt.Sprintf("host %s[%s] said: 451 4.7.650 The mail server [%s] has been temporarily rate limited due to IP reputation. For e-mail delivery information, see https://aka.ms/postmaster (S775) (in reply to MAIL FROM command)", d.relay, d.relayIP, ip)}
	case "yahoo":
		return outcome{"4.7.0", "deferred", fmt.Sprintf("host %s[%s] said: 421 4.7.0 [TSS04] Messages from %s temporarily deferred due to unexpected volume or user complaints - 4.16.55.1; see https://postmaster.yahooinc.com/error-codes (in reply to MAIL FROM command)", d.relay, d.relayIP, ip)}
	}
	return outcome{"4.4.1", "deferred", fmt.Sprintf("connect to %s[%s]:25: Connection timed out", d.relay, d.relayIP)}
}

func bounce(d destination, to string) outcome {
	switch d.provider {
	case "google":
		return outcome{"5.1.1", "bounced", fmt.Sprintf("host %s[%s] said: 550-5.1.1 The email account that you tried to reach does not exist. Please try double-checking the recipient's email address for typos or unnecessary spaces. (in reply to RCPT TO command)", d.relay, d.relayIP)}
	case "microsoft":
		return outcome{"5.5.0", "bounced", fmt.Sprintf("host %s[%s] said: 550 5.5.0 Requested action not taken: mailbox unavailable (S2017062302). (in reply to RCPT TO command)", d.relay, d.relayIP)}
	case "yahoo":
		return outcome{"5.7.9", "bounced", fmt.Sprintf("host %s[%s] said: 554 5.7.9 Message not accepted for policy reasons.  See https://postmaster.yahooinc.com/error-codes (in reply to end of DATA command)", d.relay, d.relayIP)}
	}
	return outcome{"5.1.1", "bounced", fmt.Sprintf("host %s[%s] said: 550 5.1.1 <%s>: Recipient address rejected: User unknown in virtual mailbox table (in reply to RCPT TO command)", d.relay, d.relayIP, to)}
}

type gen struct {
	r        *rand.Rand
	server   string
	ip       string
	lines    []syslog.Line
	captures []store.Capture // only when capture is set
	capture  bool
	pid      int
	// A Microsoft reputation incident: most attempts in this window are deferred.
	incidentFrom, incidentTo time.Time
}

func (g *gen) log(at time.Time, program, format string, args ...any) {
	g.lines = append(g.lines, syslog.Line{Host: g.server, Time: at, Program: program, PID: g.pid + len(program), Msg: fmt.Sprintf(format, args...)})
}

func queueID(r *rand.Rand) string { return strings.ToUpper(token(r, 10)) }

func token(r *rand.Rand, n int) string {
	const hex = "0123456789abcdef"
	b := make([]byte, n)
	for i := range b {
		b[i] = hex[r.IntN(len(hex))]
	}
	return string(b)
}

func pick(r *rand.Rand) destination {
	total := 0
	for _, d := range destinations {
		total += d.weight
	}
	n := r.IntN(total)
	for _, d := range destinations {
		if n -= d.weight; n < 0 {
			return d
		}
	}
	return destinations[0]
}

// message logs one message's whole life, starting at t.
func (g *gen) message(t time.Time) {
	r := g.r
	c := clients[r.IntN(len(clients))]
	from := senders[r.IntN(len(senders))]
	g.pid = 1000 + r.IntN(30000)
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }

	g.log(t, "postfix/smtpd", "connect from %s[%s]", c[0], c[1])
	if r.Float64() < 0.015 {
		to := names[r.IntN(len(names))] + "@" + pick(r).domain
		g.log(t.Add(ms(20)), "postfix/smtpd", "NOQUEUE: reject: RCPT from %s[%s]: 554 5.7.1 <billing@legacy-shop.example>: Sender address rejected: Access denied; from=<billing@legacy-shop.example> to=<%s> proto=ESMTP helo=<%s>", c[0], c[1], to, c[0])
		g.log(t.Add(ms(25)), "postfix/smtpd", "disconnect from %s[%s] ehlo=1 mail=1 rcpt=0/1 rset=1 quit=1 commands=4/5", c[0], c[1])
		return
	}

	qid := queueID(r)
	n := 1
	if r.Float64() < 0.12 {
		n = 2 + r.IntN(2)
	}
	size := 2000 + r.IntN(40000)
	subject := subjects[r.IntN(len(subjects))]
	if strings.Contains(subject, "%d") {
		subject = fmt.Sprintf(subject, 10000+r.IntN(90000))
	}
	msgID := token(r, 32) + "@" + from[strings.IndexByte(from, '@')+1:]
	g.log(t.Add(ms(20)), "postfix/smtpd", "%s: client=%s[%s]", qid, c[0], c[1])
	g.log(t.Add(ms(50)), "postfix/cleanup", "%s: message-id=<%s>", qid, msgID)
	g.log(t.Add(ms(52)), "postfix/cleanup", "%s: info: header Subject: %s from %s[%s]; from=<%s> to=<%s> proto=ESMTP helo=<%s>", qid, subject, c[0], c[1], from, "", c[0])
	if strings.HasSuffix(from, "acme.example") {
		g.log(t.Add(ms(60)), "opendkim", "%s: DKIM-Signature field added (s=demo2026, d=acme.example)", qid)
	}
	g.log(t.Add(ms(90)), "postfix/qmgr", "%s: from=<%s>, size=%d, nrcpt=%d (queue active)", qid, from, size, n)
	g.log(t.Add(ms(100)), "postfix/smtpd", "disconnect from %s[%s] ehlo=1 mail=1 rcpt=%d data=1 quit=1 commands=%d", c[0], c[1], n, 4+n)

	last, pending := t, false
	var tos []string
	for range n {
		d := pick(r)
		to := names[r.IntN(len(names))] + "@" + d.domain
		tos = append(tos, to)
		at := t.Add(ms(400 + r.IntN(1500)))
		for attempt := 0; ; attempt++ {
			o := g.attempt(d, to, at, attempt)
			delay := at.Sub(t).Seconds()
			g.log(at, "postfix/smtp", "%s: to=<%s>, relay=%s[%s]:25, delay=%.2f, delays=0.05/0.01/%.2f/%.2f, dsn=%s, status=%s (%s)",
				qid, to, d.relay, d.relayIP, delay, delay*0.3, delay*0.6, o.dsn, o.status, o.response)
			if at.After(last) {
				last = at
			}
			if o.status != "deferred" {
				if o.status == "bounced" {
					g.notify(qid, from, at.Add(ms(2)))
				}
				break
			}
			at = at.Add(time.Duration(5+attempt*10+r.IntN(10)) * time.Minute)
			if at.Sub(t) > 5*24*time.Hour {
				g.log(at, "postfix/qmgr", "%s: from=<%s>, status=expired, returned to sender", qid, from)
				g.notify(qid, from, at.Add(ms(2)))
				last = at
				break
			}
			if attempt > 40 {
				pending = true
				break
			}
		}
	}
	if !pending {
		g.log(last.Add(ms(5)), "postfix/qmgr", "%s: removed", qid)
	}
	if g.capture {
		g.captures = append(g.captures, captured(qid, msgID, from, tos, subject, t.Add(ms(55)), size))
	}
}

// captured makes up what the milter would have kept: headers and a two-part body.
func captured(qid, msgID, from string, tos []string, subject string, at time.Time, size int) store.Capture {
	header := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: <%s>\r\nMIME-Version: 1.0\r\n"+
		"Content-Type: multipart/alternative; boundary=\"b1\"\r\n\r\n", from, strings.Join(tos, ", "), subject, at.Format(time.RFC1123Z), msgID)
	body := "--b1\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + subject + "\r\n\r\nHello,\r\n\r\nThis is a made-up message from Mailinois's demo mode.\r\n" +
		"--b1\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<h1 style=\"font-family:sans-serif\">" + subject + "</h1><p style=\"font-family:sans-serif\">Hello,</p>" +
		"<p style=\"font-family:sans-serif\">This is a made-up message from Mailinois's demo mode.</p>\r\n--b1--\r\n"
	return store.Capture{QueueID: qid, MessageID: msgID, Server: "mail.example", At: at, Header: []byte(header), Body: []byte(body), Size: int64(size)}
}

func (g *gen) attempt(d destination, to string, at time.Time, attempt int) outcome {
	r := g.r
	p := r.Float64()
	if d.provider == "microsoft" && !at.Before(g.incidentFrom) && at.Before(g.incidentTo) && p < 0.65 {
		return deferral(d, g.ip)
	}
	if attempt > 0 {
		if d.domain == "tailspin.example" && strings.HasPrefix(to, "accounts@") {
			return deferral(d, g.ip) // a dead mail server, retried until it expires
		}
		if p < 0.85 {
			return sent(d, r)
		}
		return deferral(d, g.ip)
	}
	switch {
	case d.domain == "tailspin.example" && strings.HasPrefix(to, "accounts@"):
		return deferral(d, g.ip)
	case p < 0.025:
		return bounce(d, to)
	case p < map[string]float64{"google": 0.04, "microsoft": 0.16, "yahoo": 0.12, "apple": 0.03, "other": 0.07}[d.provider]:
		return deferral(d, g.ip)
	}
	return sent(d, r)
}

// notify logs the bounce message Postfix sends back to the sender.
func (g *gen) notify(qid, sender string, at time.Time) {
	r := g.r
	nqid := queueID(r)
	g.log(at, "postfix/bounce", "%s: sender non-delivery notification: %s", qid, nqid)
	g.log(at.Add(time.Millisecond), "postfix/cleanup", "%s: message-id=<%s@mail.%s>", nqid, at.UTC().Format("20060102150405.")+nqid, g.server)
	g.log(at.Add(2*time.Millisecond), "postfix/qmgr", "%s: from=<>, size=%d, nrcpt=1 (queue active)", nqid, 3000+r.IntN(4000))
	at = at.Add(time.Duration(300+r.IntN(500)) * time.Millisecond)
	g.log(at, "postfix/smtp", "%s: to=<%s>, relay=mx.example.net[192.0.2.53]:25, delay=0.6, delays=0/0/0.3/0.3, dsn=2.0.0, status=sent (250 2.0.0 Ok)", nqid, sender)
	g.log(at.Add(time.Millisecond), "postfix/qmgr", "%s: removed", nqid)
}

// rate is messages an hour, quiet overnight and busiest mid-afternoon.
func rate(peak float64, t time.Time) float64 {
	h := float64(t.Hour()) + float64(t.Minute())/60
	return peak * (0.25 + 0.75*math.Pow(math.Sin(math.Pi*(h-3)/24), 4))
}

// History makes up days of traffic up to now, oldest line first.
func History(days int, now time.Time) []syslog.Line {
	var all []syslog.Line
	for i, s := range Servers {
		g := &gen{r: rand.New(rand.NewPCG(uint64(i), uint64(now.Unix()))), server: s.Name, ip: s.IP}
		g.incidentFrom = now.Add(-3 * 24 * time.Hour).Truncate(time.Hour)
		g.incidentTo = g.incidentFrom.Add(6 * time.Hour)
		peak := 14.0 / float64(i+1)
		for t := now.Add(-time.Duration(days) * 24 * time.Hour); t.Before(now); t = t.Add(time.Hour) {
			for range int(rate(peak, t) + g.r.Float64()) {
				g.message(t.Add(time.Duration(g.r.Int64N(int64(time.Hour)))))
			}
		}
		all = append(all, g.lines...)
	}
	kept := all[:0]
	for _, l := range all {
		if l.Time.Before(now) {
			kept = append(kept, l)
		}
	}
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].Time.Before(kept[j].Time) })
	return kept
}

// Live sends a made-up message every few seconds, each line and its capture at the moment it happens,
// until ctx ends.
func Live(ctx context.Context, send func(syslog.Line), keep func(store.Capture)) {
	r := rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(2+r.IntN(6)) * time.Second):
		}
		s := Servers[r.IntN(len(Servers))]
		g := &gen{r: r, server: s.Name, ip: s.IP, capture: true}
		now := time.Now()
		g.message(now)
		for _, l := range g.lines {
			timer := time.AfterFunc(l.Time.Sub(now), func() { send(l) })
			context.AfterFunc(ctx, func() { timer.Stop() })
		}
		for _, c := range g.captures {
			timer := time.AfterFunc(c.At.Sub(now), func() { keep(c) })
			context.AfterFunc(ctx, func() { timer.Stop() })
		}
	}
}

// SeedBlocklists records made-up blocklist history for the demo IPs: relay-fsn-01 spent three days on
// Barracuda's list last week.
func SeedBlocklists(ctx context.Context, st *store.Store, days int, zones []string, now time.Time) error {
	for _, s := range Servers {
		if err := st.SavePTR(ctx, s.IP, s.PTR, true, now); err != nil {
			return err
		}
		for t := now.Add(-time.Duration(days) * 24 * time.Hour); t.Before(now); t = t.Add(6 * time.Hour) {
			for _, zone := range zones {
				status, response := "clean", ""
				age := now.Sub(t)
				if s.Name == "relay-fsn-01" && zone == "b.barracudacentral.org" && age < 9*24*time.Hour && age > 6*24*time.Hour {
					status, response = "listed", "127.0.0.2: http://www.barracudanetworks.com/reputation/?pr=1&ip="+s.IP
				}
				if err := st.SaveCheck(ctx, s.IP, zone, status, response, t); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// SeedDNS records made-up SPF, DKIM and DMARC results for the demo's sending domains.
func SeedDNS(ctx context.Context, st *store.Store, now time.Time) error {
	results := []store.DNSResult{
		{Domain: "acme.example", Kind: "dkim", Subject: "demo2026", Status: "pass", Summary: "demo2026._domainkey.acme.example publishes a valid 2048-bit RSA key.", Record: "v=DKIM1; h=sha256; k=rsa; p=MIIBIjANBgkqh..."},
		{Domain: "acme.example", Kind: "dmarc", Status: "warn", Summary: "The policy p=none only monitors: receivers deliver mail that fails; no aggregate reports are requested (no rua=).", Record: "v=DMARC1; p=none"},
		{Domain: "shop.example", Kind: "dmarc", Status: "missing", Summary: "shop.example publishes no DMARC record, so receivers fall back to their own guesses."},
	}
	for _, s := range Servers {
		results = append(results,
			store.DNSResult{Domain: "acme.example", Kind: "spf", Subject: s.IP, Status: "pass", Summary: s.IP + " may send for acme.example.", Record: "v=spf1 ip4:203.0.113.10 ip4:198.51.100.20 ~all"},
			store.DNSResult{Domain: "shop.example", Kind: "spf", Subject: s.IP, Status: map[bool]string{true: "pass", false: "fail"}[s.Name == "relay-hel-01"],
				Summary: map[bool]string{true: s.IP + " may send for shop.example.", false: s.IP + " is not allowed to send for shop.example: receivers may reject it."}[s.Name == "relay-hel-01"],
				Record:  "v=spf1 ip4:203.0.113.10 -all"})
	}
	for _, r := range results {
		r.CheckedAt = now
		if err := st.SaveDNS(ctx, r); err != nil {
			return err
		}
	}
	return nil
}

// IPs are the demo sending IPs.
func IPs() []string {
	var out []string
	for _, s := range Servers {
		out = append(out, s.IP)
	}
	return out
}
