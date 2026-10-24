package dnscheck

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"math/big"
	"strings"
	"testing"
)

func TestSPF(t *testing.T) {
	const record = "v=spf1 ip4:192.0.2.10 ip6:2001:db8::/32 a:mail.example.com -all"
	deep := "v=spf1"
	dns := &fakeDNS{
		records: []rr{
			txt("example.com", "google-site-verification=abc"),
			txt("example.com", record),
			addr("mail.example.com", "192.0.2.25"),
			addr("mail.example.com", "2a01:4f8::25"),
			txt("soft.example.com", "v=spf1 ip4:192.0.2.10 ~all"),
			txt("neutral.example.com", "v=spf1 ip4:192.0.2.10 ?all"),
			txt("nospf.example.com", "google-site-verification=abc"),
			txt("twice.example.com", "v=spf1 ip4:192.0.2.10 -all"),
			txt("twice.example.com", "v=spf1 -all"),
			txt("dangling.example.com", "v=spf1 include:nospf.example.com -all"),
		},
		servfail: map[string]bool{"broken.example.com": true},
	}
	for i := range 11 {
		include := fmt.Sprintf("include%d.example.com", i)
		deep += " include:" + include
		dns.records = append(dns.records, txt(include, "v=spf1 -all"))
	}
	dns.records = append(dns.records, txt("deep.example.com", deep+" ip4:192.0.2.10 -all"))
	c := Checker{Resolver: dns.resolver()}

	for _, tc := range []struct {
		domain, ip, status, summary string
	}{
		{"example.com", "192.0.2.10", "pass", "192.0.2.10 may send for example.com."},
		{"example.com", "2001:db8:1::25", "pass", "2001:db8:1::25 may send for example.com."}, // ip6:
		{"example.com", "2a01:4f8::25", "pass", "2a01:4f8::25 may send for example.com."},     // a: through AAAA
		{"example.com", "192.0.2.25", "pass", "192.0.2.25 may send for example.com."},         // a: through A
		{"example.com", "2a01:4f8::26", "fail", "2a01:4f8::26 may not send for example.com"},
		{"example.com", "2001:db9::25", "fail", "2001:db9::25 may not send for example.com"},
		{"example.com", "192.0.2.11", "fail", "192.0.2.11 may not send for example.com: receivers may reject"},
		{"soft.example.com", "192.0.2.10", "pass", "may send"},
		{"soft.example.com", "192.0.2.1", "warn", "soft-fails 192.0.2.1: receivers may treat its mail as suspicious"},
		{"neutral.example.com", "192.0.2.1", "warn", "neutral about 192.0.2.1: receivers may treat its mail as suspicious"},
		{"nospf.example.com", "192.0.2.10", "missing", "nospf.example.com publishes no SPF record."},
		{"nxdomain.example.com", "192.0.2.10", "missing", "publishes no SPF record"},
		{"twice.example.com", "192.0.2.10", "error", "permerror): the domain publishes more than one SPF record"},
		{"deep.example.com", "192.0.2.10", "error", "takes more than 10 DNS lookups"},
		{"dangling.example.com", "192.0.2.10", "error", "an include or redirect names a domain with no SPF record"},
		{"broken.example.com", "192.0.2.10", "error", "(temperror): server misbehaving"},
		{"example.com", "192.0.2", "error", "is not an IP address"},
	} {
		results := c.SPF(context.Background(), tc.domain, []string{tc.ip})
		if len(results) != 1 {
			t.Fatalf("SPF(%s, %s) gave %d results", tc.domain, tc.ip, len(results))
		}
		r := results[0]
		if r.Domain != tc.domain || r.Kind != "spf" || r.Subject != tc.ip {
			t.Errorf("SPF(%s, %s) = %+v: wrong domain, kind or subject", tc.domain, tc.ip, r)
		}
		if r.Status != tc.status || !strings.Contains(r.Summary, tc.summary) {
			t.Errorf("SPF(%s, %s) = %s %q, want %s containing %q", tc.domain, tc.ip, r.Status, r.Summary, tc.status, tc.summary)
		}
	}
}

