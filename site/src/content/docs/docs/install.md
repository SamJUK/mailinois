---
title: Installing
description: Run Mailinois as a static binary under systemd, as a container with Docker or Compose, or on Kubernetes, and upgrade it.
---

Mailinois is one static binary with SQLite built in, so every way of running it is the same program with a config file. Pick whichever matches how you run everything else:

| How | Good for |
| --- | --- |
| [Binary and systemd](#binary-and-systemd) | A VM or a Postfix host, no container runtime. |
| [Docker](#docker) or [Compose](#docker-compose) | Hosts that already run containers. |
| [Kubernetes](../../docs/kubernetes/) | Clusters, with the agent as a sidecar next to Postfix. |
| [From source](#from-source) | Anything with Go 1.27. |

## Binary and systemd

Each release carries binaries for Linux (`amd64` and `arm64`) and macOS (`arm64`), and a `SHA256SUMS` file. They are pure Go with SQLite built in, so there is nothing else to install.

```sh
version=v0.1.0
arch=amd64   # or arm64
base=https://github.com/SamJUK/mailinois/releases/download/$version
curl -fLO $base/mailinois-linux-$arch
curl -fLO $base/SHA256SUMS
sha256sum --check --ignore-missing SHA256SUMS
sudo install -m 755 mailinois-linux-$arch /usr/local/bin/mailinois
```

Then the config. Start from `deploy/config/mailinois.example.yaml` in the repository, keep what you need, and check it before starting anything:

```sh
sudo mkdir -p /etc/mailinois
sudo curl -fLo /etc/mailinois/mailinois.yaml https://raw.githubusercontent.com/SamJUK/mailinois/$version/deploy/config/mailinois.example.yaml
sudoedit /etc/mailinois/mailinois.yaml
mailinois check -config /etc/mailinois/mailinois.yaml
```

Secrets stay out of the config. Put them in `/etc/mailinois/mailinois.env`, mode 600, and refer to them in the YAML as `${NAME}`:

```sh
sudo install -m 600 /dev/null /etc/mailinois/mailinois.env
sudoedit /etc/mailinois/mailinois.env   # MAILINOIS_PASSWORD=…, SLACK_WEBHOOK=…
```

Install the unit from `deploy/systemd/` and start it:

```sh
sudo curl -fLo /etc/systemd/system/mailinois.service https://raw.githubusercontent.com/SamJUK/mailinois/$version/deploy/systemd/mailinois.service
sudo systemctl daemon-reload
sudo systemctl enable --now mailinois
curl -s http://127.0.0.1:7480/healthz
```

What the unit does:

- It runs as a throwaway user (`DynamicUser=yes`) that owns nothing but `/var/lib/mailinois`, where SQLite lives. The default `sqlite:mailinois.db` is relative, so it lands there.
- It reads `/etc/mailinois/mailinois.env` if it exists, so `MAILINOIS_PASSWORD` and the `${NAMES}` in the config come from a file only root can read.
- Everything else is locked down: a read-only system, no home directories, no capabilities, IPv4, IPv6 and Unix sockets only.

We ran both units on Debian 13, with systemd as PID 1 in a container. The server started and made its database in `/var/lib/private/mailinois`, and `systemd-analyze security` rated each unit 1.7, "OK".

### Importing old logs under systemd

`mailinois import` writes to the database itself, and under this unit the database belongs to the unit's own user, so don't run `import` as root against it. Send old logs through the API instead, as plain lines, with a token that has the `ingest` scope. A request takes up to 8 MiB, so split big files:

```sh
split -l 20000 /var/log/mail.log.1 /tmp/mail-part-
for part in /tmp/mail-part-*; do
  curl -sS --data-binary @"$part" -H "Content-Type: text/plain" \
    -H "Authorization: Bearer $INGEST_TOKEN" http://127.0.0.1:7480/api/v1/ingest
done
rm /tmp/mail-part-*
```

Lines already stored are skipped, so sending a file twice changes nothing.

### The agent on a Postfix host

When Mailinois runs somewhere else, the agent follows the mail log on each Postfix host and ships it over HTTPS ([Sending logs](../../docs/sending-logs/#the-agent-for-servers-on-other-networks) has the details). Install the same binary, then `mailinois-agent.service`, and give it its settings:

```sh
sudo curl -fLo /etc/systemd/system/mailinois-agent.service https://raw.githubusercontent.com/SamJUK/mailinois/$version/deploy/systemd/mailinois-agent.service
sudo install -m 600 /dev/null /etc/mailinois/agent.env
sudoedit /etc/mailinois/agent.env
sudo systemctl daemon-reload
sudo systemctl enable --now mailinois-agent
```

```sh title="/etc/mailinois/agent.env"
MAILINOIS_SERVER=https://mailinois.example.com
MAILINOIS_TOKEN=mlns_…                          # a token with the ingest scope
MAILINOIS_AGENT_ARGS=-file /var/log/mail.log    # or -syslog 127.0.0.1:5514, and -milter 127.0.0.1:8892
```

The unit joins the `adm` group, which can read `/var/log/mail.log` on Debian and Ubuntu. In the same Debian 13 test, the agent read a `root:adm`, mode 640 log through it and its lines arrived; with the group taken away it logged `permission denied` and kept retrying. Elsewhere, such as RHEL's root-only `/var/log/maillog`, override it:

```sh
sudo systemctl edit mailinois-agent   # [Service] SupplementaryGroups=… for a group that can read the log
```

The agent starts at the end of the file, so lines written while the agent itself is stopped (or can't read the log) are never sent. In that test, a message logged while the group was missing didn't arrive once the group was back. Send that stretch of the log through [the API](#importing-old-logs-under-systemd); the lines Mailinois already has are skipped.

## Docker

The image (`ghcr.io/samjuk/mailinois`, for `amd64` and `arm64`) is distroless, runs as a non-root user and keeps SQLite on `/data`:

```sh
docker run -d --name mailinois --restart unless-stopped \
  -p 7480:7480 -p 5514:5514 -p 5514:5514/udp \
  -v mailinois-data:/data \
  -v "$PWD/mailinois.yaml":/etc/mailinois/mailinois.yaml:ro \
  --env-file mailinois.env \
  ghcr.io/samjuk/mailinois:v0.1.0 serve -config /etc/mailinois/mailinois.yaml
```

Pin a version rather than `latest`, so a restart never upgrades it behind your back.

- The image sets `MAILINOIS_DATABASE_URL=sqlite:/data/mailinois.db`, and environment variables win over the config file. For PostgreSQL, set `MAILINOIS_DATABASE_URL` to its URL rather than `database.url`.
- The image has no health check of its own. `mailinois healthcheck` asks `/healthz` and fails unless it answers 200: use it as the Compose file below does, with `-url` if the dashboard listens anywhere but `127.0.0.1:7480` inside the container.

## Docker Compose

```yaml title="compose.yaml"
services:
  mailinois:
    image: ghcr.io/samjuk/mailinois:v0.1.0
    restart: unless-stopped
    command: ["serve", "-config", "/etc/mailinois/mailinois.yaml"]
    env_file: mailinois.env
    ports:
      - "7480:7480"       # dashboard, REST API, /metrics
      - "5514:5514"       # syslog, TCP
      - "5514:5514/udp"   # syslog, UDP
    volumes:
      - mailinois-data:/data
      - ./mailinois.yaml:/etc/mailinois/mailinois.yaml:ro
    healthcheck:
      test: ["CMD", "/mailinois", "healthcheck"]
      interval: 30s

volumes:
  mailinois-data:
```

Publish 5514 only where your Postfix hosts can reach it and nobody else can: syslog has no authentication. If Postfix runs in the same Compose project, leave the syslog ports unpublished and point its rsyslog at `mailinois` on the project network instead.

## From source

```sh
go install github.com/SamJUK/mailinois/cmd/mailinois@latest
```

Or `make build` in a checkout, which writes `bin/mailinois`. It needs Go 1.27, and no C compiler.

## Upgrading

Dashboard CSS, JavaScript and logo URLs include a hash of their content. Browsers and caching proxies fetch the matching assets when an upgrade changes them, without a cache purge.

Replace the binary or the image tag and restart. Mailinois updates its database schema when it starts, so there is no separate migration step.

Back up first. For SQLite, stop Mailinois and copy the database file and its `-wal` file, or take a copy while it runs with `sqlite3 mailinois.db ".backup backup.db"`. For PostgreSQL, `pg_dump` as usual.
