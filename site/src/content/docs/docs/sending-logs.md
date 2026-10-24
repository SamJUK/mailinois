---
title: Sending logs
description: Get Postfix's log lines to Mailinois with rsyslog, from journald-only hosts, from a boky/postfix container, through the agent, from Vector or Fluent Bit, or by importing old logs.
---

Mailinois learns everything from Postfix's log lines, and OpenDKIM's when it runs alongside. It listens for syslog on port 5514 over TCP and UDP, and takes lines over HTTPS at [`POST /api/v1/ingest`](../api/#ingest) from the agent and from log shippers. It reads RFC 5424, RFC 3164, rsyslog's forward and file formats, Postfix's own `maillog_file` format, and `boky/postfix`'s stdout format.

| Where Postfix runs | How |
| --- | --- |
| A host with rsyslog | [rsyslog](#rsyslog-on-a-postfix-host), to Mailinois's syslog port |
| A host with only journald (Debian 12 and later) | [Install rsyslog, or use Postfix's own log file](#hosts-without-rsyslog) |
| `boky/postfix` in Docker or Swarm | [Mount the rsyslog file](#bokypostfix-in-docker-or-swarm) |
| Another network, or across the internet | [The agent](#the-agent-for-servers-on-other-networks), over HTTPS with a token |
| Kubernetes | [The agent as a sidecar](../kubernetes/), or the cluster's log shipper |
| Anywhere Vector or Fluent Bit already runs | [Their HTTP output](#vector-and-fluent-bit) |
| Old log files | [`mailinois import`](#importing-old-logs) |

Each server is named by the hostname in its syslog lines, and appears when its first line arrives. Adding a relay is pointing its rsyslog at Mailinois.

:::caution[Syslog has no authentication]
Anyone who can reach port 5514 can add lines. Keep it on a private network, or firewall it to your Postfix hosts. Across the internet, use [the agent](#the-agent-for-servers-on-other-networks), which ships over HTTPS with a token.
:::

## rsyslog on a Postfix host

Copy `deploy/rsyslog/mailinois.conf` from the repository into `/etc/rsyslog.d/`:

```txt title="/etc/rsyslog.d/mailinois.conf"
# Sends Postfix and OpenDKIM lines to Mailinois, on top of wherever they already go.
#
# Drop this in /etc/rsyslog.d/ on a Postfix host, or mount it at /etc/rsyslog.d/mailinois.conf in a
# boky/postfix container, and set target to Mailinois's address. The action has its own queue: while
# Mailinois is away it holds up to 10,000 lines, then drops new ones, and never slows Postfix's own logging.
# RSYSLOG_ForwardFormat keeps the whole tag (postfix/smtp, not just postfix) and a full RFC 3339 timestamp.
if ($programname startswith "postfix" or $programname == "opendkim") then {
    action(
        type="omfwd" target="mailinois" port="5514" protocol="tcp"
        template="RSYSLOG_ForwardFormat" TCP_Framing="octet-counted"
        queue.type="LinkedList" queue.size="10000" queue.timeoutEnqueue="0"
        action.resumeRetryCount="-1" action.resumeInterval="10"
    )
}
```

Set `target` to Mailinois's address, then restart rsyslog:

```sh
sudo systemctl restart rsyslog
```

What the file does:

- It sends lines from `postfix/*` and OpenDKIM on top of wherever they already go. Postfix's own log is untouched.
- It forwards over TCP in `RSYSLOG_ForwardFormat`, which keeps the whole tag (`postfix/smtp`, not just `postfix`) and a full RFC 3339 timestamp.
- The action has its own queue. While Mailinois is away it holds up to 10,000 lines, then drops new ones rather than wait, so Postfix's own logging is never slowed.

## Hosts without rsyslog

From Debian 12, rsyslog is no longer installed by default ([the release notes](https://www.debian.org/releases/bookworm/amd64/release-notes/ch-information.en.html#changes-to-system-logging), 5.1.7): Postfix's lines go to the systemd journal only, and there is no `/var/log/mail.log`. Two ways out.

**Install rsyslog.** Debian's package writes `/var/log/mail.log` again, and takes the file above:

```sh
sudo apt install rsyslog
sudo cp mailinois.conf /etc/rsyslog.d/
sudo systemctl restart rsyslog
```

**Or let Postfix write its own log file.** Postfix 3.4 and later can log to a file through its `postlogd` service ([MAILLOG_README](https://www.postfix.org/MAILLOG_README.html)), and the agent follows it. `master.cf` needs this line, if it isn't there already:

```txt title="/etc/postfix/master.cf"
postlog   unix-dgram n  -       n       -       1       postlogd
```

```sh
sudo postconf maillog_file=/var/log/postfix.log
sudo postconf maillog_file_permissions=0644   # Postfix 3.9 and later; the default, 0600, is root's alone
sudo systemctl restart postfix
```

Then run [the agent](#the-agent-for-servers-on-other-networks) with `-file /var/log/postfix.log`. `postfix logrotate`, from cron, renames the file and Postfix starts a new one; the agent follows the new file. `0644` lets every local user read the log, which is fine on a host that only runs mail. Before Postfix 3.9 the file is root's alone, so on Debian 12 (Postfix 3.7) installing rsyslog is the simpler way.

## boky/postfix in Docker or Swarm

The `boky/postfix` image logs through rsyslog and includes `/etc/rsyslog.d/*.conf`, so mount the same file at `/etc/rsyslog.d/mailinois.conf`.

Set the service's hostname too, or every replica reports under its container ID.

With Docker Compose:

```yaml title="compose.yaml"
services:
  postfix:
    image: boky/postfix:5.1.0
    hostname: mail-1
    volumes:
      - ./mailinois.conf:/etc/rsyslog.d/mailinois.conf:ro
```

With Swarm, as a config:

```yaml title="stack.yaml"
services:
  postfix:
    image: boky/postfix:5.1.0
    hostname: "{{.Node.Hostname}}"
    configs:
      - source: mailinois-rsyslog
        target: /etc/rsyslog.d/mailinois.conf

configs:
  mailinois-rsyslog:
    file: ./mailinois.conf
```

The file's `target="mailinois"` works as it is when Mailinois runs as a service called `mailinois` on the same network.

## The agent, for servers on other networks

When a Postfix server cannot reach Mailinois over a private network, run the agent on it. The agent reads the log on the mail server and ships it to Mailinois over HTTPS, authenticated with a token. While Mailinois is unreachable, it keeps a spool on disk.

First make a token with the `ingest` scope. `mailinois token` prints a new token and its hash; the hash goes in Mailinois's config and the token goes to the agent:

```yaml
auth:
  tokens:
    - name: mail-2
      hash: "sha256:…"
      scopes: [ingest]
```

Then, on the mail server, either follow the log file:

```sh
mailinois agent -server https://mailinois.example.com -token <ingest token> -file /var/log/mail.log
```

or take rsyslog's forward locally. Set `target="127.0.0.1"` in `mailinois.conf`, and run:

```sh
mailinois agent -server https://mailinois.example.com -token <ingest token> -syslog 127.0.0.1:5514
```

The agent sends to `POST /api/v1/ingest`. It can also run the milter locally, so captures add no internet round trip to SMTP: see [Capturing headers and bodies](../capture/#with-the-agent).

| Flag | |
| --- | --- |
| `-server` | Mailinois's URL. Or `MAILINOIS_SERVER`. |
| `-token` | A token with the `ingest` scope. Or `MAILINOIS_TOKEN`, or `-token-file` to read it from a file, which keeps it out of the process list. |
| `-file` | A log file to follow. Repeat it for more than one. |
| `-syslog` | Take syslog locally on this address, such as `127.0.0.1:5514`. |
| `-name` | The server name for lines that carry no hostname, such as `docker logs` output. |
| `-milter` | Run the capture milter on this address, such as `127.0.0.1:8892`. |
| `-capture` | The milter's capture mode, `headers` (the default) or `full`. |
| `-max-size` | Bytes kept per captured message. Default `1048576`. |
| `-spool` | Where batches wait while Mailinois is unreachable. Default `/var/lib/mailinois-agent`. |

It needs `-server`, a token, and at least one of `-file`, `-syslog` or `-milter`.

To run it as a service, [Installing](../install/#the-agent-on-a-postfix-host) has a systemd unit; on Kubernetes, run it as a sidecar ([Kubernetes](../kubernetes/)).

## Vector and Fluent Bit

If a log shipper already collects Postfix's lines, such as in a cluster or a central logging setup, it can post them to the same endpoint as the agent with an `ingest` token: plain lines (Vector's `text` codec) or JSON lines with the line in `message` (Vector) or `log` (Fluent Bit). Add `?server=` to name the server when the lines carry no hostname. [The API](../api/#ingest) describes the endpoint, and [Kubernetes](../kubernetes/) has a config for each.

Captures need the milter, which only the agent or Mailinois itself runs, so a shipper brings log lines only.

## Importing old logs

`mailinois import` reads log files, or standard input with `-`, then exits:

```sh
mailinois import /var/log/mail.log.1 /var/log/mail.log
docker logs mail_postfix 2>&1 | mailinois import -server mail.example.com -
```

- It reads every format the syslog listener does.
- `-server` sets the server name for every line, for output such as `docker logs` that has no hostname in it.
- It writes straight to the database, so run it with the same configuration as the server (the same `-config` file or environment).
- Lines already stored are skipped. Importing a log twice, or a dump that overlaps the live stream, changes nothing.
