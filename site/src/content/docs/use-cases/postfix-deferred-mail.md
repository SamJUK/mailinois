---
title: Why is Postfix deferring mail?
description: Read Postfix's status=deferred lines, group the 4xx answers (Microsoft's 4.7.650, Gmail's 4.7.28, Yahoo's TSS04, timeouts) by provider and code, and see what is still in the queue and for how long.
template: splash
---

A deferral is a receiving server saying "not now". Postfix keeps the message and tries again, on a schedule that backs off, until the message is delivered or `maximal_queue_lifetime` runs out (five days by default). Then it gives up, logs `status=expired` and sends the sender a non-delivery notice.

So the questions are always the same: who is deferring us, what did they say, and how long has the mail been waiting?

## What does a deferral look like in the log?

This is an example from Mailinois's test data, shaped exactly like a real Postfix line (one line, wrapped here):

```txt
postfix/smtp[103]: 1A2B3C4D5E: to=<bob@outlook.example>,
  relay=outlook-com.olc.protection.outlook.com[52.101.68.4]:25, delay=1.5,
  delays=0.1/0/0.6/0.8, dsn=4.7.650, status=deferred
  (host outlook-com.olc.protection.outlook.com[52.101.68.4] said: 451 4.7.650 The mail server
  [203.0.113.10] has been temporarily rate limited due to IP reputation. (S775)
  (in reply to MAIL FROM command))
```

The parts that matter:

- **`1A2B3C4D5E`** is the queue ID. Every line about this message carries it.
- **`dsn=4.7.650`** is the enhanced status code. A `4` at the front means temporary; `5` would be a bounce.
- **The text in brackets** is the receiving server's own answer, word for word. It is usually the best clue you'll get.
- **`delays=a/b/c/d`** splits the time: before the queue manager (which on a retry includes the wait in the deferred queue), in the queue manager, connection setup (DNS, HELO, TLS), and the transfer. In the same test data the second attempt reads `delays=600/0/0.9/1.1`: ten minutes waiting for the retry, then two seconds of work.

## Reading it by hand

For one message on one server, Postfix's own tools are enough:

```sh
grep 1A2B3C4D5E /var/log/mail.log   # every line for that queue ID
postqueue -p                        # what is in the queue now, with the last error
postcat -q 1A2B3C4D5E               # the queued message itself
qshape deferred                     # the deferred queue, by destination domain and age
```

[qshape](https://www.postfix.org/QSHAPE_README.html) in particular is underused. It shows at a glance whether one domain is backing up.

These get slow once the question is "how much of today's Outlook mail was deferred, across both relays, and with which codes?". That needs the lines grouped by message, by provider and by code, which is what Mailinois does as the lines arrive.

## The answers you'll see most

| Answer | Who | What it usually means |
| --- | --- | --- |
| `451 4.7.650 ... rate limited due to IP reputation` | Microsoft (Outlook.com, Hotmail, many Microsoft 365 domains) | Microsoft is throttling your sending IP. Common for new IPs and after a volume jump. [Outlook.com's sender support](https://sendersupport.olc.protection.outlook.com/pm/) and [SNDS](https://sendersupport.olc.protection.outlook.com/snds/) show what Microsoft sees. |
| `421 4.7.28 Gmail has detected an unusual rate of ...` | Google | Gmail is rate limiting by IP, netblock, DKIM domain or SPF domain. [Google's list of SMTP errors](https://knowledge.workspace.google.com/admin/support/troubleshooting/gmail-smtp-errors-and-codes) has every variant. |
| `421 4.7.0 ... does not have a PTR record` | Google | Your sending IP's reverse DNS is missing or doesn't match. See [blocklist and DNS monitoring](../../use-cases/blocklist-and-dns-monitoring/). |
| `421 4.7.0 [TSS04] Messages from ... temporarily deferred` | Yahoo and AOL | Yahoo is deferring for volume or complaints. [Yahoo's sender hub](https://senders.yahooinc.com/) explains its codes. |
| `dsn=4.4.1 ... Connection timed out` | Anyone | Postfix couldn't reach the receiving server at all. Either their MX is down, or outbound port 25 is blocked where you are (some cloud providers block it until you ask). |

The exact wording drifts over time and between a provider's servers. Mailinois groups deferrals by provider and status code rather than by text, so the variations count together.

## What Mailinois shows

- **The Queue page** lists what is waiting now, oldest first, on each server: how long it has waited, how many attempts, and the last answer. Under it, **"Why mail was deferred"** groups deferrals by provider and status code, with a count and the latest response for each.
- **Each message's timeline** shows every attempt in order, so a message deferred twice and delivered on the third try reads as exactly that.
- **The provider** comes from the server that answered, not the address. Mail to a company domain hosted on Microsoft 365 counts as Microsoft, because the answer came from `*.protection.outlook.com`.

## Searches

```txt
status:deferred after:24h
provider:microsoft dsn:4.7.650 after:2d
status:deferred -provider:google
dsn:4.4.1 after:6h
status:expired after:7d
```

The last one is mail Postfix gave up on this week. The [search language](../../docs/sites-and-search/#search) has every field.

## Alerts

Two rules cover most of it: a provider deferring a large share of attempts, and anything sitting in the queue too long.

```yaml
alerts:
  - name: Microsoft deferring us
    metric: deferral_rate
    above: 0.2
    window: 30m
    min_messages: 50
    provider: microsoft
    notify: [ops]

  - name: Mail stuck in the queue
    metric: queue_oldest
    above: "2h"
    notify: [ops]
```

`min_messages` keeps a quiet night from paging anyone over one deferral out of two messages. [Alerts](../../docs/alerts/) has the rest of the options.

## What it can't tell you

Mailinois reads what Postfix logged. It can't see why Microsoft rates your IP the way it does (SNDS and Google's [Postmaster Tools](https://postmaster.google.com/) can), and it never touches the queue: retrying with `postqueue -f` or deleting with `postsuper -d` stays with you. A message you delete shows up as `deleted`.
