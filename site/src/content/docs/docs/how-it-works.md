---
title: How it works
description: How Mailinois follows a message by queue ID, works out its status from its recipients, and counts the queue from log lines.
---

Postfix logs a line for every step a message takes, each tagged with the message's queue ID. Mailinois reads those lines and puts each message back together.

## A message in the log

Here is one message, with two recipients, as Postfix logs it:

```txt
Oct 18 09:14:02 mail-1 postfix/smtpd[2211]: 4F2A91C03B: client=app-1.internal[10.0.4.12]
Oct 18 09:14:02 mail-1 postfix/cleanup[2214]: 4F2A91C03B: message-id=<order-1042@store-a.example>
Oct 18 09:14:02 mail-1 postfix/qmgr[1190]: 4F2A91C03B: from=<orders@store-a.example>, size=18342, nrcpt=2 (queue active)
Oct 18 09:14:03 mail-1 postfix/smtp[2220]: 4F2A91C03B: to=<jane@gmail.example>, relay=gmail-smtp-in.l.google.com[198.51.100.27]:25, delay=1.2, delays=0.1/0/0.4/0.7, dsn=2.0.0, status=sent (250 2.0.0 OK)
Oct 18 09:14:04 mail-1 postfix/smtp[2221]: 4F2A91C03B: to=<sam@outlook.example>, relay=outlook-com.olc.protection.outlook.com[198.51.100.40]:25, delay=2.1, delays=0.1/0/1.2/0.8, dsn=4.7.650, status=deferred (host outlook-com.olc.protection.outlook.com[198.51.100.40] said: 451 4.7.650 The mail server [203.0.113.10] has been temporarily rate limited due to IP reputation.)
```

From these lines Mailinois knows the client that handed the message over, its Message-ID and sender, and each recipient's latest answer. The message shows as `deferred`: one recipient has its mail, the other is waiting for Postfix to try again.

## From line to event

1. Mailinois listens for syslog on TCP and UDP. It reads RFC 5424, RFC 3164, rsyslog's forward and file formats, and `boky/postfix`'s stdout format.
2. Lines from `postfix/*` and OpenDKIM become events about a queue ID: client, Message-ID, queued, delivery attempt, expiry, removal, rejection, subject header, or a notice.
3. One writer applies the events in order, in batches, to three tables: `messages` (one per queue ID, or per refused attempt), `recipients` (one per recipient, carrying its latest attempt) and `events` (every line, for the timeline and for counting).

## Correlation by queue ID

Following a queue ID sounds simple. Four things make it less so:

- **Queue IDs are reused.** Postfix hands out the same ID again later. A queue ID that comes back is a new message, not more lines for the old one.
- **Lines arrive out of order.** Syslog over several connections, and rsyslog's own queue, can deliver a later line first. Mailinois still puts each line on the right message.
- **Servers are kept apart.** Two servers can use the same queue ID at the same time. Every message belongs to the server named in its lines, so the two never mix.
- **The same line twice is skipped.** Importing a log again, or a `docker logs` dump that overlaps the live stream, changes nothing.

A message Postfix refuses before it is queued has no queue ID. Each refused attempt becomes a message of its own, with the status `rejected`.

## Status from the recipients

A message's status follows from its recipients' statuses. It takes the first of these that any recipient has:

1. `deferred`
2. `queued`
3. `expired`
4. `bounced`
5. `deleted`
6. `rejected`
7. `sent`

So a message to ten people shows as `deferred` while even one of them is still being retried, and as `sent` only once all ten have their mail. A message with one bounce and nine deliveries shows as `bounced`.

Postfix can allocate a queue ID before accepting a message. A cleanup line ending `removed (canceled)` closes an abandoned SMTP transaction: Mailinois keeps it as `cancelled`, removes it from the queue and excludes it from the Received count. Upgrades repair previously stored cancelled transactions. A sender that was never logged displays as unknown; a logged null sender `<>` remains a bounce notice.

## Queue depth from the logs

Mailinois never reads Postfix's queue directly. A message counts as in the queue from the moment Postfix accepts it until Postfix logs `removed`, and the queue view and `queue_depth` are worked out from that.

If a message's `removed` line is lost, it leaves the queue view after `queue_lifetime`. Set that to match Postfix's `maximal_queue_lifetime` (Postfix's default is five days, `120h`, the same as Mailinois's).

## Servers

Each server is named by the hostname in its syslog lines, and appears when its first line arrives. There is nothing to register: adding a relay is pointing its rsyslog at Mailinois.
