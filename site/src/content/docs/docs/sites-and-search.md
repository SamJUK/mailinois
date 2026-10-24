---
title: Sites and search
description: Group sending domains and servers into sites, and find any message with the search language.
---

## Sites

A site groups sending domains, exact sender addresses and optional servers, so one shop, brand or client can be looked at alone:

```yaml
sites:
  - name: Store A
    slug: storea
    domains: [store-a.example]   # subdomains match too
    servers: []
  - name: Store B
    slug: storeb
    domains: [store-b.example]
```

The site picker in the top bar scopes every page to one site, or shows everything. Without any sites configured, the picker lists the sending domains seen in the logs, so you can still look at one domain at a time.

Apps sharing a sending domain can use exact addresses instead:

```yaml
sites:
  - name: Accounts
    slug: accounts
    senders: [accounts@apps.example]
  - name: Forms
    slug: forms
    senders: [forms@apps.example]
```

Domains, senders and servers are alternative ways to match a message. Leave `domains` empty for sender-only sites, otherwise a domain match also includes the other app. Addresses match without case, including bounce notices sent back to that address. The same matching applies to site permissions and alerts. DNS pages show the shared domain's records for either app.

The slug is how the rest of Mailinois refers to a site: `site:storea` in search, `?site=storea` in the [API](../api/#scoping), `site: storea` on an [alert](../alerts/), and `sites: [storea]` on a [user or token](../authentication/#limiting-users-and-tokens-to-sites). Leave it out and Mailinois makes one from the name.

## Search

The search language works in the ⌘K box and on the Messages page, and in the API's `q` parameter.

Free words match addresses, subjects, queue IDs and Message-IDs:

```txt
4F2A91C03B
jane@example.org
```

Fields narrow it down:

| Field | Matches |
| --- | --- |
| `from:` | The sender. A value with `@` matches the address or part of it; a bare domain matches that domain and its subdomains. |
| `to:` | A recipient, matched the same way as `from:`. |
| `domain:` | A domain on either side, sender or recipient. |
| `status:` | `queued`, `deferred`, `sent`, `bounced`, `expired`, `deleted`, `cancelled`, `rejected` or `unknown`. Comma-separated values mean any of them: `status:bounced,expired`. |
| `provider:` | `google`, `microsoft`, `yahoo`, `apple` or `other`. |
| `server:` | The Postfix server, by hostname. |
| `site:` | A site's slug, or `@` and a sending domain: `site:@store-a.example`. |
| `dsn:` | The enhanced status code, as a prefix: `dsn:4.7` matches `4.7.650`. |
| `qid:` | The Postfix queue ID. |
| `msgid:` | The Message-ID, with or without angle brackets. |
| `subject:` | The subject. |
| `client:` | The client that handed the message to Postfix, by hostname or IP address. |
| `relay:` | The host Postfix handed the message on to, by hostname or IP address. |
| `has:capture` | Messages with [captured headers or body](../capture/). |
| `after:`, `before:` | A date such as `2026-10-18`, or a relative time such as `2d` or `6h`. These override the period picker. |

Every term must match. A leading `-` negates any field: `-status:sent`. Quote values that contain spaces: `subject:"order confirmed"`.

### Examples

Store A's mail that Microsoft deferred with 4.7.650 (its reputation throttling) in the last two days:

```txt
from:store-a.example provider:microsoft status:deferred dsn:4.7.650 after:2d
```

Everything for Store B that failed for good this week:

```txt
site:storeb status:bounced,expired after:7d
```

Security and policy failures (5.7.x) from Google in the last six hours:

```txt
provider:google dsn:5.7 after:6h
```

Order confirmations still waiting to go out:

```txt
subject:"order confirmed" status:queued,deferred
```

Mail to or from one domain that was not delivered, before 1st October 2026:

```txt
domain:example.org -status:sent before:2026-10-01
```

What Postfix refused from one app server in the last day:

```txt
client:10.0.4.12 status:rejected after:24h
```

Password reset emails with a capture you can open:

```txt
has:capture from:store-a.example subject:"password reset"
```

## What the statuses mean

| Status | Meaning |
| --- | --- |
| `queued` | In the queue, with no final answer yet. |
| `deferred` | A temporary failure. Postfix will try again. |
| `sent` | The receiving server accepted it. |
| `bounced` | A permanent failure. |
| `expired` | Postfix gave up after `maximal_queue_lifetime`. |
| `deleted` | Removed from the queue by hand, with `postsuper -d`. |
| `cancelled` | An SMTP transaction abandoned before message acceptance, for example after `RSET`. It is not queued or counted as received. |
| `rejected` | Refused by Postfix, before or after it was queued. |
| `unknown` | Postfix removed it from the queue, but no line about any of its recipients arrived. |

A message's status comes from its recipients' statuses. [How it works](../how-it-works/#status-from-the-recipients) has the order.
