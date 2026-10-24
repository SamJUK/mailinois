// Package dnscheck checks the SPF, DKIM and DMARC records a sending domain publishes.
package dnscheck

import (
	"context"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"

	"blitiri.com.ar/go/spf"
	"github.com/emersion/go-msgauth/dmarc"
)

type Result struct {
	Domain  string
	Kind    string // "spf", "dkim" or "dmarc"
	Subject string // spf: the sending IP checked; dkim: the selector; dmarc: ""
	Status  string // "pass", "warn", "fail", "missing" or "error"
	Summary string // one plain-English sentence for the dashboard
	Record  string // the TXT record as published, when there is one
}

type Checker struct {
	Resolver *net.Resolver // nil: net.DefaultResolver
}

// SPF evaluates the domain's SPF policy for each sending IP (check_host with that IP, the domain, and
// sender postmaster@<domain>), one Result per IP.
func (c *Checker) SPF(ctx context.Context, domain string, ips []string) []Result {
	dns := c.dns()
	// Only for showing the record: the check makes this lookup again and reports its errors.
	txts, _ := dns.LookupTXT(ctx, domain)
	var records []string
	for _, txt := range txts {
		if l := strings.ToLower(txt); l == "v=spf1" || strings.HasPrefix(l, "v=spf1 ") {
			records = append(records, txt)
		}
	}
	results := make([]Result, 0, len(ips))
	for _, ip := range ips {
		r := Result{Domain: domain, Kind: "spf", Subject: ip, Record: strings.Join(records, "\n")}
		r.Status, r.Summary = checkSPF(ctx, dns, domain, ip)
		results = append(results, r)
	}
	return results
}

func checkSPF(ctx context.Context, dns rooted, domain, ip string) (status, summary string) {
	addr := net.ParseIP(ip)
	if addr == nil {
		return "error", ip + " is not an IP address."
	}
	result, err := spf.CheckHostWithSender(addr, domain, "postmaster@"+domain, spf.WithResolver(dns), spf.WithContext(ctx))
	switch result {
	case spf.Pass:
		return "pass", fmt.Sprintf("%s may send for %s.", ip, domain)
	case spf.SoftFail:
		return "warn", fmt.Sprintf("%s's SPF soft-fails %s: receivers may treat its mail as suspicious.", domain, ip)
	case spf.Neutral:
		return "warn", fmt.Sprintf("%s's SPF is neutral about %s: receivers may treat its mail as suspicious.", domain, ip)
	case spf.Fail:
		return "fail", fmt.Sprintf("%s may not send for %s: receivers may reject its mail.", ip, domain)
	case spf.None:
		return "missing", domain + " publishes no SPF record."
	case spf.TempError:
		return "error", fmt.Sprintf("SPF for %s could not be checked (temperror): %s.", domain, reason(err))
	}
	return "error", fmt.Sprintf("SPF for %s is broken (permerror): %s.", domain, spfProblem(err))
}

func spfProblem(err error) string {
	switch {
	case errors.Is(err, spf.ErrMultipleRecords):
		return "the domain publishes more than one SPF record"
	case errors.Is(err, spf.ErrLookupLimitReached):
		return "evaluating it takes more than 10 DNS lookups"
	case errors.Is(err, spf.ErrVoidLookupLimitReached):
		return "more than 2 of its DNS lookups find nothing"
	case errors.Is(err, spf.ErrNoResult):
		return "an include or redirect names a domain with no SPF record"
	}
	return reason(err)
}

// DKIM checks the public key at <selector>._domainkey.<domain>.
func (c *Checker) DKIM(ctx context.Context, domain, selector string) Result {
	r := Result{Domain: domain, Kind: "dkim", Subject: selector}
	name := selector + "._domainkey." + domain
	// A record split into 255-byte strings comes back as one string, joined without spaces.
	txts, err := c.dns().LookupTXT(ctx, name)
	switch {
	case notFound(err) || err == nil && len(txts) == 0:
		r.Status, r.Summary = "missing", "No DKIM key is published at "+name+"."
	case err != nil:
		r.Status, r.Summary = "error", "Looking up the DKIM key at "+name+" failed: "+reason(err)+"."
	case len(txts) > 1:
		r.Record = strings.Join(txts, "\n")
		r.Status, r.Summary = "error", name+" has more than one TXT record, so receivers can't tell which key to use."
	default:
		r.Record = txts[0]
		r.Status, r.Summary = dkimKey(name, txts[0])
	}
	return r
}