func TestSPFRecordAndOneResultPerIP(t *testing.T) {
	const record = "v=spf1 ip4:192.0.2.10 ip6:2001:db8::/32 -all"
	dns := &fakeDNS{records: []rr{
		txt("example.com", "google-site-verification=abc"),
		txt("example.com", record),
		txt("twice.example.com", "v=spf1 ip4:192.0.2.10 -all"),
		txt("twice.example.com", "v=spf1 -all"),
		txt("nospf.example.com", "google-site-verification=abc"),
	}}
	c := Checker{Resolver: dns.resolver()}

	results := c.SPF(context.Background(), "example.com", []string{"192.0.2.10", "192.0.2.1", "2001:db8::1"})
	var got []string
	for _, r := range results {
		got = append(got, r.Subject+" "+r.Status)
		if r.Record != record {
			t.Errorf("%s: Record = %q, want %q", r.Subject, r.Record, record)
		}
	}
	if want := []string{"192.0.2.10 pass", "192.0.2.1 fail", "2001:db8::1 pass"}; strings.Join(got, ", ") != strings.Join(want, ", ") {
		t.Errorf("SPF results = %v, want %v", got, want)
	}

	if r := c.SPF(context.Background(), "twice.example.com", []string{"192.0.2.10"})[0]; r.Record != "v=spf1 ip4:192.0.2.10 -all\nv=spf1 -all" {
		t.Errorf("both records should be shown when there are two, got %q", r.Record)
	}
	if r := c.SPF(context.Background(), "nospf.example.com", []string{"192.0.2.10"})[0]; r.Record != "" {
		t.Errorf("a TXT record that isn't SPF shouldn't be shown as the SPF record, got %q", r.Record)
	}
	if results := c.SPF(context.Background(), "example.com", nil); len(results) != 0 {
		t.Errorf("no IPs should give no results, got %v", results)
	}
}

