---
title: Quick start
description: Run Mailinois with Docker, Docker Compose or the binary, or try the local preview with made-up traffic.
---

Want to explore first? The [interactive browser demo](../../demo/) runs with made-up traffic, without installing Mailinois or Postfix.

:::tip[Only want to look at a log?]
`mailinois report /var/log/mail.log` writes one HTML report from the log you already have, with nothing to run afterwards. [One-off reports](../report/).
:::

## Docker

```sh
docker run -d --name mailinois \
  -p 7480:7480 -p 5514:5514 -p 5514:5514/udp \
  -v mailinois-data:/data \
  -e MAILINOIS_DATABASE_URL=sqlite:/data/mailinois.db \
  ghcr.io/samjuk/mailinois
```

Port 7480 serves the dashboard, the REST API and `/metrics`. Port 5514 takes syslog from your Postfix hosts over TCP and UDP. The SQLite database sits on the `mailinois-data` volume, so it survives a new image.

Open http://localhost:7480.

There is no login until you set one up. For a single login, add `-e MAILINOIS_PASSWORD=…` and sign in as `admin`. [Authentication](../authentication/) covers the rest.

## Docker Compose

```yaml title="compose.yaml"
services:
  mailinois:
    image: ghcr.io/samjuk/mailinois
    restart: unless-stopped
    environment:
      MAILINOIS_DATABASE_URL: sqlite:/data/mailinois.db
      MAILINOIS_PASSWORD_FILE: /run/secrets/mailinois_password
    secrets: [mailinois_password]
    ports:
      - "7480:7480"       # dashboard, REST API, /metrics
      - "5514:5514"       # syslog, TCP
      - "5514:5514/udp"   # syslog, UDP
    volumes:
      - mailinois-data:/data

secrets:
  mailinois_password:
    file: ./mailinois_password.txt

volumes:
  mailinois-data:
```

Put the password in `mailinois_password.txt` next to the file, then `docker compose up -d`. The user name is `admin`.

For anything beyond the [environment variables](../configuration/#environment-variables), mount a config file and point `MAILINOIS_CONFIG` at it:

```yaml
    environment:
      MAILINOIS_CONFIG: /etc/mailinois/mailinois.yaml
    volumes:
      - mailinois-data:/data
      - ./mailinois.yaml:/etc/mailinois/mailinois.yaml:ro
```

## The binary

Download it from a release (Linux `amd64` and `arm64`, macOS `arm64`; [Installing](../install/#binary-and-systemd) has the commands and a systemd unit), or build it with Go from a checkout (`make build` writes `bin/mailinois`), then run it:

```sh
mailinois serve
```

With no config file it runs on the defaults: the dashboard on port 7480, syslog on port 5514, and SQLite in `mailinois.db` in the working directory.

For anything more, copy `deploy/config/mailinois.example.yaml` from the repository, keep the parts you need and point Mailinois at it:

```sh
mailinois serve -config mailinois.yaml
```

The example file includes sample sites, users, a webhook and an alert. Remove the ones you don't want, or a firing alert will be posted to the placeholder Slack URL. [Configuration](../configuration/) explains every key.

## Send it some logs

On each Postfix host, copy `deploy/rsyslog/mailinois.conf` from the repository into `/etc/rsyslog.d/`, set `target` to Mailinois's address and restart rsyslog:

```sh
sudo cp mailinois.conf /etc/rsyslog.d/
sudo systemctl restart rsyslog
```

Each server appears under its hostname when its first line arrives. [Sending logs](../sending-logs/) covers containers, the agent for servers on other networks, and importing old logs.

## Try the local preview

The repository has a compose file that runs everything on your machine, with made-up traffic to look at. In a checkout:

```sh
docker compose up -d --build
```

- Dashboard: http://localhost:7480
- Mailpit, where the local Postfix delivers: http://localhost:8325

Mailinois runs in demo mode on SQLite: two weeks of made-up traffic from two relays (including a Microsoft reputation incident three days ago and a dead mail server), with a new message every few seconds.

Alongside it runs a real Postfix (`boky/postfix`) that forwards its log to Mailinois, hands every message to Mailinois's capture milter, and relays everything to Mailpit, so nothing leaves your machine. Send it a real message:

```sh
make mail TO=someone@example.com
```

It shows up on the Messages page under the server `postfix-local`, with its headers and body captured, and in Mailpit. `docker compose down` stops it all.
