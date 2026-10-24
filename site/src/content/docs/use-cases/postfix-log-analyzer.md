---
title: A Postfix log analyser that follows each message
description: "Ways to read Postfix's mail log, compared honestly: grep on the queue ID, pflogsumm reports, postfix_exporter for Prometheus, Loki or Graylog, and Mailinois, which puts each message back together by queue ID."
template: splash
---

Postfix writes a good log. The trouble is that one message is spread over several lines from several processes, interleaved with every other message on the server:

```txt
postfix/smtpd[100]: 0DFD162666: client=app-1[10.1.0.4], sasl_method=PLAIN, sasl_username=acme
postfix/cleanup[101]: 0DFD162666: message-id=<a1@acme.example>
opendkim[50]: 0DFD162666: DKIM-Signature field added (s=demo2026, d=acme.example)
postfix/qmgr[90]: 0DFD162666: from=<hello@acme.example>, size=673, nrcpt=1 (queue active)
postfix/smtp[102]: 0DFD162666: to=<Ann@gmail.example>, relay=gmail-smtp-in.l.google.com[173.194.220.26]:25,
  delay=0.78, delays=0.11/0.06/0.13/0.48, dsn=2.0.0, status=sent (250 2.0.0 OK ... - gsmtp)
postfix/qmgr[90]: 0DFD162666: removed
```

(An example from Mailinois's test data, timestamps and hostnames trimmed.) The queue ID ties it together. Three things make following it harder than it looks: Postfix reuses queue IDs, so the same ID later on is a different message; a message to five people has five delivery lines that can arrive in any order; and two servers can hand out the same queue ID at the same time.

There are several good ways to deal with this. They answer different questions.

## grep on the queue ID

```sh
grep 0DFD162666 /var/log/mail.log
```

The right tool for "what happened to this one message, on this one server, today". Nothing to install. It stops working once the question spans servers, days of rotated logs, or "every message Microsoft deferred this morning".

## pflogsumm

[pflogsumm](http://jimsun.linxnet.com/postfix_contrib.html), by Jim Seymour, reads a log file and prints a summary: messages received and delivered, top senders and recipients, deferral and bounce reasons, and SMTP warnings. Most people run it from cron and get the report by email ([Debian packages it](https://packages.debian.org/stable/pflogsumm)).

**A better fit than Mailinois when** a daily summary by email is what you want: its top senders and recipients, SMTP warnings and per-hour tables go further than Mailinois's report. It's a Perl script and a cron line.

## postfix_exporter and Prometheus

[postfix_exporter](https://github.com/kumina/postfix_exporter) turns the log into Prometheus metrics: messages processed and deferred, delivery delays, rejections, bounce notifications, and the size and age of what is in the queue (read from Postfix's `showq` socket). Kumina's original hasn't changed since 2024; [hsn723's fork](https://github.com/hsn723/postfix_exporter) is still active and can read the systemd journal, Docker logs or Kubernetes pod logs as well as a file.

**A better fit when** you already run Prometheus and Grafana and want rates and Alertmanager rules next to everything else. Metrics don't keep individual messages, so "did Ann's email arrive?" isn't something it can answer. (Mailinois exposes [Prometheus metrics](../../docs/metrics/) of its own, if you want both.)

## Loki, Graylog or the Elastic stack

[Grafana Loki](https://grafana.com/oss/loki/), [Graylog](https://graylog.org/) and the [Elastic stack](https://www.elastic.co/elastic-stack) store and search every line from every server. Searching for a queue ID across all your relays is easy.

**A better fit when** you already run one of them for everything else and mail is one more log. The correlation is still yours to do: they return lines, not messages, so working out that a message is "deferred to one recipient, sent to the other two" means reading the lines yourself or writing the queries.

## Mailinois

Mailinois reads the same lines over syslog and puts each message back together as they arrive: the client that handed it over, its Message-ID, every delivery attempt for every recipient with the receiving server's answer, and a status worked out from all of them. [How it works](../../docs/how-it-works/) explains the correlation, including the reused-ID and out-of-order cases.

On top of that sit a [search language](../../docs/sites-and-search/#search) (`provider:microsoft status:deferred dsn:4.7.650 after:2d`), the queue as it stands, providers compared with the previous period, [blocklist and DNS checks](../../docs/dns-checks/), [alerts to webhooks](../../docs/alerts/) and a [REST API](../../docs/api/).

For a one-off look there's nothing to run: `mailinois report /var/log/mail.log` reads the log and writes one HTML, Markdown or JSON file with the same per-message stories, the counts, and why mail waited ([One-off reports](../../docs/report/)).

**The cost:** the dashboard is another service to run (one Go binary, SQLite built in), and it only understands Postfix.

## Side by side

| | Per-message history | Across servers | Alerts | Runs as |
| --- | --- | --- | --- | --- |
| grep | Yes, by hand | No | No | Nothing to run |
| pflogsumm | No, totals | If you merge the logs | No | A cron job |
| postfix_exporter | No, counters | Through Prometheus | Alertmanager | An exporter per server, plus Prometheus |
| Loki, Graylog, Elastic | Lines, by query | Yes | Yes, on queries | A log platform |
| Mailinois | Yes | Yes | Webhooks | One binary or container |
| `mailinois report` | Yes | If you pass each server's log | No | A command, then a file |

## Can I use more than one?

Yes, and plenty of people should. rsyslog can send the same lines to several places. Mailinois's [rsyslog file](../../docs/sending-logs/) adds a copy for Mailinois and leaves `/var/log/mail.log`, and anything else reading it, exactly as it was. Keep pflogsumm's morning email, or Loki for everything else, and use Mailinois for the questions about individual messages.