func TestDKIM(t *testing.T) {
	rsa2048 := rsaPublicKey(t, 2048)
	full := "v=DKIM1; k=rsa; p=" + rsa2048
	if len(full) <= 255 {
		t.Fatalf("a 2048-bit key record should need more than one 255-byte string, is %d bytes", len(full))
	}
	edKey, _, _ := ed25519.GenerateKey(nil)
	notAKey := make([]byte, 64)
	rand.Read(notAKey)

	dns := &fakeDNS{
		records: []rr{
			txt("rsa2048._domainkey.example.com", full),
			txt("split._domainkey.example.com", full[:30], full[30:200], full[200:]),
			txt("pkcs1._domainkey.example.com", "p="+pkcs1PublicKey(2048)),
			txt("ed._domainkey.example.com", "v=DKIM1; k=ed25519; p="+base64.StdEncoding.EncodeToString(edKey)),
			txt("rsa1024._domainkey.example.com", "v=DKIM1; p="+rsaPublicKey(t, 1024)),
			txt("rsa512._domainkey.example.com", "v=DKIM1; p="+rsaPublicKey(t, 512)),
			txt("revoked._domainkey.example.com", "v=DKIM1; k=rsa; p="),
			txt("testing._domainkey.example.com", "v=DKIM1; t=y; p="+rsa2048),
			txt("testing2._domainkey.example.com", "v=DKIM1; t=s:y; p="+rsa2048),
			txt("strict._domainkey.example.com", "v=DKIM1; t=s; p="+rsa2048),
			txt("garbled._domainkey.example.com", "v=DKIM1; p=not*base64"),
			txt("notakey._domainkey.example.com", "v=DKIM1; p="+base64.StdEncoding.EncodeToString(notAKey)),
			txt("shorted._domainkey.example.com", "v=DKIM1; k=ed25519; p="+base64.StdEncoding.EncodeToString(edKey[:16])),
			txt("dsa._domainkey.example.com", "v=DKIM1; k=dsa; p="+rsa2048),
			txt("spf._domainkey.example.com", "v=spf1 -all"),
			txt("nop._domainkey.example.com", "v=DKIM1; k=rsa"),
			txt("dup._domainkey.example.com", "v=DKIM1; p="+rsa2048+"; p="+rsa2048),
			txt("twice._domainkey.example.com", "v=DKIM1; p="+rsa2048),
			txt("twice._domainkey.example.com", "v=DKIM1; p="+rsaPublicKey(t, 2048)),
			addr("a-only._domainkey.example.com", "192.0.2.1"), // the name exists, with no TXT record
		},
		servfail: map[string]bool{"broken._domainkey.example.com": true},
	}
	c := Checker{Resolver: dns.resolver()}

	for _, tc := range []struct {
		selector, status, summary string
	}{
		{"rsa2048", "pass", "rsa2048._domainkey.example.com publishes a valid 2048-bit RSA key."},
		{"split", "pass", "publishes a valid 2048-bit RSA key"},
		{"pkcs1", "pass", "publishes a valid 2048-bit RSA key"},
		{"ed", "pass", "publishes a valid Ed25519 key"},
		{"strict", "pass", "2048-bit RSA key"}, // t=s alone isn't testing mode
		{"rsa1024", "warn", "1024-bit RSA key at rsa1024._domainkey.example.com works, but 2048 bits is recommended"},
		{"rsa512", "fail", "512-bit RSA key at rsa512._domainkey.example.com is too short"},
		{"revoked", "fail", "revoked"},
		{"testing", "warn", "testing mode (t=y)"},
		{"testing2", "warn", "testing mode (t=y)"},
		{"missing", "missing", "No DKIM key is published at missing._domainkey.example.com."},
		{"a-only", "missing", "No DKIM key"},
		{"garbled", "error", "isn't valid base64"},
		{"notakey", "error", "isn't a valid RSA key"},
		{"shorted", "error", "Ed25519 key at shorted._domainkey.example.com is 16 bytes long instead of 32"},
		{"dsa", "error", "(k=dsa)"},
		{"spf", "error", "is not a DKIM key (v=spf1 -all)"},
		{"nop", "error", "has no p= public key"},
		{"dup", "error", "the p= tag appears twice"},
		{"twice", "error", "more than one TXT record"},
		{"broken", "error", "failed: server misbehaving"},
	} {
		r := c.DKIM(context.Background(), "example.com", tc.selector)
		if r.Domain != "example.com" || r.Kind != "dkim" || r.Subject != tc.selector {
			t.Errorf("DKIM(%s) = %+v: wrong domain, kind or subject", tc.selector, r)
		}
		if r.Status != tc.status || !strings.Contains(r.Summary, tc.summary) {
			t.Errorf("DKIM(%s) = %s %q, want %s containing %q", tc.selector, r.Status, r.Summary, tc.status, tc.summary)
		}
	}

	for selector, want := range map[string]string{"rsa2048": full, "split": full, "missing": "", "broken": ""} {
		if r := c.DKIM(context.Background(), "example.com", selector); r.Record != want {
			t.Errorf("DKIM(%s).Record = %q, want %q", selector, r.Record, want)
		}
	}
}

