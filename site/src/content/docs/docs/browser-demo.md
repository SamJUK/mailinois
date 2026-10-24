---
title: Browser demo
description: Explore the Mailinois dashboard with simulated traffic, entirely in your browser. Search messages, follow delivery timelines and try a DNS failure.
---

[Open the interactive demo](../../demo/). It runs in JavaScript with made-up traffic from two relays and three apps. Nothing to install, no account, and no mail is sent.

## Things to try

- Open the delayed order email from the shortcut above the dashboard. Its timeline shows Microsoft deferring it and the delivery attempts so far.
- Search `provider:microsoft status:deferred`, or choose a site and server to narrow the message list.
- Open a delivered message to explore its timeline, headers, body and log lines. The example `.eml` downloads are made up too.
- Expand a row in Domains to inspect the simulated SPF, DKIM and DMARC records.
- Switch to **Broken DNS**, then open Domains and Alerts. The Newsletter app's missing DKIM key appears in both.
- Switch to **Healthy traffic** to clear the active Microsoft delay. Pause the simulation to explore a fixed snapshot.

The period picker, queue and summary counts use the same in-memory messages. The queue shows messages still waiting, including those received before the selected period, as the app does.

## What is simulated

All addresses, subjects, message content, log lines, DNS records and blocklist results are fictional. The demo uses the app's dashboard stylesheet, with a JavaScript model in place of its Go server and database. Its Search tips list the supported fields.

Traffic and delivery results are generated in the browser. It makes no API calls, DNS lookups or SMTP connections, sends no notifications and stores no messages. Reset or reload to restore the examples. The light/dark preference is saved locally when browser storage is available.

For an example of the offline HTML output, [read the example report](../../example-report.html). To run the app with its own database and simulated Postfix logs, use the [local preview](../quick-start/#try-the-local-preview).
