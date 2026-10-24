package postfix

import (
	"testing"

	"github.com/SamJUK/mailinois/internal/syslog"
)

func parse(program, msg string) (Event, bool) {
	return Parse(syslog.Line{Host: "relay", Program: program, Msg: msg})
}

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		program, msg string
		want         Event
	}{
		{"postfix/smtpd", "0DFD162666: client=app-1[10.1.0.4], sasl_method=PLAIN, sasl_username=acme",
			Event{QueueID: "0DFD162666", Kind: Client, Client: "app-1", ClientIP: "10.1.0.4", SASLUser: "acme"}},
		{"postfix/smtpd", "4dRcP31x9Lz2Kl: client=unknown[2001:db8::7]",
			Event{QueueID: "4dRcP31x9Lz2Kl", Kind: Client, Client: "unknown", ClientIP: "2001:db8::7"}},
		{"postfix/pickup", "7A8B9C0D1E: uid=33 from=<www-data>", Event{QueueID: "7A8B9C0D1E", Kind: Pickup, Client: "uid=33"}},
		{"postfix/cleanup", "0DFD162666: message-id=<a1@acme.test>", Event{QueueID: "0DFD162666", Kind: Cleanup, MsgID: "a1@acme.test"}},
		{"postfix/qmgr", "0DFD162666: from=<hello@acme.test>, size=673, nrcpt=2 (queue active)",
			Event{QueueID: "0DFD162666", Kind: Queued, Sender: "hello@acme.test", Size: 673, Nrcpt: 2}},
		{"postfix/qmgr", "0DFD162666: from=<>, size=2900, nrcpt=1 (queue active)", Event{QueueID: "0DFD162666", Kind: Queued, Size: 2900, Nrcpt: 1}},
		{"postfix/smtp", "0DFD162666: to=<Ann@Gmail.example>, orig_to=<ann@example.com>, relay=gmail-smtp-in.l.google.com[173.194.220.26]:25, conn_use=2, delay=0.78, delays=0.11/0.06/0.13/0.48, dsn=2.0.0, status=sent (250 2.0.0 OK (x) - gsmtp)",
			Event{QueueID: "0DFD162666", Kind: Delivery, To: "ann@gmail.example", OrigTo: "ann@example.com", Relay: "gmail-smtp-in.l.google.com", RelayIP: "173.194.220.26", Delay: 0.78, DSN: "2.0.0", Status: "sent", Response: "250 2.0.0 OK (x) - gsmtp"}},
		{"postfix/error", "5E6F708192: to=<frank@example.org>, relay=none, delay=30, delays=0.1/0/30/0, dsn=4.4.1, status=deferred (delivery temporarily suspended: connect to mx.example.org[203.0.113.99]:25: Connection timed out)",
			Event{QueueID: "5E6F708192", Kind: Delivery, To: "frank@example.org", Delay: 30, DSN: "4.4.1", Status: "deferred", Response: "delivery temporarily suspended: connect to mx.example.org[203.0.113.99]:25: Connection timed out"}},
		{"postfix/qmgr", "5E6F708192: from=<hello@acme.test>, status=expired, returned to sender",
			Event{QueueID: "5E6F708192", Kind: Expired, Sender: "hello@acme.test", Status: "expired", Response: "Gave up after the maximal queue lifetime"}},
		{"postfix/qmgr", "0DFD162666: removed", Event{QueueID: "0DFD162666", Kind: Removed}},
		{"postfix/cleanup", "6E59B63605: removed (canceled)", Event{QueueID: "6E59B63605", Kind: Cancelled, Status: "cancelled"}},
		{"postfix/smtpd", "NOQUEUE: reject: RCPT from unknown[10.1.0.9]: 554 5.7.1 <billing@example.org>: Sender address rejected: Access denied; from=<billing@example.org> to=<Kim@gmail.example> proto=ESMTP helo=<worker>",
			Event{Kind: Reject, Client: "unknown", ClientIP: "10.1.0.9", Sender: "billing@example.org", To: "kim@gmail.example", DSN: "5.7.1", Status: "rejected", Response: "554 5.7.1 <billing@example.org>: Sender address rejected: Access denied"}},
		{"postfix/cleanup", "8B9C0D1E2F: milter-reject: END-OF-MESSAGE from app-1[10.1.0.4]: 5.7.1 Message rejected by policy; from=<a@acme.test> to=<ivy@gmail.example> proto=ESMTP helo=<app-1>",
			Event{QueueID: "8B9C0D1E2F", Kind: Reject, Client: "app-1", ClientIP: "10.1.0.4", Sender: "a@acme.test", To: "ivy@gmail.example", DSN: "5.7.1", Status: "rejected", Response: "5.7.1 Message rejected by policy"}},
		{"postfix/cleanup", "8B9C0D1E2F: reject: header Subject: win big from app-1[10.1.0.4]; from=<a@acme.test> to=<ivy@gmail.example> proto=ESMTP helo=<app-1>: 5.7.1 Spam is not welcome",
			Event{QueueID: "8B9C0D1E2F", Kind: Reject, Sender: "a@acme.test", To: "ivy@gmail.example", DSN: "5.7.1", Status: "rejected", Response: "5.7.1 Spam is not welcome"}},
		{"postfix/cleanup", "0DFD162666: info: header Subject: Order #10 from us from app-1[10.1.0.4]; from=<a@b.c> to=<d@e.f> proto=ESMTP helo=<app-1>",
			Event{QueueID: "0DFD162666", Kind: Header, Subject: "Order #10 from us"}},
		{"opendkim", "0DFD162666: DKIM-Signature field added (s=demo2026, d=acme.test)", Event{QueueID: "0DFD162666", Kind: Notice, DKIMDomain: "acme.test", DKIMSelector: "demo2026"}},
		{"postfix/bounce", "0DFD162666: sender non-delivery notification: 1A2B3C4D5E", Event{QueueID: "0DFD162666", Kind: Notice}},
	} {
		got, ok := parse(tc.program, tc.msg)
		tc.want.Server, tc.want.Program, tc.want.Raw = "relay", tc.program, tc.msg
		if !ok || got != tc.want {
			t.Errorf("%s\n got %+v\nwant %+v", tc.msg, got, tc.want)
		}
	}
}

