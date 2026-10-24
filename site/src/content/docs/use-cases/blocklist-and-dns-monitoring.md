---
title: Blocklist and DNS monitoring for a mail server
description: Check every sending IP, IPv4 and IPv6, against DNS blocklists and for forward-confirmed reverse DNS, and every sending domain's SPF, DKIM and DMARC, on a schedule, with alerts when something changes.
template: splash
---

Most delivery problems that aren't the recipient's fault come down to a handful of DNS answers: is the sending IP on a blocklist, does its reverse DNS match, and do the sending domain's SPF, DKIM and DMARC records pass. They're each easy to check once. The trouble is that they change without telling you: a listing appears overnight, someone edits the SPF record for a new newsletter tool and drops your relay.

Mailinois checks all of them on a schedule (every six hours by default, and at start) and alerts when one goes wrong.

## What gets checked

| For each | Check | Fails when |
| --- | --- | --- |
| Sending IP | Reverse DNS, forward-confirmed | There is no PTR, or the PTR's name doesn't resolve back to the same IP |
| Sending IP | Each blocklist zone | The IP is listed |
| Sending domain | SPF, against every sending IP | There is no record, or a relay's IP isn't allowed (a soft-fail or neutral result is a warning) |
| Sending domain | DKIM, for every selector seen | The key is missing, revoked, or under 1024 bits (under 2048, or in testing mode, is a warning) |
| Sending domain | DMARC | There is no record (`p=none` is a warning: monitor-only) |

The default blocklists are Spamhaus ZEN, Barracuda, SpamCop, PSBL and Mailspike. Add or replace zones in the config.

Warnings show on the Domains page but don't count towards the `dns_failing` alert, so a deliberate `p=none` doesn't page anyone.

## Where the selectors come from

DKIM is checked per selector, and Mailinois finds the selectors itself. OpenDKIM logs a line for every message it signs:

```txt
opendkim[50]: 0DFD162666: DKIM-Signature field added (s=demo2026, d=acme.example)
```

That `s=` and `d=` pair is enough to look up `demo2026._domainkey.acme.example`. Selectors also come from [captured headers](../../docs/capture/), and you can list your own for signers that don't log.

## The config

```yaml
checks:
  interval: 6h
  sending_ips: [203.0.113.10, "2001:db8::25"]
  dnsbl_zones: [zen.spamhaus.org, b.barracudacentral.org, bl.spamcop.net, psbl.surriel.com, bl.mailspike.net]
  domains: [store-a.example]          # on top of the domains in your sites and those seen in the logs
  dkim_selectors:
    store-a.example: [s1, s2]         # signers that don't log their selector
```

IPv6 addresses are looked up the way [RFC 5782](https://www.rfc-editor.org/rfc/rfc5782) describes: all 32 nibbles, reversed, so `2001:db8::1` becomes `1.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.8.b.d.0.1.0.0.2.zen.spamhaus.org`. Not every list publishes IPv6 data, so check each list's own documentation before relying on it for an IPv6 relay.

## What happened when we pointed it at our own relay

The first real Mailinois watches the relay that sends copilot-seo.com's mail. We ran its checks against that domain on 18th October 2026, the day before it went in beside the relay:

- **SPF passed** for the relay's IP.
- **DKIM passed** with a 2048-bit key, for the selector OpenDKIM logged.
- **DMARC was flagged** for `p=none`: receivers report, but deliver mail that fails. True, and on purpose while the reports come in, so the warning stays until the policy goes up.
- **Spamhaus answered `127.255.255.254`** when we queried it from the relay's own server, while getting ready to deploy. That's not a listing: it's Spamhaus refusing queries that arrive through a public or shared resolver, which is what a Hetzner server uses by default. Mailinois shows it as an error rather than a listing, so nobody gets paged for it. We took Spamhaus out of that config and check it by hand until there's a resolver of our own.

If you run your own resolver (a local Unbound, say), point the checks at it and Spamhaus answers normally:

```yaml
checks:
  dns_server: "127.0.0.1:53"
```

## Alerts

```yaml
alerts:
  - name: Listed on a blocklist
    metric: blocklisted
    above: 0
    notify: [ops]
    repeat: 24h

  - name: SPF, DKIM, DMARC or PTR failing
    metric: dns_failing
    above: 0
    notify: [ops]
    repeat: 24h
```

`above: 0` fires on the first listing or failure, and resolves when the next check comes back clean. [Alerts](../../docs/alerts/) has the webhook formats (JSON, Slack and Discord).

## Other tools worth knowing

- **[MXToolbox](https://mxtoolbox.com/blacklists.aspx)** checks an IP against far more lists than Mailinois does, by hand. Better for a one-off look, and for lists you wouldn't want to query every six hours.
- **[Google Postmaster Tools](https://postmaster.google.com/)** and **Microsoft's [SNDS](https://sendersupport.olc.protection.outlook.com/snds/)** show how Gmail and Outlook.com rate your IP and domain. Mailinois can't see that: it only knows what the DNS says and what the receiving servers answer.
- **[Spamhaus](https://www.spamhaus.org/)** has its own lookup and delisting pages, and a Data Query Service for anyone who has to query it from shared infrastructure.

[DNS and blocklist checks](../../docs/dns-checks/) in the docs has every option.
