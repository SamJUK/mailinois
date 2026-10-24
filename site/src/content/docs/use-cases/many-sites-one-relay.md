---
title: Many client sites on one Postfix relay
description: Send for many shops or clients through one Postfix relay, then look at each client's mail alone or all together, give each client's team a login or API token that only sees their own, and alert per client.
template: splash
---

Agencies and hosting companies often run one Postfix relay, or a pair, for every client's website. It's cheaper and easier than a relay each. The cost shows up when something goes wrong: the log is everyone's mail at once, and "is Store B's order mail getting through?" means grepping through Store A's too.

Mailinois groups mail into **sites**. A site is one client, shop or brand: its sending domains, and optionally the servers that are only theirs.

## Defining the sites

```yaml
sites:
  - name: Store A
    slug: storea
    domains: [store-a.example]                 # subdomains match too: mail.store-a.example, news.store-a.example
  - name: Store B
    slug: storeb
    domains: [store-b.example, store-b-uk.example]
  - name: Fabrikam
    slug: fabrikam
    domains: [fabrikam.example]
    servers: [relay-fabrikam]             # a relay only Fabrikam uses: all its mail counts, whatever the sender
```

A message belongs to a site when its sender's domain is one of the site's domains, or when it went through one of the site's servers. A non-delivery notice has no sender, so it counts towards the site of the address it's returned to.

Without any sites configured, the site picker lists the busiest sending domains of the last week, so you can still look at one domain at a time before writing any config.

## One client, or all of them

The site picker in the top bar scopes every page to one site. **All sites** puts them back together, which is the view for the relay itself: is the IP on a blocklist, is the queue backing up, is Microsoft throttling everyone.

The same scope works in search and in the API:

```txt
site:storeb status:bounced,expired after:7d
site:@store-b-uk.example provider:microsoft status:deferred
```

```sh
curl -H "Authorization: Bearer $TOKEN" "https://mailinois.example.com/api/v1/summary?site=storeb&period=7d"
```

## A login per client

In password mode, each user can be limited to some sites. They see only those sites' mail, on every page and in every API call, and the site picker only offers them:

```yaml
auth:
  mode: password
  users:
    - name: sam
      password_hash: "$2a$12$…"            # every site
    - name: storeb-support
      password_hash: "$2a$12$…"
      sites: [storeb]
```

API tokens take the same `sites:` list, so a client's own systems (their support desk, their shop's admin) can look up their customers' mail and nothing else:

```yaml
auth:
  tokens:
    - name: storeb-helpdesk
      hash: "sha256:…"                    # from `mailinois token`
      scopes: [read]
      sites: [storeb]
```

With single sign-on, `auth.mode: oidc` can do the same by group: `group_sites` maps a group at your identity provider (Google Workspace, Entra ID, Okta, Keycloak, authentik) to the sites its members see, so adding someone to a client's group is all it takes. Behind a single sign-on proxy instead, `auth.mode: proxy` takes the user name from a header, and a user listed in the config keeps their `sites:` limit. [Authentication](../../docs/authentication/) has all four modes.

## Alerts per client

Every alert can be scoped to one site, so one client's problem pages the right people and doesn't drown in everyone else's averages:

```yaml
alerts:
  - name: Store B bounce rate over 5%
    metric: bounce_rate
    above: 0.05
    window: 1h
    min_messages: 20
    site: storeb
    notify: [storeb-team]

  - name: Store B's SPF or DKIM failing
    metric: dns_failing
    above: 0
    site: storeb
    notify: [storeb-team]
```

A shared relay's IP-level problems (blocklists, the queue) affect every client, so those rules stay unscoped and go to whoever runs the relay. [Alerts](../../docs/alerts/) has the rest.

## DNS for every client's domain

Every site's domains get the [DNS checks](../../docs/dns-checks/): SPF against each of the relay's IPs, DKIM for each selector seen, and DMARC. On a shared relay that catches the most common slip of all: a client edits their SPF record to add a new newsletter tool and drops the relay's IP. It shows as a failure for that domain and that IP, on that client's site, at the next check.

## What it doesn't do

Sites are a way of looking at mail, not a way of sending it. Mailinois doesn't route, rate limit or bill per client, and it doesn't keep usage reports per site beyond what the overview and the API's summary show for a period. A client sending from a domain that isn't in their site's list won't show under that site until you add it.

How you get the relay's logs to Mailinois doesn't change with the number of clients: see [setups](../../setups/) for Debian or Ubuntu, Docker, mailcow and Kubernetes.