func TestDMARC(t *testing.T) {
	dns := &fakeDNS{
		records: []rr{
			txt("_dmarc.reject.example.com", "v=DMARC1; p=reject; rua=mailto:dmarc@reject.example.com"),
			txt("_dmarc.quarantine.example.com", "v=DMARC1; p=quarantine; pct=100; sp=reject; adkim=s; aspf=s; rua=mailto:a@example.com,mailto:b@example.net"),
			txt("_dmarc.sameSP.example.com", "v=DMARC1; p=reject; sp=reject; aspf=s; rua=mailto:a@example.com"),
			txt("_dmarc.partial.example.com", "v=DMARC1; p=quarantine; pct=50; rua=mailto:a@example.com"),
			txt("_dmarc.none.example.com", "v=DMARC1; p=none; rua=mailto:dmarc@none.example.com"),
			txt("_dmarc.norua.example.com", "v=DMARC1; p=reject"),
			txt("_dmarc.split.example.com", "v=DMARC1; p=reject; ", "rua=mailto:a@example.com"),
			txt("_dmarc.other.example.com", "some-verification=abc"),
			txt("_dmarc.twice.example.com", "v=DMARC1; p=none"),
			txt("_dmarc.twice.example.com", "v=DMARC1; p=reject"),
			txt("_dmarc.garbled.example.com", "v=DMARC1; p=sometimes"),
			txt("_dmarc.nopolicy.example.com", "v=DMARC1; rua=mailto:a@example.com"),
		},
		servfail: map[string]bool{"_dmarc.broken.example.com": true},
	}
	c := Checker{Resolver: dns.resolver()}

	for _, tc := range []struct {
		domain, status  string
		summary, absent []string
	}{
		{"reject.example.com", "pass", []string{"The policy p=reject tells receivers to refuse mail that fails; aggregate reports go to dmarc@reject.example.com."}, nil},
		{"quarantine.example.com", "pass", []string{"p=quarantine", "subdomains get sp=reject", "DKIM and SPF alignment are strict", "aggregate reports go to a@example.com, b@example.net"}, nil},
		{"sameSP.example.com", "pass", []string{"SPF alignment is strict"}, []string{"subdomains", "DKIM"}},
		{"split.example.com", "pass", []string{"aggregate reports go to a@example.com"}, nil},
		{"norua.example.com", "pass", []string{"p=reject", "no aggregate reports are requested (no rua=)"}, nil},
		{"partial.example.com", "warn", []string{"p=quarantine covers only 50% of mail that fails (pct=50)"}, nil},
		{"none.example.com", "warn", []string{"p=none only monitors: receivers deliver mail that fails", "aggregate reports go to dmarc@none.example.com"}, []string{"no aggregate"}},
		{"missing.example.com", "missing", []string{"missing.example.com publishes no DMARC record at _dmarc.missing.example.com."}, nil},
		{"other.example.com", "missing", []string{"publishes no DMARC record"}, nil},
		{"twice.example.com", "error", []string{"more than one DMARC record"}, nil},
		{"garbled.example.com", "error", []string{"can't be parsed: invalid policy for parameter 'p'"}, nil},
		{"nopolicy.example.com", "error", []string{"can't be parsed: record is missing a 'p' parameter"}, nil},
		{"broken.example.com", "error", []string{"failed: server misbehaving"}, nil},
	} {
		r := c.DMARC(context.Background(), tc.domain)
		if r.Domain != tc.domain || r.Kind != "dmarc" || r.Subject != "" {
			t.Errorf("DMARC(%s) = %+v: wrong domain, kind or subject", tc.domain, r)
		}
		if r.Status != tc.status {
			t.Errorf("DMARC(%s) = %s %q, want %s", tc.domain, r.Status, r.Summary, tc.status)
		}
		for _, s := range tc.summary {
			if !strings.Contains(r.Summary, s) {
				t.Errorf("DMARC(%s) summary %q doesn't contain %q", tc.domain, r.Summary, s)
			}
		}
		for _, s := range tc.absent {
			if strings.Contains(r.Summary, s) {
				t.Errorf("DMARC(%s) summary %q shouldn't mention %q", tc.domain, r.Summary, s)
			}
		}
	}

	for domain, want := range map[string]string{
		"reject.example.com": "v=DMARC1; p=reject; rua=mailto:dmarc@reject.example.com",
		"split.example.com":  "v=DMARC1; p=reject; rua=mailto:a@example.com",
		"twice.example.com":  "v=DMARC1; p=none\nv=DMARC1; p=reject",
		"other.example.com":  "",
	} {
		if r := c.DMARC(context.Background(), domain); r.Record != want {
			t.Errorf("DMARC(%s).Record = %q, want %q", domain, r.Record, want)
		}
	}
}

// rsaPublicKey is a base64 SubjectPublicKeyInfo around a random modulus of exactly bits bits. Parsing a
// public key only reads the numbers, and rsa.GenerateKey refuses sizes under 1024.
func rsaPublicKey(t *testing.T, bits int) string {
	der, err := x509.MarshalPKIXPublicKey(&rsa.PublicKey{N: modulus(bits), E: 65537})
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(der)
}

func pkcs1PublicKey(bits int) string {
	return base64.StdEncoding.EncodeToString(x509.MarshalPKCS1PublicKey(&rsa.PublicKey{N: modulus(bits), E: 65537}))
}

func modulus(bits int) *big.Int {
	b := make([]byte, bits/8)
	rand.Read(b)
	b[0] |= 0x80
	b[len(b)-1] |= 1
	return new(big.Int).SetBytes(b)
}