func dkimKey(name, record string) (status, summary string) {
	tags, err := tagList(record)
	if err != nil {
		return "error", fmt.Sprintf("The DKIM record at %s can't be parsed: %v.", name, err)
	}
	if v, ok := tags["v"]; ok && v != "DKIM1" {
		return "error", fmt.Sprintf("The record at %s is not a DKIM key (v=%s).", name, v)
	}
	p, ok := tags["p"]
	if !ok {
		return "error", fmt.Sprintf("The DKIM record at %s has no p= public key.", name)
	}
	if p == "" {
		return "fail", fmt.Sprintf("The DKIM key at %s has been revoked (empty p=), so signatures made with it fail.", name)
	}
	der, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(p), ""))
	if err != nil {
		return "error", fmt.Sprintf("The p= public key at %s isn't valid base64.", name)
	}

	var key string
	short := false
	switch k := tags["k"]; k {
	case "", "rsa":
		pub := rsaKey(der)
		if pub == nil {
			return "error", fmt.Sprintf("The p= public key at %s isn't a valid RSA key.", name)
		}
		bits := pub.N.BitLen()
		key = fmt.Sprintf("%d-bit RSA key", bits)
		if bits < 1024 {
			return "fail", fmt.Sprintf("The %s at %s is too short: receivers ignore signatures from keys under 1024 bits.", key, name)
		}
		short = bits < 2048
	case "ed25519":
		if len(der) != ed25519.PublicKeySize {
			return "error", fmt.Sprintf("The Ed25519 key at %s is %d bytes long instead of %d.", name, len(der), ed25519.PublicKeySize)
		}
		key = "Ed25519 key"
	default:
		return "error", fmt.Sprintf("The DKIM key at %s has a type receivers don't support (k=%s).", name, k)
	}

	testMode := slices.ContainsFunc(strings.Split(tags["t"], ":"), func(flag string) bool { return strings.TrimSpace(flag) == "y" })
	switch {
	case testMode:
		return "warn", fmt.Sprintf("The %s at %s is in testing mode (t=y): receivers treat its signatures as if the mail were unsigned.", key, name)
	case short:
		return "warn", fmt.Sprintf("The %s at %s works, but 2048 bits is recommended.", key, name)
	}
	return "pass", fmt.Sprintf("%s publishes a valid %s.", name, key)
}

// rsaKey parses p= as a SubjectPublicKeyInfo, or as a bare RSAPublicKey, which RFC 6376 erratum 3017 also
// allows. It returns nil for anything else.
func rsaKey(der []byte) *rsa.PublicKey {
	if pub, err := x509.ParsePKIXPublicKey(der); err == nil {
		rsaPub, _ := pub.(*rsa.PublicKey)
		return rsaPub
	}
	pub, _ := x509.ParsePKCS1PublicKey(der)
	return pub
}

// tagList parses a DKIM tag list (RFC 6376 section 3.2), such as "v=DKIM1; k=rsa; p=MIIBIjAN...".
func tagList(record string) (map[string]string, error) {
	tags := map[string]string{}
	for spec := range strings.SplitSeq(record, ";") {
		if strings.TrimSpace(spec) == "" {
			continue
		}
		name, value, ok := strings.Cut(spec, "=")
		name = strings.TrimSpace(name)
		if !ok || name == "" {
			return nil, fmt.Errorf("%q is not a tag=value pair", strings.TrimSpace(spec))
		}
		if _, dup := tags[name]; dup {
			return nil, fmt.Errorf("the %s= tag appears twice", name)
		}
		tags[name] = strings.TrimSpace(value)
	}
	return tags, nil
}