func TestParseIgnores(t *testing.T) {
	for _, tc := range [][2]string{
		{"postfix/smtpd", "connect from app-1[10.1.0.4]"},
		{"postfix/smtpd", "disconnect from app-1[10.1.0.4] ehlo=1 quit=1 commands=2"},
		{"postfix/smtpd", "warning: hostname app-1 does not resolve to address 10.1.0.4"},
		{"postfix/anvil", "statistics: max connection rate 1/60s for (smtp:10.1.0.4) at Oct 18 19:39:47"},
		{"postfix/smtp", "connect to mx.example.org[203.0.113.99]:25: Connection timed out"},
		{"sshd", "0DFD162666: not postfix at all"},
	} {
		if e, ok := parse(tc[0], tc[1]); ok {
			t.Errorf("%s: %q gave %+v", tc[0], tc[1], e)
		}
	}
}

func TestProvider(t *testing.T) {
	for _, tc := range [][3]string{
		{"gmail.com", "", "google"},
		{"northwind.example", "aspmx.l.google.com", "google"},
		{"contoso.example", "contoso-example.mail.protection.outlook.com", "microsoft"},
		{"hotmail.co.uk", "", "microsoft"},
		{"live.com", "", "microsoft"},
		{"yahoo.fr", "", "yahoo"},
		{"aol.com", "mx-aol.mail.gm0.yahoodns.net", "yahoo"},
		{"me.com", "", "apple"},
		{"example.com", "mx.example.com", "other"},
		{"livestock.example", "", "other"},
		{"example.com", "notgoogle.com", "other"},
	} {
		if got := Provider(tc[0], tc[1]); got != tc[2] {
			t.Errorf("Provider(%q, %q) = %s, want %s", tc[0], tc[1], got, tc[2])
		}
	}
}
