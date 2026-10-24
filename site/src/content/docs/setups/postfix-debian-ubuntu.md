---
title: Monitor Postfix on Debian and Ubuntu
description: Follow every message through Postfix on Debian or Ubuntu. Send /var/log/mail.log to Mailinois with one rsyslog file or the agent, deal with Debian 12's journal-only default, and capture headers through a milter that never holds up mail.
template: splash
---

Postfix on Debian already writes down everything about every message: who handed it over, each delivery attempt, and what the other end said. It is all in `/var/log/mail.log`. `grep 4F2A91C03B /var/log/mail.log` works for one queue ID. It stops working when the question is "which messages to Outlook were deferred last night, and why?", or when last week's log has been rotated and gzipped.

Mailinois takes a copy of each line as Postfix writes it, joins each message's lines by queue ID, and keeps 30 days by default. This page sets that up on Debian and Ubuntu, with Mailinois on the same machine or elsewhere.

## Which kind of host is it?

```sh
postconf -h mail_version       # Postfix's version
systemctl is-active rsyslog    # "active" means /var/log/mail.log exists
```

| Release | Postfix | rsyslog by default |
| --- | --- | --- |
| Debian 11 | 3.5 | Yes |
| Debian 12 | 3.7 | [No](https://www.debian.org/releases/bookworm/amd64/release-notes/ch-information.en.html#changes-to-system-logging) |
| Debian 13 | 3.10 | No |
| Ubuntu 22.04 | 3.6 | Check with the command above |
| Ubuntu 24.04 | 3.8 | Check with the command above |

The Postfix versions are the ones each release's archive carries (October 2026). Without rsyslog, Postfix's lines go to the systemd journal only: see [Debian 12 and later without rsyslog](#debian-12-and-later-without-rsyslog) below, then come back.

## Mailinois on the same machine

The simplest setup is Mailinois on the Postfix host itself, fed over loopback. Install the binary and its systemd unit ([Installing](../../docs/install/#binary-and-systemd) has the commands and checksums), and keep syslog on loopback in its config:

```yaml title="/etc/mailinois/mailinois.yaml"
listen:
  http: "127.0.0.1:7480"
  syslog: "127.0.0.1:5514"   # the default is every address, and syslog has no authentication
```

`MAILINOIS_PASSWORD` in `/etc/mailinois/mailinois.env` turns on sign-in, as user `admin` ([Authentication](../../docs/authentication/) has the other modes).

Then tell rsyslog to send Postfix's lines there as well as to `mail.log`:

```sh
sudo curl -fLo /etc/rsyslog.d/mailinois.conf \
  https://raw.githubusercontent.com/SamJUK/mailinois/v0.1.0/deploy/rsyslog/mailinois.conf
sudo sed -i 's/target="mailinois"/target="127.0.0.1"/' /etc/rsyslog.d/mailinois.conf
sudo rsyslogd -N1              # checks the config without restarting anything
sudo systemctl restart rsyslog
```

The file sends lines from `postfix/*` and OpenDKIM only. Its action has its own queue: while Mailinois is down it holds up to 10,000 lines and then drops new ones, so it never slows Postfix or `mail.log` ([Sending logs](../../docs/sending-logs/#rsyslog-on-a-postfix-host) explains each setting). Postfix itself doesn't change, and nothing restarts it.

That queue lives in memory. When rsyslog itself restarts while Mailinois is down (a package upgrade, say), the lines waiting in it are lost. We checked on Debian 13: with Mailinois stopped, a restart of Mailinois alone kept the queued lines and they arrived, while a restart of rsyslog lost them. `grep` the message's lines out of `mail.log` and send them through [the API](../../docs/install/#importing-old-logs-under-systemd) to fill the gap.

The server appears in Mailinois under the hostname in its syslog lines as soon as Postfix next logs something.

## Mailinois somewhere else

**On the same private network**, set `target` in the rsyslog file to Mailinois's private address, and firewall port 5514 to your Postfix hosts. Syslog has no authentication: anyone who can reach that port can add lines.

**Across the internet**, run the agent on the Postfix host. It follows `/var/log/mail.log` and ships it over HTTPS with a token, keeping a spool on disk while Mailinois is unreachable. Make an ingest token on the Mailinois side:

```sh
mailinois token    # prints a token, and the hash for the config
```

```yaml title="/etc/mailinois/mailinois.yaml (on the Mailinois server)"
auth:
  tokens:
    - name: mail-1
      hash: "sha256:…"
      scopes: [ingest]
```

Then, on the Postfix host, install the binary and `mailinois-agent.service` ([Installing](../../docs/install/#the-agent-on-a-postfix-host)), and give it its settings:

```sh title="/etc/mailinois/agent.env"
MAILINOIS_SERVER=https://mailinois.example.com
MAILINOIS_TOKEN=mlns_…
MAILINOIS_AGENT_ARGS=-file /var/log/mail.log
```

The unit joins the `adm` group, which can read `mail.log` on Debian and Ubuntu, and runs as a throwaway user otherwise. It follows the file through logrotate's renames.

## Debian 12 and later without rsyslog

From Debian 12, rsyslog isn't installed by default, and Postfix's lines are in the journal (`journalctl -t postfix/smtp`). Two ways to get a file Mailinois can read:

**Install rsyslog.** The least surprising choice:

```sh
sudo apt install rsyslog
ls -l /var/log/mail.log   # root:adm, mode 640
```

Everything above then applies unchanged.

**Postfix's own log file.** Since Postfix 3.4, [`maillog_file`](https://www.postfix.org/MAILLOG_README.html) makes Postfix write its log itself, and the agent follows that. Postfix creates the file readable by root only, and only Postfix 3.9 and later can open it up (`maillog_file_permissions`), so this route needs Debian 13:

```sh
sudo postconf 'maillog_file = /var/log/postfix.log' 'maillog_file_permissions = 0644'
sudo postfix reload
```

```sh title="/etc/mailinois/agent.env"
MAILINOIS_AGENT_ARGS=-file /var/log/postfix.log
```

Know the trade-offs. Postfix stops logging to the journal once `maillog_file` is set. `0644` lets every local user read the addresses in it. Postfix doesn't rotate the file itself: `postfix logrotate` (from cron) renames it and starts a new one, which the agent picks up. On Debian 12 or Ubuntu 24.04 (Postfix 3.7 and 3.8), install rsyslog instead.

## Capturing headers

The log has addresses, servers and answers, but not subjects. To see subjects and Message-IDs, Postfix hands each message to Mailinois's milter as well. Turn it on in Mailinois:

```yaml title="/etc/mailinois/mailinois.yaml"
listen:
  milter: "127.0.0.1:8892"
capture:
  mode: headers
```

Check whether Postfix already runs milters (OpenDKIM is the usual one):

```sh
postconf smtpd_milters non_smtpd_milters
```

Then add Mailinois after them, in braces with its own `default_action`. Here with OpenDKIM on `inet:localhost:8891`; drop it if you have none:

```sh
sudo postconf \
  'smtpd_milters = inet:localhost:8891, { inet:127.0.0.1:8892, default_action=accept }' \
  'non_smtpd_milters = inet:localhost:8891, { inet:127.0.0.1:8892, default_action=accept }'
sudo postfix reload
```

Postfix's own default for a milter it can't reach is `tempfail`: it refuses mail until the milter is back. The braces override that for Mailinois's milter alone ([MILTER_README](https://www.postfix.org/MILTER_README.html), Postfix 3.0 and later), so a Mailinois outage never holds up mail, and OpenDKIM keeps whatever you had. We checked both cases on Postfix 3.10 (details on [Postfix in Docker](../../setups/postfix-docker/#capturing-headers-in-docker-mailserver)). A plain entry with `tempfail` refused mail while Mailinois was stopped; the braces form delivered it. [Capturing headers and bodies](../../docs/capture/) covers modes and retention.

## What you get

Once lines arrive:

- **Messages:** one row per message with its sender, recipients and outcome. Search with `from:`, `to:`, `status:`, `dsn:`, `provider:` and dates.
- **Queue:** what is waiting, for how long, and the last answer for each.
- **Providers:** Google, Microsoft, Yahoo and Apple against the period before.
- **Servers and Domains:** reverse DNS and blocklists for your sending IP, and SPF, DKIM and DMARC for your domains.

[Use cases](../../use-cases/) works through the questions these answer, such as [why Postfix is deferring mail](../../use-cases/postfix-deferred-mail/).

## Do you need it?

If a daily summary email is enough, [pflogsumm](http://jimsun.linxnet.com/postfix_contrib.html) (by Jim Seymour, packaged in Debian as `pflogsumm`) reads the same log from cron, and was doing so long before Mailinois existed. If you want counts on a Grafana board, [postfix_exporter](https://github.com/Hsn723/postfix_exporter) turns the log into Prometheus metrics. Mailinois is for when you need to look up one message, or one recipient's story, and get an answer without grepping.
