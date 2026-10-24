---
title: Configuration
description: Every key in mailinois.yaml, grouped as in the example file, and the environment variables that override them.
---

Mailinois reads one YAML file. Every key is optional, and with no file at all it runs on the defaults below. Point it at a file with `-config` or `MAILINOIS_CONFIG`:

```sh
mailinois serve -config /etc/mailinois/mailinois.yaml
MAILINOIS_CONFIG=/etc/mailinois/mailinois.yaml mailinois serve
```

Durations are Go durations: `90s`, `5m`, `6h`, `720h`. There is no day unit, so 30 days is `720h`.

`deploy/config/mailinois.example.yaml` in the repository has every key with a comment. The whole file is at the [end of this page](#the-example-file).

## listen

| Key | Default | |
| --- | --- | --- |
| `listen.http` | `":7480"` | Dashboard, REST API, `/metrics` and `/healthz`. |
| `listen.syslog` | `":5514"` | Syslog from Postfix hosts, over TCP and UDP. `""` turns it off. |
| `listen.milter` | `""` | Address Postfix's milter connects to, such as `":8892"`, to [capture headers or bodies](../capture/). `""` turns it off. |

## database

| Key | Default | |
| --- | --- | --- |
| `database.url` | `"sqlite:mailinois.db"` | `sqlite:<path>`, or a `postgres://` URL. To share a PostgreSQL database that already exists, give Mailinois a schema of its own with `?search_path=mailinois`. See [Databases](../databases/). |

## retention

| Key | Default | |
| --- | --- | --- |
| `retention.messages` | `720h` | How long to keep messages, their log lines, and blocklist and DNS check results. |
| `retention.captures` | `168h` | How long to keep captured headers and bodies, which are more sensitive. |

## Queue and time zone

| Key | Default | |
| --- | --- | --- |
| `queue_lifetime` | `120h` | Set it to match Postfix's `maximal_queue_lifetime`. A message whose `removed` line was lost leaves the queue view after this long. |
| `log_timezone` | `UTC` | The time zone of classic syslog timestamps, which carry none. An IANA name such as `Europe/London`. |

## auth

See [Authentication](../authentication/) for how the modes fit together.

| Key | Default | |
| --- | --- | --- |
| `auth.mode` | `off` | `off`: no login. `password`: the users below sign in with a form. `oidc`: sign in through an OpenID Connect provider. `proxy`: trust the user name a reverse proxy sends. |
| `auth.session_secret` | `""` | Signs session cookies. Empty: one is generated and kept in the database. |
| `auth.users[].name` | | The name the user signs in with. In `oidc` mode, their email address, matched only when the provider says it's verified. |
| `auth.users[].password_hash` | | The output of `mailinois hash-password`. Optional in `oidc` mode, where it makes a break-glass login. |
| `auth.users[].sites` | `[]` | Site slugs this user may see. Empty: every site. |
| `auth.proxy.user_header` | `Remote-User` | The header the proxy puts the user name in. |
| `auth.proxy.trusted` | | Addresses or CIDR ranges the header is trusted from, such as `["127.0.0.1/32", "10.0.0.0/8"]`. Needed in proxy mode. |
| `auth.trusted_proxies` | `[]` | Proxies whose `X-Forwarded-For` is believed, in any mode. List each one in the chain, such as Traefik's address and Cloudflare's ranges. `auth.proxy.trusted` counts too. |
| `auth.oidc.issuer` | | The provider's issuer URL, such as `https://accounts.google.com`. Needed in `oidc` mode. |
| `auth.oidc.client_id`, `auth.oidc.client_secret` | | From registering Mailinois with the provider. Put the secret in `${NAME}`. |
| `auth.oidc.redirect_url` | | Mailinois's own address ending in `/auth/callback`, as registered with the provider. |
| `auth.oidc.scopes` | `[openid, email, profile]` | Must include `openid`. Some providers need `groups` too. |
| `auth.oidc.allowed_domains` | `[]` | Lets in people with a verified email address in these domains. |
| `auth.oidc.allowed_groups` | `[]` | Lets in members of any one of these groups. With `allowed_domains` too, both must pass. With neither, only `auth.users`. |
| `auth.oidc.groups_claim` | `groups` | The ID token claim that lists groups. |
| `auth.oidc.group_sites` | `{}` | Group to the site slugs its members see. |
| `auth.oidc.button_label` | `Sign in with SSO` | The login page's button. |
| `auth.tokens[].name` | | A name for the token, such as what uses it. |
| `auth.tokens[].hash` | | The `sha256:` hash that `mailinois token` prints. |
| `auth.tokens[].scopes` | | `read` for the REST API, `ingest` for what agents send. |
| `auth.tokens[].sites` | `[]` | Site slugs this token may see. Empty: every site. |

## sites

Sites group sending domains, exact sender addresses and optional servers so each can be looked at alone, or all together. Without any, the site picker lists the sending domains seen. See [Sites and search](../sites-and-search/).

| Key | Default | |
| --- | --- | --- |
| `sites[].name` | | The name shown in the site picker. |
| `sites[].slug` | from the name | The short name used by `site:` in search, the API's `site` parameter, alerts, and `sites` lists on users and tokens. |
| `sites[].domains` | | Sending domains. Subdomains match too. |
| `sites[].senders` | `[]` | Exact email addresses, matched without case. Use sender-only sites to separate apps sharing a domain. Bounce notices match their recipient. |
| `sites[].servers` | `[]` | Optional: Postfix servers, by hostname, that belong to the site. |

## capture

| Key | Default | |
| --- | --- | --- |
| `capture.mode` | `off` | `off`, `headers` or `full`. Needs `listen.milter`. |
| `capture.max_size` | `1048576` | Bytes kept per message (1 MiB). Anything past it is cut, and the capture is marked as cut. |

## checks

See [DNS and blocklist checks](../dns-checks/).

| Key | Default | |
| --- | --- | --- |
| `checks.sending_ips` | `[]` | Public IPv4 and IPv6 addresses your relays send from. |
| `checks.dnsbl_zones` | five zones | `zen.spamhaus.org`, `b.barracudacentral.org`, `bl.spamcop.net`, `psbl.surriel.com`, `bl.mailspike.net`. |
| `checks.interval` | `6h` | Time between runs. The checks also run at start. |
| `checks.dns_server` | `""` | `host:port` of the resolver for blocklist and DNS lookups. Empty: the system resolver. Spamhaus refuses public resolvers. |
| `checks.domains` | `[]` | Sending domains to check besides the ones in sites and the ones seen in the logs. |
| `checks.dkim_selectors` | `{}` | Extra DKIM selectors per domain, such as `{store-a.example: [s1]}`. Selectors seen in the logs and captures are added. |

## webhooks

See [Alerts and webhooks](../alerts/).

| Key | Default | |
| --- | --- | --- |
| `webhooks[].name` | | The name alerts use in `notify`. |
| `webhooks[].url` | | Where notifications are posted. |
| `webhooks[].format` | `json` | `json`, `slack` or `discord`. |
| `webhooks[].secret` | `""` | `json` only: signs the body, sent as `X-Mailinois-Signature: sha256=<hex HMAC>`. |

## alerts

| Key | Default | |
| --- | --- | --- |
| `alerts[].name` | | Shown in notifications and on the Alerts page. |
| `alerts[].metric` | | `bounce_rate`, `deferral_rate`, `rejected`, `queue_depth`, `queue_oldest`, `blocklisted`, `server_silent` or `dns_failing`. |
| `alerts[].above` | | The threshold. Rates are fractions (`0.05`), `queue_oldest` and `server_silent` are durations (`"2h"`), counts are numbers. |
| `alerts[].window` | `1h` | The period the metric is worked out over. |
| `alerts[].min_messages` | `0` | Rates wait for at least this many outcomes. |
| `alerts[].site` | `""` | Optional: one site's slug. |
| `alerts[].provider` | `""` | Optional, rates only: `google`, `microsoft`, `yahoo`, `apple` or `other`. |
| `alerts[].server` | `""` | Optional: one server. |
| `alerts[].notify` | `[]` | Webhook names to notify. |
| `alerts[].repeat` | `0` | Remind this often while it stays firing. `0` notifies only on firing and resolving. |

## demo

| Key | Default | |
| --- | --- | --- |
| `demo` | `false` | Fill an empty database with made-up traffic and keep it coming. For trying Mailinois out, not for production. |

## Environment variables

These override the file. They suit containers, where a file is one more thing to mount.

| Variable | Sets |
| --- | --- |
| `MAILINOIS_CONFIG` | The path to the config file, like `-config`. |
| `MAILINOIS_DATABASE_URL` | `database.url` |
| `MAILINOIS_HTTP` | `listen.http` |
| `MAILINOIS_SYSLOG` | `listen.syslog` |
| `MAILINOIS_MILTER` | `listen.milter` |
| `MAILINOIS_SENDING_IPS` | `checks.sending_ips`, comma-separated: `203.0.113.10,2001:db8::25` |
| `MAILINOIS_DEMO` | `demo`: `1` turns it on. |
| `MAILINOIS_PASSWORD` | A shortcut for one login: sets `auth.mode` to `password` and adds a user called `admin` with this password (the password itself, not a hash). With `auth.mode: oidc`, the mode stays and `admin` is a break-glass login. |
| `MAILINOIS_PASSWORD_FILE` | The same, with the password read from a file, such as a Docker or Swarm secret. |
| `MAILINOIS_USER` | The user name for that shortcut, instead of `admin`. |
| `MAILINOIS_LOG_TZ` | `log_timezone` |
| `MAILINOIS_RETENTION` | `retention.messages` |
| `MAILINOIS_QUEUE_LIFETIME` | `queue_lifetime` |
| `MAILINOIS_DNS_SERVER` | `checks.dns_server` |
| `MAILINOIS_DNSBL_ZONES` | `checks.dnsbl_zones`, comma-separated |
| `MAILINOIS_DNSBL_INTERVAL` | `checks.interval` |
| `MAILINOIS_URL` | Where the dashboard is reached, for links in alert webhooks. Not in the file. |

## The example file

```yaml title="mailinois.example.yaml"
# Mailinois configuration. Every key is optional; these are the defaults unless the comment says otherwise.
# Point Mailinois at this file with -config or MAILINOIS_CONFIG. Durations are Go durations (90s, 5m, 6h, 720h).

listen:
  http: ":7480"     # dashboard, REST API, /metrics, /healthz
  syslog: ":5514"   # TCP and UDP syslog from Postfix hosts; "" turns it off
  milter: ""        # e.g. ":8892" to capture headers or bodies (see capture); "" turns it off

database:
  # sqlite:<path> (the default, nothing to install) or a postgres:// URL. To share a database that already
  # exists, give Mailinois a schema of its own: postgres://user:pass@db/app?search_path=mailinois
  url: "sqlite:mailinois.db"

retention:
  messages: 720h    # messages, their log lines and blocklist and DNS checks
  captures: 168h    # captured headers and bodies, which are more sensitive

queue_lifetime: 120h  # match Postfix's maximal_queue_lifetime
log_timezone: UTC     # zone of classic syslog timestamps, which carry none

auth:
  # off:      no login. Keep the dashboard on a private network, or put your own auth in front of it.
  # password: users below sign in with a form; sessions are signed cookies.
  # proxy:    trust the user name a reverse proxy puts in user_header, from trusted addresses only.
  # oidc:     sign in through an OpenID Connect provider, set up under oidc below.
  mode: off
  session_secret: ""   # signs session cookies; empty: one is generated and kept in the database
  users:
    - name: sam
      password_hash: "$2a$12$replace.with.the.output.of.mailinois.hash-password"
      sites: []        # empty: every site; otherwise this user only ever sees these sites
  proxy:
    user_header: Remote-User
    trusted: ["127.0.0.1/32", "10.0.0.0/8"]
  # Google Workspace, Entra ID, Okta, Keycloak, authentik… In oidc mode, set allowed_domains, allowed_groups or
  # users (by email address, password optional), or Mailinois won't start: anyone at the provider could sign in.
  oidc:
    issuer: ""                 # e.g. https://accounts.google.com
    client_id: ""
    client_secret: ""          # from the environment, written as a dollar-brace reference
    redirect_url: ""           # https://<this dashboard>/auth/callback, as registered with the provider
    scopes: [openid, email, profile]
    allowed_domains: []        # a verified email address in one of these
    allowed_groups: []         # a member of any one of these, read from groups_claim
    groups_claim: groups
    group_sites: {}            # group: [site slugs] its members see
    button_label: Sign in with SSO
  # Proxies whose X-Forwarded-For is believed, in any mode, so sign-in limits count each client rather than
  # the proxy. List every proxy in the chain (a CDN's ranges too); proxy.trusted counts already.
  trusted_proxies: []
  # API tokens work in every mode. `mailinois token` prints a new token and the hash to put here.
  tokens:
    - name: grafana
      hash: "sha256:replace.with.the.output.of.mailinois.token"
      scopes: [read]   # read: the REST API; ingest: what agents send
      sites: []

# Sites group sending domains, exact sender addresses and optional servers so each can be looked at alone, or all together.
# Without any, the site picker lists the sending domains seen.
sites:
  - name: Store A
    slug: storea
    domains: [store-a.example]       # subdomains match too
    senders: []               # exact addresses; useful for apps sharing one domain
    servers: []
  - name: Store B
    slug: storeb
    domains: [store-b.example]

capture:
  mode: off          # off | headers | full; needs listen.milter
  max_size: 1048576  # bytes kept per message; anything past it is cut and marked as cut

checks:
  sending_ips: []    # public IPv4 and IPv6 addresses your relays send from
  dnsbl_zones: [zen.spamhaus.org, b.barracudacentral.org, bl.spamcop.net, psbl.surriel.com, bl.mailspike.net]
  interval: 6h
  dns_server: ""     # host:port for blocklist and DNS lookups; Spamhaus refuses public resolvers
  domains: []        # sending domains to check besides the ones in sites and the ones seen in the logs
  dkim_selectors: {} # e.g. {store-a.example: [s1]}; selectors seen in the logs and captures are added

webhooks:
  - name: ops
    url: "https://hooks.slack.com/services/XXX"
    format: slack    # json | slack | discord
    secret: ""       # json only: signs the body, sent as X-Mailinois-Signature: sha256=<hex HMAC>

alerts:
  - name: Bounce rate over 5%
    metric: bounce_rate   # bounce_rate | deferral_rate | rejected | queue_depth | queue_oldest | blocklisted | server_silent | dns_failing
    above: 0.05           # rates are fractions; queue_oldest and server_silent are durations ("2h"); counts are numbers
    window: 1h            # the period the metric is worked out over
    min_messages: 20      # rates wait for at least this many outcomes
    site: ""              # optional: one site's slug
    provider: ""          # optional, rates only: google | microsoft | yahoo | apple | other
    server: ""            # optional
    notify: [ops]
    repeat: 6h            # remind while it stays firing; 0 notifies only on firing and resolving

demo: false   # fill an empty database with made-up traffic and keep it coming
```

The users, tokens, sites, webhook and alert in it are examples, not defaults.
