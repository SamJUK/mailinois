# Contributing

Fixes, docs and new log formats are welcome. For anything bigger, open an issue first so we can agree on the
shape before you write it.

Security problems go through [SECURITY.md](SECURITY.md), not issues.

## Setting up

You need Go (the version in `go.mod`), Node 22.12+ for the site, and Docker for the local preview and the
PostgreSQL tests.

```sh
make hooks           # gofmt and vet before each commit, Conventional Commits on the message
make test            # vet and every test, SQLite
make test-postgres   # the same on PostgreSQL (starts the compose postgres)
make vuln            # govulncheck
make up              # local preview: demo data plus a real Postfix, on http://localhost:7480
cd site && npm ci && npm test && npm run build
```

## What a change needs

- **Docs move with the code, in the same pull request.** A new flag, config key, endpoint, default or file in
  `deploy/` updates the docs page, the README, `deploy/config/mailinois.example.yaml` and any site page that mentions it.
- **Mailinois is never in the delivery path.** Nothing it does may stop or slow Postfix. The milter fails open.
- **Both databases.** SQL runs on SQLite and PostgreSQL: `?` placeholders, times as BIGINT microseconds. Schema
  changes are a new numbered file in `internal/store/schema/`; released ones are never edited.
- **Tests prove something.** Every negative test has a positive control in the same run, and security checks
  assert the status code.
- **Untrusted text stays text.** Log lines and captured mail are escaped, and the CSP stays.
- **Small.** Standard library first, no new dependency for what a few lines do.
- **Made-up data only.** Tests, the demo and docs use reserved names (`example.com`, `.example`, `.test`) and
  documentation IP ranges (`192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24`), never real addresses.
- Docs are in British English, without em dashes.

## Commits and pull requests

Commits follow [Conventional Commits](https://www.conventionalcommits.org/): `fix(store): count expired mail
once`. Keep the pull request description as short as the change allows, and say what you tested.

## Licence

Mailinois is [AGPL-3.0](LICENSE). By opening a pull request you agree your contribution is licensed the same way.
