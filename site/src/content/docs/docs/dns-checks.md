---
title: DNS and blocklist checks
description: Reverse DNS and blocklists for every sending IP, and SPF, DKIM and DMARC for every sending domain.
---

Mailinois checks your sending IPs and sending domains at start, then every `checks.interval` (default `6h`). The results show in the dashboard, in the API (`/api/v1/ips` and `/api/v1/domains`), and in the `blocklisted` and `dns_failing` [alert metrics](../alerts/#metrics).

## Sending IPs

List the public addresses your relays send from, IPv4 and IPv6:

```yaml
checks:
  sending_ips: [203.0.113.10, "2001:db8::25"]
```

or `MAILINOIS_SENDING_IPS=203.0.113.10,2001:db8::25`.

Each address gets:

- **Reverse DNS with forward confirmation.** The address's PTR name has to resolve back to the same address. Large mailbox providers expect this of anyone sending to them.
- **Blocklist lookups** in every zone in `checks.dnsbl_zones`.

The default zones are:

| Zone | Blocklist |
| --- | --- |
| `zen.spamhaus.org` | Spamhaus ZEN |
| `b.barracudacentral.org` | Barracuda |
| `bl.spamcop.net` | SpamCop |
| `psbl.surriel.com` | PSBL |
| `bl.mailspike.net` | Mailspike |

:::caution[Spamhaus and public resolvers]
Spamhaus refuses queries that come through public resolvers (Google's, Cloudflare's and the like) and answers with an address in `127.255.255.x` instead. Mailinois shows that as an error, not a listing.

Point `checks.dns_server` at a resolver of your own, such as a local Unbound, so the lookups come from your address:

```yaml
checks:
  dns_server: "127.0.0.1:53"
```
:::

## Sending domains

Mailinois checks the domains in your [sites](../sites-and-search/#sites), the sending domains it sees in the logs, and any listed in `checks.domains`.

### SPF

Each domain's SPF record is evaluated against every sending IP, so a relay missing from a domain's SPF shows up as a failure for that pair.

### DKIM

Mailinois checks the DKIM key for every selector it knows about:

- selectors seen in the logs, from OpenDKIM's `DKIM-Signature field added (s=…, d=…)` lines;
- selectors seen in [captures](../capture/);
- selectors you list:

```yaml
checks:
  dkim_selectors:
    store-a.example: [s1, s2]
```

### DMARC

Mailinois reads each domain's DMARC record and shows its policy, its alignment modes, and where aggregate (`rua`) and forensic (`ruf`) reports go.

A policy of `p=none` is flagged as monitor-only: receivers send reports, but take no action on mail that fails.
