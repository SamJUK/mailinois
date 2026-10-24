---
title: Works with
description: Where Postfix keeps its log on Debian, Ubuntu and RHEL, in boky/postfix, docker-mailserver, mailcow, iRedMail, Mail-in-a-Box and Modoboa, how to get it to Mailinois, and which of these have been run against it.
---

Mailinois reads Postfix's own log lines, and OpenDKIM's. Anything built on Postfix writes those lines, so the parsing is the same everywhere. What changes from one setup to the next is where the lines end up, and so how you get a copy to Mailinois.

| Setup | Where Postfix's log goes | Way in | Run against Mailinois |
| --- | --- | --- | --- |
| [boky/postfix](#bokypostfix) | rsyslog inside the container, to its output | Mount the rsyslog file | Yes: the compose preview, and production since 19th October 2026 |
| [docker-mailserver](#docker-mailserver) | rsyslog inside the container, to `/var/log/mail/mail.log` | Mount the rsyslog file | Yes: version 16.0.1, on 19th October 2026 |
| [mailcow](#mailcow) | syslog-ng inside `postfix-mailcow`, to its output and Redis | A hook that adds a syslog-ng destination | The hook and its syslog-ng config, in mailcow's Postfix image alone |
| [Debian and Ubuntu](#debian-and-ubuntu) | rsyslog, `/var/log/mail.log` | The rsyslog file, or the agent | The agent's systemd unit, on Debian 13 ([Installing](../../docs/install/#the-agent-on-a-postfix-host)) |
| [Debian 12 and later without rsyslog](#debian-12-and-later-without-rsyslog) | The systemd journal only | Install rsyslog, or Postfix's `maillog_file` | The log format only |
| [RHEL, Rocky and Alma](#rhel-rocky-and-alma) | rsyslog, `/var/log/maillog` | The rsyslog file, or the agent | No |
| [iRedMail](#iredmail-mail-in-a-box-and-modoboa) | The system's rsyslog | As for its OS | No |
| [Mail-in-a-Box](#iredmail-mail-in-a-box-and-modoboa) | rsyslog on Ubuntu 22.04, `/var/log/mail.log` | As for Ubuntu | No |
| [Modoboa](#iredmail-mail-in-a-box-and-modoboa) | The system's rsyslog | As for its OS | No |

"No" means nobody has pointed that exact setup at Mailinois yet. Its lines are Postfix's standard ones in a file or stream Mailinois already reads, so it should work, but that is a reading of the setup's source, not a result.

## boky/postfix

[boky/postfix](https://github.com/bokysan/docker-postfix) logs through rsyslog inside the container and reads `/etc/rsyslog.d/*.conf`. Mount Mailinois's rsyslog file there and set the container's hostname. [Sending logs](../../docs/sending-logs/#bokypostfix-in-docker-or-swarm) has the Compose and Swarm versions, and [Postfix in Docker](../../setups/postfix-docker/) the whole stack.

This is the setup Mailinois is developed against: the repository's `compose.yaml` runs it, and Mailinois watches a boky/postfix relay in production.

## docker-mailserver

In [docker-mailserver](https://github.com/docker-mailserver/docker-mailserver), Postfix logs to an rsyslog inside the container, which writes `/var/log/mail/mail.log`. The image's [Dockerfile](https://github.com/docker-mailserver/docker-mailserver/blob/master/Dockerfile) points Debian's mail log there, and its [start script](https://github.com/docker-mailserver/docker-mailserver/blob/master/target/scripts/start-mailserver.sh) ends with `tail -F` of that file, which is why `docker logs` shows it. The image's `/etc/rsyslog.conf` includes `/etc/rsyslog.d/*.conf`, so the same rsyslog file works:

```yaml title="compose.yaml"
services:
  mailserver:
    image: ghcr.io/docker-mailserver/docker-mailserver:16.0.1
    hostname: mail.example.com
    volumes:
      - ./mailinois.conf:/etc/rsyslog.d/mailinois.conf:ro
      # …the volumes from docker-mailserver's own compose.yaml
```

What we ran, on 19th October 2026, with docker-mailserver 16.0.1 (Debian 13, Postfix 3.10.13):

- With the rsyslog file mounted and pointed at a local Mailinois, three messages arrived: a delivery to a local mailbox, a message to `example.com` (which publishes a null MX, so Postfix bounced it without connecting anywhere), and the bounce notice. They showed up under server `mail`, the short form of the container's hostname.
- `docker logs mailserver 2>&1 | mailinois import -server dms -` stored the same three. Seven of the 40 lines were docker-mailserver's own start-up messages, which `import` skipped.
- Header capture through `postfix-main.cf`, and mail still flowing with Mailinois stopped. Both are on [Postfix in Docker](../../setups/postfix-docker/#docker-mailserver).

docker-mailserver's compose file mounts `/var/log/mail` from the host (`./docker-data/dms/mail-logs/`), so the agent can follow `mail.log` there instead, when Mailinois is on another network.

## mailcow

mailcow's Postfix container has no rsyslog. It runs syslog-ng, and its [syslog-ng.conf](https://github.com/mailcow/mailcow-dockerized/blob/master/data/Dockerfiles/postfix/syslog-ng.conf) sends every line to the container's output and the mail lines to Redis, which mailcow's own UI reads. The container's [entrypoint](https://github.com/mailcow/mailcow-dockerized/blob/master/data/Dockerfiles/postfix/docker-entrypoint.sh) runs each executable in `data/hooks/postfix/` before the daemons start. A hook can add a destination for Mailinois without touching the image.

[mailcow](../../setups/mailcow/) has the hook, the Compose override and the gotchas. In short:

- The hook runs on every start, so it has to check it hasn't already added itself.
- syslog-ng would name the server after the container's ID, which changes when the container is recreated, so the hook sets a fixed name.
- mailcow sets `milter_default_action = tempfail`. Add Mailinois's milter with its own `default_action=accept`, or Mailinois being down makes Postfix refuse mail.

What we ran: the hook and its syslog-ng config inside `ghcr.io/mailcow/postfix:3.10.12-1` (syslog-ng 4.8.1), without the rest of mailcow. Run twice, it added the destination once, and `syslog-ng --syntax-only` passed. A Postfix-shaped message logged through `/dev/log` arrived in Mailinois under server `mailcow`, while mailcow's own copy kept the container's hostname. The full stack has not been run.

## Debian and Ubuntu

With rsyslog, Postfix's lines are in `/var/log/mail.log`, readable by the `adm` group. Either:

- copy `deploy/rsyslog/mailinois.conf` to `/etc/rsyslog.d/` and set `target` to Mailinois ([Sending logs](../../docs/sending-logs/#rsyslog-on-a-postfix-host)), or
- run the agent with `-file /var/log/mail.log` ([Installing](../../docs/install/#the-agent-on-a-postfix-host)), when Mailinois is on another network.

[Postfix on Debian and Ubuntu](../../setups/postfix-debian-ubuntu/) walks through both, with Mailinois on the same machine or elsewhere.

## Debian 12 and later without rsyslog

[From Debian 12, rsyslog is no longer installed by default](https://www.debian.org/releases/bookworm/amd64/release-notes/ch-information.en.html#changes-to-system-logging), and Postfix's lines go to the systemd journal only. `ls /var/log/mail.log` tells you which kind of host you have. Two ways out:

- **Install rsyslog.** `sudo apt install rsyslog` brings back `/var/log/mail.log`, and everything in the section above applies.
- **Postfix's own log file.** Since Postfix 3.4, [`maillog_file`](https://www.postfix.org/MAILLOG_README.html) makes Postfix write its log itself, through its `postlogd` service, and the agent follows that file. Postfix then stops logging to the journal. Postfix creates the file as mode 600 for root, and only Postfix 3.9 and later can make it readable by others (`maillog_file_permissions`). Debian 13 ships 3.10. Debian 12 ships 3.7 and Ubuntu 24.04 3.8, so there the agent has to run as root, and rsyslog is the simpler choice.

On Debian 13:

```sh
sudo postconf 'maillog_file = /var/log/postfix.log' 'maillog_file_permissions = 0644'
sudo postfix reload
```

`0644` lets every local user read the addresses in it. Rotate it with `postfix logrotate`, which renames the file. The agent keeps reading a renamed file for a while, and then picks up the new one.

The file's timestamps are `Oct 05 10:00:00`, zero-padded where syslog writes `Oct  5`. Mailinois reads both. That is all we have checked for this route: the parser, not a Debian host running it.

## RHEL, Rocky and Alma

rsyslog writes Postfix's lines to `/var/log/maillog`, readable by root only. The rsyslog file works as it is. The agent's systemd unit joins `adm`, which can't read `maillog`, so give it a group that can (`systemctl edit mailinois-agent`), or use the rsyslog route. Not run against Mailinois.

## iRedMail, Mail-in-a-Box and Modoboa

All three install Postfix on an ordinary Linux host and log through the system's rsyslog, so the instructions for the host's OS apply.

- [iRedMail](https://github.com/iredmail/iRedMail) uses rsyslog, and logs to `/var/log/mail.log` on Debian and Ubuntu and `/var/log/maillog` on RHEL. On Debian and Ubuntu it adds a `/var/log/maillog` link, so either name works there (`conf/postfix`, `functions/postfix.sh` in its source).
- [Mail-in-a-Box](https://github.com/mail-in-a-box/mailinabox) supports Ubuntu 22.04 only, installs rsyslog, and reads `/var/log/mail.log` itself for its status pages (`management/mail_log.py`).
- [Modoboa's installer](https://github.com/modoboa/modoboa-installer) points Modoboa's own statistics at whichever of `/var/log/maillog` and `/var/log/mail.log` exists.

Mailinois's rsyslog file is a new file in `/etc/rsyslog.d/`, so it doesn't change theirs. We haven't checked whether their upgrade scripts leave it alone. None of the three has been run against Mailinois.

## Not Postfix

Mailinois reads Postfix and OpenDKIM lines only. Exim, Sendmail, OpenSMTPD, Haraka and Stalwart write different logs, and Mailinois ignores them. For Exim, `eximstats` ships with Exim and summarises its log.
