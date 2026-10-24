// Package dnsbl checks sending IPs against DNS blocklists and their reverse DNS.
package dnsbl

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/SamJUK/mailinois/internal/store"
)

var DefaultZones = []string{"zen.spamhaus.org", "b.barracudacentral.org", "bl.spamcop.net", "psbl.surriel.com", "bl.mailspike.net"}

type Checker struct {
	Store    *store.Store
	IPs      []string
	Zones    []string
	Interval time.Duration
	Resolver *net.Resolver
}

// Resolver uses server (host:port) for every lookup, or the system resolver when it's empty. Spamhaus
// refuses queries that arrive through big public resolvers, so point this at your own.
func Resolver(server string) *net.Resolver {
	if server == "" {
		return net.DefaultResolver
	}
	return &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, server)
	}}
}

func (c *Checker) Run(ctx context.Context) {
	for {
		c.CheckAll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(c.Interval):
		}
	}
}

// CheckAll looks every IP up in every zone, one query a second.
func (c *Checker) CheckAll(ctx context.Context) {
	for _, ip := range c.IPs {
		ptr, confirmed := c.reverse(ctx, ip)
		if err := c.Store.SavePTR(ctx, ip, ptr, confirmed, time.Now()); err != nil {
			slog.Error("save ptr", "ip", ip, "err", err)
		}
		for _, zone := range c.Zones {
			status, response := c.Check(ctx, ip, zone)
			if err := c.Store.SaveCheck(ctx, ip, zone, status, response, time.Now()); err != nil {
				slog.Error("save dnsbl check", "ip", ip, "zone", zone, "err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}
}

func (c *Checker) Check(ctx context.Context, ip, zone string) (status, response string) {
	q, err := query(ip, zone)
	if err != nil {
		return "error", err.Error()
	}
	addrs, err := c.Resolver.LookupHost(ctx, q)
	status, response = classify(addrs, err)
	if status == "listed" {
		if txt, err := c.Resolver.LookupTXT(ctx, q); err == nil && len(txt) > 0 {
			response += ": " + strings.Join(txt, " ")
		}
	}
	return status, response
}

// reverse returns the PTR name and whether it resolves back to ip (forward-confirmed reverse DNS).
func (c *Checker) reverse(ctx context.Context, ip string) (string, bool) {
	names, err := c.Resolver.LookupAddr(ctx, ip)
	if err != nil || len(names) == 0 {
		return "", false
	}
	ptr := strings.TrimSuffix(names[0], ".")
	addrs, err := c.Resolver.LookupHost(ctx, ptr)
	return ptr, err == nil && containsIP(addrs, ip)
}

// containsIP compares parsed addresses, so 2001:db8::1 matches 2001:0db8::0001.
func containsIP(addrs []string, ip string) bool {
	want := net.ParseIP(ip)
	return want != nil && slices.ContainsFunc(addrs, func(a string) bool { return net.ParseIP(a).Equal(want) })
}

// query builds the name to look up: 192.0.2.10 in zen.spamhaus.org is 10.2.0.192.zen.spamhaus.org. An
// IPv6 address is its 32 nibbles in reverse (RFC 5782): 2001:db8::1 is 1.0.0.[...].8.b.d.0.1.0.0.2.zen.spamhaus.org.
func query(ip, zone string) (string, error) {
	addr := net.ParseIP(ip)
	if addr == nil {
		return "", fmt.Errorf("%s is not an IP address", ip)
	}
	if v4 := addr.To4(); v4 != nil {
		return fmt.Sprintf("%d.%d.%d.%d.%s", v4[3], v4[2], v4[1], v4[0], zone), nil
	}
	var name strings.Builder
	for _, b := range slices.Backward(addr) {
		fmt.Fprintf(&name, "%x.%x.", b&0xf, b>>4)
	}
	return name.String() + zone, nil
}

func classify(addrs []string, err error) (string, string) {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
		return "clean", ""
	}
	if err != nil {
		return "error", err.Error()
	}
	for _, a := range addrs {
		if strings.HasPrefix(a, "127.255.255.") {
			return "error", "the blocklist refused the query (" + a + "): use your own resolver, not a public one"
		}
		if !strings.HasPrefix(a, "127.") {
			return "error", "unexpected answer " + a + " (is a resolver rewriting NXDOMAIN?)"
		}
	}
	return "listed", strings.Join(addrs, ", ")
}
