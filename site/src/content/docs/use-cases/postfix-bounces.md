---
title: Tracking Postfix bounces
description: Find every message Postfix bounced or gave up on, read its 5.x.x status code, follow the non-delivery notice it sent, alert on a rising bounce rate, and pull the bad addresses out through the API.
template: splash
---

A bounce is a permanent failure. Postfix logs `status=bounced` against the recipient, sends the envelope sender a non-delivery notice, and removes the message. An expiry ends the same way, only slower: five days of deferrals, then `status=expired`.

## What does a bounce look like in the log?

An example from Mailinois's test data, in the shape Postfix writes it (wrapped here, one line in the log):

```txt
postfix/smtp[106]: 2B3C4D5E6F: to=<nobody@example.net>, relay=mx.example.net[198.51.100.7]:25,
  delay=0.6, delays=0.1/0/0.2/0.3, dsn=5.1.1, status=bounced
  (host mx.example.net[198.51.100.7] said: 550 5.1.1 <nobody@example.net>: Recipient address
  rejected: User unknown in virtual mailbox table (in reply to RCPT TO command))
postfix/bounce[107]: 2B3C4D5E6F: sender non-delivery notification: 3C4D5E6F70
postfix/qmgr[90]: 3C4D5E6F70: from=<>, size=2900, nrcpt=1 (queue active)
```

The second line is Postfix creating the notice, a new message with its own queue ID (`3C4D5E6F70`) and an empty sender, `<>`. The third is that notice queued for delivery back to whoever sent the original.

An expiry reads like this:

```txt
postfix/qmgr[90]: 5E6F708192: from=<hello@acme.example>, status=expired, returned to sender
```

In Mailinois the original message's timeline carries the notification line with the notice's queue ID, and the notice is a message of its own, from `<>`, with its own delivery.

## What do the codes mean?

The first digit says how permanent, the rest say what went wrong ([RFC 3463](https://www.rfc-editor.org/rfc/rfc3463) defines them). The ones you'll meet most:

| Code | Meaning | What to do |
| --- | --- | --- |
| `5.1.1` | No such mailbox | Stop sending to it. A typo at sign-up, or someone who left. |
| `5.2.2` | Mailbox full | The address exists. Postfix won't retry a `5`, so send again in a few days, and drop it if it keeps failing. |
| `5.7.1` | Refused by policy | Read the text: a blocklist, a content filter, or a sender the receiver doesn't accept. |
| `5.7.26` | Gmail: unauthenticated sender | Your SPF, DKIM or DMARC isn't passing. See [blocklist and DNS monitoring](../../use-cases/blocklist-and-dns-monitoring/). |

`5.7.x` from one provider, rather than `5.1.1` spread across many, usually means the problem is your sending setup, not your list.

## Searches

```txt
status:bounced after:7d
status:bounced,expired site:storea
dsn:5.1.1 after:30d
provider:google dsn:5.7 after:24h
to:example.net status:bounced
```

`to:` with a bare domain matches that domain and its subdomains. [Sites and search](../../docs/sites-and-search/) has the full language.

## A bounce-rate alert

```yaml
alerts:
  - name: Bounce rate over 5%
    metric: bounce_rate
    above: 0.05
    window: 1h
    min_messages: 20
    notify: [ops]
    repeat: 6h
```

`bounce_rate` is the share of final outcomes in the window that failed. Add `site:` to watch one sending domain group, or `provider:` to watch one mailbox provider. [Alerts](../../docs/alerts/) lists every metric.

## Feeding bounces back to your app

Mailinois doesn't keep a suppression list, and doesn't stop anything being sent. It does make the list easy to pull, with a read token:

```sh
curl -G -H "Authorization: Bearer $TOKEN" https://mailinois.example.com/api/v1/messages \
  --data-urlencode 'q=status:bounced dsn:5.1.1 after:7d' \
  --data-urlencode 'limit=500'
```

Each message in the list carries a recipient and the number of recipients. For messages that went to more than one, `GET /api/v1/messages/{id}` has every recipient with its own status, so a nightly job can mark exactly the addresses that bounced. The [API reference](../../docs/api/) has the response shapes and paging.

## The bounces Mailinois never sees

Some receivers accept a message with `250` and only later decide they can't deliver it. They then send a bounce of their own, as a new email, to your envelope sender. Postfix logged `status=sent`, so Mailinois shows the message as sent.

Those late bounces land in the mailbox behind your `Return-Path`, and nothing joins them to the original message. Read that mailbox, or point the return path at something that processes bounces, if late bounces matter to you.
