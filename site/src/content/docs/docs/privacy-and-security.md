---
title: Privacy and security
description: What Mailinois stores, how long it keeps it, and how to keep its ports private.
---

## What it stores

From the log lines, always:

- sender and recipient addresses;
- the names and IP addresses of the clients that handed mail to Postfix;
- queue IDs and Message-IDs;
- the SMTP responses from receiving servers;
- the raw log lines themselves;
- the results of the [DNS and blocklist checks](../dns-checks/).

Subjects are stored only if Postfix logs them, which it does not by default (adding `/^Subject:/ INFO` to Postfix's `header_checks` turns that on), or if a capture has them.

From [captures](../capture/), only when you turn them on: headers, or whole messages up to `capture.max_size`. With captures off, Mailinois never sees a message body.

## How long it keeps it

| Key | Default | Covers |
| --- | --- | --- |
| `retention.messages` | `720h` (30 days) | Messages, their log lines, and blocklist and DNS check results. |
| `retention.captures` | `168h` (7 days) | Captured headers and bodies. |

Anything older is deleted. Captures get their own, shorter default because they are the more sensitive of the two.

## Captures are opt-in

Captures are off until you set `capture.mode` and `listen.milter`. When you do:

- HTML parts render in a sandboxed frame, with scripts and remote content blocked, so opening a capture never loads anything from the sender;
- attachments are listed by name, type and size;
- downloading the `.eml` needs a signed-in user.

## The dashboard is read-only

Nothing in the dashboard or the API changes Postfix. There is no requeue, no delete and no configuration. Someone who gets in can read mail metadata, but cannot touch the mail flow.

## Untrusted text

SMTP responses, Message-IDs, subjects, headers and bodies all come from other people's servers. Mailinois escapes that text wherever it shows it, and its pages send a strict Content Security Policy.

## Keep the ports private

| Port | Why |
| --- | --- |
| Syslog, `5514` | Syslog has no authentication: anyone who can reach it can add lines. Keep it on a private network or firewalled to your Postfix hosts, and use the [agent](../sending-logs/#the-agent-for-servers-on-other-networks) across the internet. |
| Milter, `8892` | Only Postfix should talk to it. |
| Dashboard, `7480` | With auth `off`, anyone who can reach it can read it. Posting log lines to the ingest endpoint always needs a token with the `ingest` scope. Use [password, OIDC or proxy mode](../authentication/) with TLS in front, or keep it on a private network, a VPN or an SSH tunnel. |

## Where data goes

Mailinois runs on your own infrastructure, with its data in your own database. There is no hosted service in between.

Two things leave your network, and you configure both: [webhooks](../alerts/#webhooks), which carry an alert's name, metric, value and scope, and the DNS lookups the checks make, which tell your resolver and the blocklist operators which IPs and domains you send from.
