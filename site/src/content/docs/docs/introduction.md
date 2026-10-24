---
title: Introduction
description: What Mailinois is, what it is not, and what it needs to run.
---

Mailinois watches Postfix. It reads Postfix's logs over syslog, follows every message by queue ID from the app that handed it over to each recipient's final answer, and serves a dashboard of what happened.

One Mailinois can watch any number of Postfix servers, and any number of sending domains on them.

## What it shows

- **Every message's timeline:** the client that handed it over, its Message-ID, each delivery attempt and each recipient's final answer, in order.
- **Deferrals and bounces, explained:** grouped by provider (Google, Microsoft, Yahoo, Apple, other) and compared with the previous period.
- **The queue:** what is waiting on each server, and why.
- **Sites:** sending domains and servers grouped so each can be looked at alone, or all together. See [Sites and search](../sites-and-search/).
- **Captured headers and bodies,** if you turn them on. See [Capturing headers and bodies](../capture/).
- **SPF, DKIM, DMARC, reverse DNS and blocklists** for your sending domains and IPs. See [DNS and blocklist checks](../dns-checks/).

Around that sit [alerts to webhooks](../alerts/), a [REST API](../api/) and [Prometheus metrics](../metrics/).

## What it is not

**Not a sending platform.** Mailinois sends no mail for you. There is no sending API, no templates and no lists. Your apps keep handing mail to Postfix exactly as they do now.

**Not an SMTP server.** Postfix accepts, queues and delivers. Mailinois is never in the delivery path: if it or its database goes down, Postfix carries on. rsyslog holds up to 10,000 lines for it while it is away (the agent keeps a spool on disk), and anything beyond that is missing from the dashboard and nowhere else.

The one place Mailinois meets SMTP is the optional milter for captures. Postfix is told to accept mail whenever Mailinois's milter is unreachable (`default_action=accept` on that milter), so mail still flows with Mailinois down.

## What it needs

- **Postfix,** logging through rsyslog (on the host, or inside a `boky/postfix` container), or a log file the agent can read.
- **Somewhere to run it:** Docker, or the binary. SQLite is built in. PostgreSQL is optional.
- **A way for log lines to reach it:** a private network path to port 5514 for syslog, or HTTPS for the agent when the Postfix server is on another network.

The [quick start](../quick-start/) gets it running.
