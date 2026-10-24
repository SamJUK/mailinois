package dnsbl

import (
	"errors"
	"net"
	"testing"
)

func TestQuery(t *testing.T) {
	for ip, want := range map[string]string{
		"192.0.2.10":        "10.2.0.192.zen.spamhaus.org",
		"::ffff:192.0.2.10": "10.2.0.192.zen.spamhaus.org",
		"2001:db8::1":       "1.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.8.b.d.0.1.0.0.2.zen.spamhaus.org",
		"2001:0DB8::0001":   "1.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.8.b.d.0.1.0.0.2.zen.spamhaus.org",
		// RFC 5782 section 2.4's own example, with its zone swapped for ours.
		"2001:db8:1:2:3:4:567:89ab": "b.a.9.8.7.6.5.0.4.0.0.0.3.0.0.0.2.0.0.0.1.0.0.0.8.b.d.0.1.0.0.2.zen.spamhaus.org",
	} {
		if q, err := query(ip, "zen.spamhaus.org"); err != nil || q != want {
			t.Errorf("query(%s) = %q, %v, want %q", ip, q, err, want)
		}
	}
	for _, ip := range []string{"", "mail.example.com", "192.0.2", "2001:db8::1%eth0"} {
		if q, err := query(ip, "zen.spamhaus.org"); err == nil {
			t.Errorf("query(%q) = %q, want an error", ip, q)
		}
	}
}

func TestContainsIP(t *testing.T) {
	addrs := []string{"192.0.2.10", "2001:db8::1"}
	for _, ip := range []string{"192.0.2.10", "2001:db8::1", "2001:0db8::0001", "::ffff:192.0.2.10"} {
		if !containsIP(addrs, ip) {
			t.Errorf("containsIP(%v, %s) = false, want true", addrs, ip)
		}
	}
	for _, ip := range []string{"192.0.2.11", "2001:db8::2", "", "not an ip"} {
		if containsIP(addrs, ip) {
			t.Errorf("containsIP(%v, %q) = true, want false", addrs, ip)
		}
	}
	if containsIP([]string{"", "garbage"}, "") {
		t.Error("unparseable addresses must not match an unparseable IP")
	}
}

func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		addrs []string
		err   error
		want  string
	}{
		{nil, &net.DNSError{Err: "no such host", IsNotFound: true}, "clean"},
		{[]string{"127.0.0.2"}, nil, "listed"},
		{[]string{"127.0.0.4", "127.0.0.11"}, nil, "listed"},
		{[]string{"127.255.255.254"}, nil, "error"}, // Spamhaus: query through a public resolver
		{[]string{"92.242.132.24"}, nil, "error"},   // a resolver answering for NXDOMAIN
		{nil, &net.DNSError{Err: "i/o timeout", IsTimeout: true}, "error"},
		{nil, errors.New("boom"), "error"},
	} {
		if got, _ := classify(tc.addrs, tc.err); got != tc.want {
			t.Errorf("classify(%v, %v) = %s, want %s", tc.addrs, tc.err, got, tc.want)
		}
	}
}
