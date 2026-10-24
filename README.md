<p align="center"><img src="internal/web/static/logo.svg" width="120" alt=""></p>

# Mailinois

Mailinois watches Postfix. It reads mail logs over syslog, follows every message by queue ID from the
client that handed it over to each recipient's final answer, and serves a dashboard of deliveries,
deferrals, bounces, rejections, the queue, providers, blocklists and DNS, for any number of Postfix
servers and sending domains.

Postfix stays the mail server. Mailinois never sits in the delivery path: syslog is forwarded with its own
queue, and the capture milter lets mail through if Mailinois is down or slow.

It's one Go binary with SQLite built in (PostgreSQL optional), configured by one YAML file.

Docs, an interactive demo and an example report: <https://samjuk.github.io/mailinois/>

## Try it locally

```sh
docker compose up -d --build
make mail TO=someone@example.com
```

- Dashboard: <http://localhost:7480>
- Mailpit, where the local Postfix delivers: <http://localhost:8325>

The preview runs Mailinois in demo mode (two weeks of made-up traffic from two relays, with more every few
seconds) next to a real Postfix that forwards its log to Mailinois, hands each message to its capture
milter, and relays everything to Mailpit. Nothing leaves your machine. The real messages show up as server
`postfix-local`, with their headers and bodies.

The docs and marketing site live in [`site/`](site/): `make site`, then <http://localhost:4321/mailinois/>.

## What it does

- **Every message's story:** the client, the queue, each delivery attempt with the remote server's answer,
  and the outcome, with captured headers and bodies when capture is on (`.eml` download, HTML shown in a
  sandbox).
- **Why mail is waiting:** the queue, deferrals grouped by provider and status code, and each provider
  (Google, Microsoft, Yahoo, Apple, other) against the previous period.
- **Many servers, many sites:** servers appear when their first line arrives; sites group sending domains or exact sender addresses
  and servers so each can be looked at alone or all together, and users and tokens can be limited to some.
- **Search:** `from:store-a.example provider:microsoft status:deferred dsn:4.7.650 after:2d`, with
  `to:`, `domain:`, `server:`, `site:`, `qid:`, `msgid:`, `subject:`, `client:`, `relay:`, `has:capture`,
  `before:` and `-` to leave things out.
- **Checks:** SPF for each sending domain against each sending IP, the DKIM key for every selector seen
  signing, DMARC, reverse DNS, and blocklists, for IPv4 and IPv6.
- **Alerts:** bounce and deferral rates, rejections, queue depth and age, blocklistings, silent servers and
  failing DNS, to JSON (signed), Slack or Discord webhooks.
- **API:** a read-only REST API over `/api/v1` with bearer tokens, plus `POST /api/v1/ingest` for agents
  and log shippers, and Prometheus metrics at `/metrics`.

The Domains page has one expandable summary row per domain; full DNS records stay in its details. Recent failures sit below the outcomes chart, and the dashboard uses one fixed spacing with a light/dark theme toggle. SMTP transactions cancelled before acceptance stay in the message history as `cancelled`, outside the queue and Received count.

Dashboard assets use content hashes in their URLs so browsers and caching proxies fetch matching CSS and JavaScript after an upgrade.

## One-off reports

The marketing site also has an [interactive browser demo](site/src/pages/demo.astro) at `/demo/`: simulated traffic, working app views, filters, message timelines and DNS scenarios, entirely in JavaScript. It uses the app's stylesheet and makes no API, SMTP or DNS calls. See [Browser demo](site/src/content/docs/docs/browser-demo.md).

No server, no config: read the logs you have and get one file.

```sh
mailinois report -q "to:customer@example.com" /var/log/mail.log.1 /var/log/mail.log
journalctl SYSLOG_FACILITY=2 --since today -o short --no-pager | mailinois report -o today.md -
```

HTML (interactive, offline, prints to PDF), Markdown or JSON, with the summary, why mail waited, what was still
queued and every message's story. See [One-off reports](site/src/content/docs/docs/report.md).

## Running it