// DMARC checks _dmarc.<domain> (falling back to the organisational domain as RFC 7489 says is out of
// scope: just report missing).
func (c *Checker) DMARC(ctx context.Context, domain string) Result {
	r := Result{Domain: domain, Kind: "dmarc"}
	name := "_dmarc." + domain
	txts, err := c.dns().LookupTXT(ctx, name)
	if err != nil && !notFound(err) {
		r.Status, r.Summary = "error", "Looking up the DMARC record at "+name+" failed: "+reason(err)+"."
		return r
	}
	var records []string
	for _, txt := range txts {
		if isDMARC(txt) {
			records = append(records, txt)
		}
	}
	r.Record = strings.Join(records, "\n")
	switch len(records) {
	case 0:
		r.Status, r.Summary = "missing", domain+" publishes no DMARC record at "+name+"."
		return r
	case 1:
	default:
		r.Status, r.Summary = "error", domain+" publishes more than one DMARC record, so receivers ignore them all."
		return r
	}
	rec, err := dmarc.Parse(records[0])
	if err != nil {
		r.Status, r.Summary = "error", "The DMARC record at "+name+" can't be parsed: "+strings.TrimPrefix(err.Error(), "dmarc: ")+"."
		return r
	}
	r.Status, r.Summary = dmarcPolicy(rec)
	return r
}

// isDMARC reports whether a TXT record starts with v=DMARC1, which RFC 7489 section 6.6.3 says tells
// DMARC records apart from anything else published at _dmarc.
func isDMARC(txt string) bool {
	version, _, _ := strings.Cut(txt, ";")
	tag, value, _ := strings.Cut(version, "=")
	return strings.TrimSpace(tag) == "v" && strings.TrimSpace(value) == "DMARC1"
}

func dmarcPolicy(rec *dmarc.Record) (status, summary string) {
	pct := 100
	if rec.Percent != nil {
		pct = *rec.Percent
	}
	var parts []string
	switch {
	case rec.Policy == dmarc.PolicyNone:
		status = "warn"
		parts = append(parts, "The policy p=none only monitors: receivers deliver mail that fails")
	case pct < 100:
		status = "warn"
		parts = append(parts, fmt.Sprintf("The policy p=%s covers only %d%% of mail that fails (pct=%d)", rec.Policy, pct, pct))
	case rec.Policy == dmarc.PolicyReject:
		status = "pass"
		parts = append(parts, "The policy p=reject tells receivers to refuse mail that fails")
	default:
		status = "pass"
		parts = append(parts, "The policy p=quarantine tells receivers to send mail that fails to spam")
	}
	if rec.SubdomainPolicy != "" && rec.SubdomainPolicy != rec.Policy {
		parts = append(parts, fmt.Sprintf("subdomains get sp=%s", rec.SubdomainPolicy))
	}
	switch {
	case rec.DKIMAlignment == dmarc.AlignmentStrict && rec.SPFAlignment == dmarc.AlignmentStrict:
		parts = append(parts, "DKIM and SPF alignment are strict")
	case rec.DKIMAlignment == dmarc.AlignmentStrict:
		parts = append(parts, "DKIM alignment is strict")
	case rec.SPFAlignment == dmarc.AlignmentStrict:
		parts = append(parts, "SPF alignment is strict")
	}
	if len(rec.ReportURIAggregate) == 0 {
		parts = append(parts, "no aggregate reports are requested (no rua=)")
	} else {
		var to []string
		for _, uri := range rec.ReportURIAggregate {
			to = append(to, strings.TrimPrefix(uri, "mailto:"))
		}
		parts = append(parts, "aggregate reports go to "+strings.Join(to, ", "))
	}
	return status, strings.Join(parts, "; ") + "."
}

// rooted looks every name up as absolute. Go's resolver otherwise follows a SERVFAIL for example.com by
// trying example.com.<search domain> from resolv.conf, and reports that name's NXDOMAIN instead, which
// would turn "error" into "missing".
type rooted struct{ *net.Resolver }

func (c *Checker) dns() rooted {
	if c.Resolver == nil {
		return rooted{net.DefaultResolver}
	}
	return rooted{c.Resolver}
}

func (r rooted) LookupTXT(ctx context.Context, name string) ([]string, error) {
	return r.Resolver.LookupTXT(ctx, absolute(name))
}

func (r rooted) LookupMX(ctx context.Context, name string) ([]*net.MX, error) {
	return r.Resolver.LookupMX(ctx, absolute(name))
}

func (r rooted) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return r.Resolver.LookupIPAddr(ctx, absolute(host))
}

func absolute(name string) string {
	return strings.TrimSuffix(name, ".") + "."
}

func notFound(err error) bool {
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr) && dnsErr.IsNotFound
}

// reason is why a lookup failed, without the name and server Go puts around it.
func reason(err error) string {
	var dnsErr *net.DNSError
	switch {
	case errors.As(err, &dnsErr):
		return dnsErr.Err
	case err != nil:
		return err.Error()
	}
	return "unknown error"
}
