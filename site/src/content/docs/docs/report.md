---
title: One-off reports
description: Turn existing Postfix logs into one report file (interactive HTML, Markdown or JSON) without running a server. Find out what happened to one customer's email, or summarise a day.
---

`mailinois report` reads mail logs you already have and writes one file: the summary, why mail waited or failed, what was still queued, and each message's story with its log lines. It needs no config, no server and no database: it builds a throwaway one, writes the report, and deletes it.

It's for the question that arrives after the fact: a customer says the password reset never came this morning.

```sh
mailinois report -q "to:customer@example.com" /var/log/mail.log
```

That writes `mailinois-report-<date>-<time>.html` in the current directory. Open it in a browser; it works offline and loads nothing from anywhere. [Here is one](../../example-report.html), made from a week of the demo's made-up traffic from two relays.

## Which logs

| Where the log is | Command |
| --- | --- |
| `/var/log/mail.log`, and yesterday's | `mailinois report /var/log/mail.log.1 /var/log/mail.log` |
| Older, rotated and gzipped | `mailinois report /var/log/mail.log.2.gz /var/log/mail.log.1 /var/log/mail.log` (gzip is read as it is) |
| The systemd journal only (Debian 12 and later) | `journalctl SYSLOG_FACILITY=2 --since today -o short --no-pager \| mailinois report -` |
| A container | `docker logs mail 2>&1 \| mailinois report -server mail -` |
| RHEL and friends | `mailinois report /var/log/maillog` |

It reads every format the server does. Lines that aren't syslog are counted on the report and otherwise ignored.

We ran the journal line on Debian 13 with Postfix 3.10 on 19th October 2026, where there is no `/var/log/mail.log` at all: three local deliveries, a bounce and its notice came out as five messages.

## Narrowing it down

`-q` takes the dashboard's [search language](../sites-and-search/), so the same words work in both:

```sh
mailinois report -q "to:customer@example.com" /var/log/mail.log      # one customer
mailinois report -q "status:bounced after:6h" /var/log/mail.log      # what bounced in the last six hours
mailinois report -q "provider:microsoft dsn:4.7.650" /var/log/mail.log
mailinois report -q "msgid:<order-1234@shop.example>" /var/log/mail.log
```

Every count on the report covers what matched. When five or fewer messages match, the HTML report opens on their stories straight away.

The whole log is still read, because following a message needs its other lines. A report narrowed to one address takes as long as the full one.

## Formats

| `-o` ends in | You get |
| --- | --- |
| `.html` (the default) | One file with everything inline: the summary, a chart, the tables and every message, with a filter box, status buttons and a dark and light theme. |
| `.md` | The same for a ticket or a chat: tables, then each message with its recipients and its log lines in a code block. |
| `.json` | Everything, for a script. |

`-o -` writes to standard output, and `-format` picks the format when the file name doesn't.

There is no PDF format. Open the HTML report and use **Print or save as PDF**: the print layout opens every message and leaves out the buttons. We printed one with Chrome's print to PDF and got three pages, each message with its recipients and log lines.

## Flags

| Flag | |
| --- | --- |
| `-q` | Only messages matching this search. |
| `-o` | The file to write. Its extension picks the format. Default `mailinois-report-<date>-<time>.html`; `-` for standard output. |
| `-format` | `html`, `md` or `json`, when `-o` doesn't say. |
| `-server` | The server name for lines that carry none, such as `docker logs` output. |
| `-tz` | The time zone of timestamps that carry none, as in `/var/log/mail.log`. Default: this machine's. Run it on the mail server, or pass the server's zone. |
| `-limit` | How many messages to list in full, newest first. Default 1,000; the counts always cover everything that matched. |

## Is it safe to send someone?

The report holds what the log holds: addresses, remote servers, client IPs and Message-IDs. Treat it like the log.

The HTML file can't be turned against whoever opens it. The log's text goes in as data and onto the page only as text. Its Content-Security-Policy lets only the report's own script and styles run and nothing load from anywhere, so opening it makes no network request. The Markdown puts every logged value in code spans and blocks, where Markdown renders no links or images.

## How long it takes

As long as reading the log into Mailinois takes, about 30,000 lines a second on the laptop in [Sizing](../sizing/): 510,000 lines (100,000 messages) took 17.5 seconds. A quiet relay's day is done in under a second.