| How | |
|---|---|
| Binary and systemd | Static binaries for Linux (amd64, arm64) and macOS (arm64) on each release; units in [`deploy/systemd/`](deploy/systemd/) |
| Docker or Compose | `ghcr.io/samjuk/mailinois:<version>` (amd64, arm64); [`deploy/compose/`](deploy/compose/) |
| Kubernetes | [`deploy/kubernetes/`](deploy/kubernetes/): the server, and Postfix with the agent as a sidecar |

[Installing](site/src/content/docs/docs/install.md) has the steps, and [Sizing](site/src/content/docs/docs/sizing.md)
what one instance handles.

## Getting logs in

| Where Postfix runs | How |
|---|---|
| Same network | rsyslog: [`deploy/rsyslog/mailinois.conf`](deploy/rsyslog/mailinois.conf) in `/etc/rsyslog.d/` (or mounted into `boky/postfix`) |
| journald only (Debian 12+) | install rsyslog, or Postfix's own `maillog_file` and the agent |
| Anywhere else | `mailinois agent -server https://mailinois.example.com -token … -file /var/log/mail.log`, over HTTPS with a spool on disk |
| Kubernetes | the agent as a sidecar next to Postfix, or the cluster's log shipper |
| Vector, Fluent Bit | their HTTP output to `POST /api/v1/ingest` (`text/plain` or `application/x-ndjson`) with an ingest token |
| Old logs | `mailinois import /var/log/mail.log`, or `docker logs … \| mailinois import -server name -` |

To capture headers or bodies, point Postfix at the milter (`listen.milter`) with
`{ inet:mailinois:8892, default_action=accept }`, after OpenDKIM so the signature is captured too (not the global
`milter_default_action`, which would change every milter's failure policy). Across networks, run the
milter in the agent (`-milter 127.0.0.1:8892 -capture full`) so SMTP never waits on the internet.

## Configuration

Everything is in [`deploy/config/mailinois.example.yaml`](deploy/config/mailinois.example.yaml), with the defaults. The main choices:

- **Database:** `sqlite:<path>` (default) or a `postgres://` URL, own database or a `search_path` schema.
- **Auth:** `off` (behind your own network or proxy), `password` (users with bcrypt hashes from
  `mailinois hash-password`; signed session cookies), `oidc` (single sign-on through Google Workspace, Entra ID,
  Okta, Keycloak, authentik or any OpenID Connect provider, limited by domain or group, groups mapped to sites)
  or `proxy` (trust a header such as `Remote-User` from listed addresses). API tokens (`mailinois token`) work
  in every mode, with scopes `read` and `ingest`.
- **Retention:** messages 30 days, captures 7 days.

Environment variables override the basics: `MAILINOIS_CONFIG`, `MAILINOIS_DATABASE_URL`, `MAILINOIS_HTTP`,
`MAILINOIS_SYSLOG`, `MAILINOIS_MILTER`, `MAILINOIS_SENDING_IPS`, `MAILINOIS_DEMO`, and
`MAILINOIS_PASSWORD` (a shortcut: password mode with one user, `admin`).

## Privacy and security

- Stored: addresses, client names and IPs, Message-IDs, SMTP responses and the raw log lines. Bodies only
  with capture on, kept for their own (shorter) retention.
- Syslog and the milter have no authentication: keep their ports on a private network, and use the agent
  across the internet.
- The dashboard is read-only. Remote text is escaped, captured HTML is served to a sandboxed frame with no
  scripts or remote content, and every page sends a strict Content Security Policy.

## Development

```sh
make test            # vet and every test, on SQLite
make test-postgres   # the same against PostgreSQL in compose
make vuln            # govulncheck
make hooks           # gofmt, vet and Conventional Commits checks before each commit
```

The correlation tests read [`internal/store/testdata/scenarios.log`](internal/store/testdata/scenarios.log):
deliveries, retries, bounces and their notifications, rejections before and after queueing, multiple
recipients, expiry, `postsuper -d`, local pickup, reused queue IDs, lines out of order, and two servers
sharing a queue ID. They run against both databases.

## Contributing

Contributions are welcome: see [CONTRIBUTING.md](CONTRIBUTING.md), and [SECURITY.md](SECURITY.md) for reporting a
vulnerability privately.

## Licence

[AGPL-3.0](LICENSE). Run it, change it and host it; if you offer a changed version to others over a network,
publish your changes under the same licence.
