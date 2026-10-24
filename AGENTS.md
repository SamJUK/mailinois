# Mailinois

Postfix observability: reads Postfix's logs, follows each message by queue ID, serves a dashboard, REST API,
alerts and DNS/blocklist checks. One Go binary, SQLite by default (PostgreSQL optional), one YAML config.
Public, AGPL-3.0 (contributions welcome); domain undecided. A hosted version later is this same app run per
tenant, not a multi-tenant rewrite.

## Layout

- `cmd/mailinois`: every command (`serve`, `agent`, `import`, `report`, `check`, `token`, `hash-password`, `healthcheck`).
  `internal/report` builds the one-off report (HTML with inline script and CSP hashes, Markdown, JSON).
- `internal/postfix` parses lines into events; `internal/syslog` reads every log format; `internal/ingest`
  batches them; `internal/store` correlates and stores (migrations in `store/schema/`); `internal/web` is the
  dashboard and API; `capture` is the milter; `agent`, `alerts`, `dnscheck`, `dnsbl`, `auth`, `config`, `demo`.
- `deploy/`: the example config, and rsyslog, Compose, systemd and Kubernetes files people copy.
- `site/`: docs (`src/content/docs/docs/`) and marketing pages (`src/pages/index.astro`, `use-cases/`,
  `setups/`), Astro Starlight. Local only for now: `make site`.

## Commands

```sh
make test            # vet and every test, SQLite
make test-postgres   # the same on PostgreSQL (starts the compose postgres)
make vuln            # govulncheck
make hooks           # gofmt, vet and Conventional Commits before each commit
make up              # local preview: demo data plus a real Postfix
cd site && npm run build
go test ./internal/store -run '^$' -bench . -benchtime 1x   # the numbers in docs/sizing
go run ./tools/loadgen -rate 20000 -duration 30s   # syslog load at a set rate, for the ceilings in docs/sizing
```

Sam keeps the local preview and the docs site running while we work. After a change to the app, rebuild it with
`docker compose up -d --build mailinois` (dashboard on http://localhost:7480). The docs server is a daemon that
reloads by itself (http://localhost:4321/mailinois/; `npx astro dev status` in `site/`, `npx astro dev` to start it again).

## Rules

1. **Docs and the site move with the code, in the same commit.** A change to behaviour, a flag, a config key,
   an endpoint, a default or a file in `deploy/` updates every place that describes it:
   - the docs page;
   - the README;
   - `deploy/config/mailinois.example.yaml` and its copy in `docs/configuration.md`;
   - any use-case, setup or landing page that mentions it.

   New pages go in the sidebar in `site/astro.config.mjs`. `npm run build` passes before committing.
2. **Never in the delivery path.** The milter fails open (`{ inet:…:8892, default_action=accept }` on Mailinois's
   milter, not the global, which images such as boky/postfix set only sometimes), rsyslog forwards with its own
   queue. Anything that can stop or slow Postfix is a bug.
3. **Unauthenticated listeners stay private.** Syslog and the milter have no auth: loopback or a trusted
   network only. Anything across networks goes over HTTPS with a token. Ingest always needs an `ingest`
   token, even with auth off.
4. **Both databases.** SQL runs on SQLite and PostgreSQL: `?` placeholders, times as BIGINT microseconds.
   Schema changes are a new numbered file in `internal/store/schema/`; never edit one already released. Run
   `make test-postgres` for store changes.
5. **Tests prove something.** Every negative test has a positive control in the same run; security checks
   assert the status code. Break a new guard on purpose and watch its test fail.
6. **Untrusted text stays text.** Log lines and captured mail are escaped; captured HTML renders only in the
   sandboxed frame; keep the CSP.
7. **No secrets in config.** `${NAME}` from the environment or an env file.
8. **Claims are things that happened.**
   - Docs and marketing say only what was run or what the code does, and say what was tested and what wasn't.
   - The one real deployment is the copilot-seo.com relay (since 18th October 2026).
   - Screenshots are demo data and say so.
   - Tests, the demo and docs use made-up data only: `.example`/`.test` names and documentation IP ranges, never
     real addresses, hostnames, IPs or DKIM selectors (including the copilot-seo.com relay's).
   - Performance numbers come from `bench_test.go` and name the machine.
9. **Writing.** British spelling, no em dashes, worded dates ("19th October 2026"), plain words. Credit and link
   other tools (pflogsumm, postfix_exporter, Loki, Graylog) and say when one is the better fit.
10. **Small.** Standard library first, no new dependency for what a few lines do, one binary.

## Releasing

Conventional Commits, no AI attribution. A published GitHub release `vX.Y.Z` builds the image for amd64 and arm64
(`ghcr.io/samjuk/mailinois:vX.Y.Z`) and attaches Linux and macOS binaries with `SHA256SUMS`. Production (the
Swarm, `SamJUK/infra-swarm-platform`, `mailinois/`) pins the image by digest and deploys by hand from there.
