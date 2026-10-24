---
title: A Postfix log dashboard for mailcow
description: mailcow's Postfix logs through syslog-ng, not rsyslog. A small hook copies its lines to Mailinois for per-message history, deferral reasons and alerts, and a milter captures headers without ever holding up mail.
template: splash
---

mailcow already shows Postfix's log. Its UI reads recent lines from Redis, which is enough to see whether something just went out. It is a list of lines, though. A message that was deferred three times and then delivered is spread over a dozen of them, among everyone else's, and older lines drop off the end.

Mailinois keeps each message as one record (its recipients, every attempt and the remote server's answers) for 30 days by default, groups deferrals by provider and reason, and alerts on bounce rates, blocklistings and failing DNS. If mailcow's log view already answers your questions, you don't need it.

## How does mailcow log Postfix?

The `postfix-mailcow` container has no rsyslog. It runs syslog-ng, and its [syslog-ng.conf](https://github.com/mailcow/mailcow-dockerized/blob/master/data/Dockerfiles/postfix/syslog-ng.conf) does two things with each line: it writes it to the container's output (what `docker compose logs postfix-mailcow` shows), and it pushes mail lines to Redis for mailcow's UI and its fail2ban-style blocking.

So the rsyslog file the other setups use does nothing here. Two things make another way in easy:

- The container's [entrypoint](https://github.com/mailcow/mailcow-dockerized/blob/master/data/Dockerfiles/postfix/docker-entrypoint.sh) runs every executable file in `data/hooks/postfix/` before the daemons start.
- syslog-ng can send to more than one place. A hook can add a destination for Mailinois to the container's syslog-ng.conf, next to mailcow's own, without changing the image.

## Add Mailinois to the stack

Run Mailinois as one more service on `mailcow-network`, in the `docker-compose.override.yml` beside mailcow's `docker-compose.yml`:

```yaml title="docker-compose.override.yml"
services:
  mailinois-mailcow:
    image: ghcr.io/samjuk/mailinois:v0.1.0
    restart: always
    command: ["serve", "-config", "/etc/mailinois.yaml"]
    environment:
      - MAILINOIS_PASSWORD=${MAILINOIS_PASSWORD}
    volumes:
      - ./data/conf/mailinois/mailinois.yaml:/etc/mailinois.yaml:ro
      - mailinois-vol-1:/data
    ports:
      - "127.0.0.1:7480:7480"   # the dashboard, on this host only
    networks:
      mailcow-network:
        aliases:
          - mailinois

volumes:
  mailinois-vol-1:
```

Add `MAILINOIS_PASSWORD=…` to `mailcow.conf`, and you sign in as `admin`. mailcow's `.env` is a link to `mailcow.conf`, so Compose fills in `${MAILINOIS_PASSWORD}` from it. Then the config:

```yaml title="data/conf/mailinois/mailinois.yaml"
listen:
  http: ":7480"
  syslog: ":5514"   # on mailcow-network only: nothing publishes it
  milter: ":8892"
capture:
  mode: headers
checks:
  sending_ips: [203.0.113.10]   # your mailcow's public address
  domains: [example.com]        # the domains you send as
```

`docker compose up -d mailinois-mailcow` starts it. The dashboard is on the host's loopback. Reach it by SSH tunnel, or put your own TLS proxy in front ([Authentication](../../docs/authentication/) covers sign-in limits behind a proxy).

## The hook

```sh title="data/hooks/postfix/mailinois.sh"
#!/bin/sh
# Copies Postfix's lines to Mailinois, on top of mailcow's own log (the container's output and Redis).
# mailcow runs this before syslog-ng starts, on every container start: the grep stops a restart adding it twice.
conf=/etc/syslog-ng/syslog-ng.conf
grep -q d_mailinois "$conf" && exit 0
cat >> "$conf" <<'EOF'
rewrite r_mailinois_host { set("mailcow" value("HOST")); };
destination d_mailinois { network("mailinois" port(5514) transport("tcp") flags(syslog-protocol)); };
log { source(s_src); filter(f_mail); rewrite(r_mailinois_host); destination(d_mailinois); };
EOF
```

```sh
chmod +x data/hooks/postfix/mailinois.sh
docker compose restart postfix-mailcow
```

What each line does:

- **The `grep`.** A restart reuses the container, and the hook has already added its lines. Without the check, the second start would define `d_mailinois` twice, syslog-ng would refuse its config, and mailcow would lose its log too.
- **The `rewrite`.** syslog-ng names each line after the container's hostname, which is its ID and changes when the container is recreated. This gives Mailinois a fixed server name, `mailcow`, in Mailinois's copy only.
- **The `destination`.** RFC 5424 over TCP to the `mailinois` alias on `mailcow-network`, with full timestamps and time zones.
- **The `log` path.** mailcow's own source (`s_src`) and its mail filter (`f_mail`), so Mailinois gets what mailcow's UI gets.

What we ran: this hook inside `ghcr.io/mailcow/postfix:3.10.12-1`, the image mailcow's `docker-compose.yml` uses at the time of writing (syslog-ng 4.8.1). We pointed it at a local Mailinois, without the rest of mailcow:

- Run twice, it added the destination once, and `syslog-ng --syntax-only` passed.
- A Postfix-shaped message logged through `/dev/log` (client, Message-ID, sender, a delivery, removal) arrived in Mailinois as one sent message, under server `mailcow`.
- mailcow's own copy on the container's output still carried the container's hostname, so the rewrite stayed in Mailinois's path.

A whole mailcow stack has not been run against Mailinois.

### Gotchas

- **`REDIS_SLAVEOF_IP`.** When it is set, the entrypoint copies `syslog-ng-redis_slave.conf` over syslog-ng.conf after the hooks have run, and the hook's lines are lost. Append the same block to that file too, if you use it.
- **An outage.** The new log path doesn't ask syslog-ng for flow control, so a Mailinois outage doesn't slow Postfix's logging. We tested that on 25th October 2026: the hook in `ghcr.io/mailcow/postfix:3.10.12-1` (syslog-ng 4.8.1) beside Mailinois, without Redis or the rest of mailcow. With Mailinois stopped, syslog-ng logged `Syslog connection closed … time_reopen='60'` and carried on. 200 lines through `/dev/log` took 84 to 93 ms, against 92 to 113 ms with Mailinois up, and the container's output had all 600 lines of three runs.
- **What an outage keeps.** syslog-ng holds the lines in memory and reconnects a minute after Mailinois is back. In that test all 120 messages logged during the outage arrived. A restart of `postfix-mailcow` while Mailinois is down loses them (40 of 40 in the same test), and [Old lines](#old-lines) brings them back from the container's output.

## Capturing headers

mailcow's `main.cf` runs Rspamd as a milter, with `milter_default_action = tempfail`. mailcow's [documented place for changes](https://docs.mailcow.email/manual-guides/Postfix/u_e-postfix-extra_cf/) is `data/conf/postfix/extra.cf`. Add Mailinois after Rspamd, with its own default:

```txt title="data/conf/postfix/extra.cf"
smtpd_milters = inet:rspamd:9900, { inet:mailinois:8892, default_action=accept }
non_smtpd_milters = inet:rspamd:9900, { inet:mailinois:8892, default_action=accept }
```

```sh
docker compose restart postfix-mailcow
```

**Don't leave out `default_action=accept`.** With mailcow's global `tempfail`, a plain `inet:mailinois:8892` makes Postfix refuse mail whenever Mailinois is down. We reproduced exactly that in docker-mailserver with the global setting at `tempfail` (`milter-reject: … 4.7.1 Service unavailable - try again later`). With the braces form, the same messages were delivered while Mailinois was stopped. That ran on Postfix 3.10.13; mailcow's image runs 3.10.12. The details are on [Postfix in Docker](../../setups/postfix-docker/#capturing-headers-in-docker-mailserver).

Rspamd stays first in the list, so Postfix asks it before Mailinois's milter. [Capturing headers and bodies](../../docs/capture/) explains what is stored, and why bodies are off unless you ask.

## Old lines

`docker compose logs` holds whatever Docker has kept since the container was created. Load it once:

```sh
docker compose logs --no-log-prefix postfix-mailcow \
  | docker compose exec -T mailinois-mailcow /mailinois import -server mailcow -
```

`--no-log-prefix` matters: without it each line starts with the container's name, and `import` skips them. The lines themselves are syslog-ng's classic format (`Oct 19 10:45:09 6a4cda985b5c postfix/qmgr[200]: …`). In our test, `import` read all five of five. `-server mailcow` matches the name the hook gives live lines, and `import` skips lines it already has.

## What you see then

The Servers page lists `mailcow`, and every message it handles appears under Messages with its recipients and attempts. [Use cases](../../use-cases/) goes through deferrals, bounces, blocklists and the rest, with the searches that answer each.
