---
title: Authentication
description: Four modes (off, password, single sign-on with OIDC, and proxy), per-site limits for users and tokens, and API tokens.
---

`auth.mode` picks one of four modes:

| Mode | Who gets in |
| --- | --- |
| `off` | Anyone who can reach the dashboard. The default. Keep it on a private network, or put your own authentication in front of it. |
| `password` | The users in `auth.users`, who sign in with a form. Sessions are signed cookies. |
| `oidc` | People your OpenID Connect provider signs in (Google Workspace, Microsoft Entra ID, Okta, Keycloak, authentik and others), limited by domain, group or a list. |
| `proxy` | Whoever a reverse proxy names in `auth.proxy.user_header`, believed only from the addresses in `auth.proxy.trusted`. |

API tokens work in every mode.

## Password mode

Make a password hash for each user. `mailinois hash-password` reads the password on standard input and prints its hash:

```sh
mailinois hash-password
```

Put the output in the config:

```yaml
auth:
  mode: password
  users:
    - name: sam
      password_hash: "$2a$12$…"
    - name: storea-support
      password_hash: "$2a$12$…"
      sites: [storea]
```

Sessions are signed cookies. `auth.session_secret` signs them; left empty, Mailinois generates one and keeps it in the database, so sessions survive a restart. Changing the secret signs everyone out.

Put TLS in front of password mode. Without it, passwords and session cookies cross the network in the clear. Caddy, for example, fetches a certificate on its own:

```txt title="Caddyfile"
mailinois.example.com {
	reverse_proxy 127.0.0.1:7480
}
```

### Sign-in limits

After 5 failed sign-ins in 15 minutes, a client gets `429 Too Many Requests` until the oldest failure is 15 minutes old, right password or not. After 50 failures from everyone together, every sign-in waits the same way: a guess spread over many addresses stops too. Each failure is logged with the name and the client's address.

Behind a reverse proxy, every request comes from the proxy, so list it in `auth.trusted_proxies`. Mailinois then reads `X-Forwarded-For` from the right, skipping the proxies listed, and counts the first address that isn't one of them. Anything further left is whatever the client sent. With a CDN in front, list its ranges too:

```yaml
auth:
  trusted_proxies: ["10.1.0.2", "173.245.48.0/20", "103.21.244.0/22"] # Traefik, then Cloudflare's ranges
```

Sign-in and sign-out forms are refused when the browser says they came from another site (`Sec-Fetch-Site`, or `Origin` from older browsers). API clients and agents send neither header, and need a token anyway.

### The shortcut

`MAILINOIS_PASSWORD` (or `MAILINOIS_PASSWORD_FILE`, read from a file such as a Docker secret) sets the mode to `password` and adds a user called `admin` with that password. It is the plain password, not a hash. Handy for a single login in a container. With `auth.mode: oidc` it leaves the mode alone and adds `admin` as a [break-glass login](#break-glass-passwords).

## Single sign-on with OIDC

Mailinois can send people to your identity provider to sign in, over OpenID Connect. Register Mailinois with the provider as a web application, with this redirect (callback) URL:

```txt
https://mailinois.example.com/auth/callback
```

Then:

```yaml
auth:
  mode: oidc
  oidc:
    issuer: https://accounts.google.com
    client_id: "123456789.apps.googleusercontent.com"
    client_secret: "${OIDC_CLIENT_SECRET}"
    redirect_url: https://mailinois.example.com/auth/callback
    allowed_domains: [example.com]
```

The login page then shows a **Sign in with SSO** button (`button_label` changes it). Mailinois sends the browser to the provider with a random state, a nonce and a PKCE challenge, swaps the code it gets back for tokens, and checks the ID token's signature, issuer, audience, expiry and nonce before trusting anything in it. The session that follows is the same signed cookie as password mode, for 12 hours. Mailinois keeps no refresh tokens.

If the provider can't be reached when Mailinois starts, it starts anyway, logs a warning, and tries again at the next sign-in.

### Who may sign in

Set at least one of these. Mailinois refuses to start in `oidc` mode with none of them, because then anyone with an account at the provider could sign in (for Google, that is anyone with a Google account).

| Key | Lets in |
| --- | --- |
| `allowed_domains` | People whose email address is in one of these domains and which the provider says is verified (`email_verified` true). |
| `allowed_groups` | People in any one of these groups, read from the `groups_claim` claim (default `groups`). |
| `auth.users` | Only when neither of the above is set: the users listed, by email address, once the provider says it's verified. |

With both `allowed_domains` and `allowed_groups`, someone has to pass both. Refusals answer `403` with a plain message, and the log says why.

An email address only counts when the ID token says it's verified (`email_verified` true). A provider that lets people type in any address, unchecked, would otherwise let anyone become a listed user: the ["nOAuth" problem](https://www.descope.com/blog/post/noauth) with apps that trust Entra ID's email claim.

### Which sites they see

1. The sites their groups map to in `group_sites`, all of them together.
2. Otherwise, the `sites` of an `auth.users` entry whose `name` is their email address, if the provider says that address is verified.
3. Otherwise, every site.

```yaml
auth:
  mode: oidc
  oidc:
    # …
    scopes: [openid, email, profile, groups]   # some providers need a groups scope for the groups claim
    allowed_groups: [mail-admins, storea-support]
    group_sites:
      storea-support: [storea]
  users:
    - name: contractor@example.com             # no password: just their sites
      sites: [storeb]
```

The sites are worked out at sign-in and kept in the session, so a change of group applies the next time someone signs in.

### Break-glass passwords

Users in `auth.users` with a `password_hash` can still sign in with a password, on a form under the SSO button: useful when the provider is down. The [sign-in limits](#sign-in-limits) apply to the form and to failed SSO sign-ins alike.

### Providers

On 19th October 2026 we ran the whole flow against [Dex](https://dexidp.io/) v2.46.0, in a local container with two users: the one in the allowed domain signed in and reached the queue, the one in another domain got `403` and the log said why, and with Dex stopped, Mailinois started anyway and signed people in once Dex was back. The providers below have not been run against Mailinois. They are standard OIDC; these notes come from their own documentation.

| Provider | `issuer` | Groups |
| --- | --- | --- |
| Google Workspace | `https://accounts.google.com` | Not in the ID token. Use `allowed_domains`. |
| Microsoft Entra ID | `https://login.microsoftonline.com/<tenant-id>/v2.0` | An optional claim, of group object IDs. Use `allowed_groups`. |
| Okta | `https://<your-okta-domain>/oauth2/default`, or `https://<your-okta-domain>` for the org server | Add a groups claim to the ID token. The org server also needs the `groups` scope. |
| Keycloak | `https://<host>/realms/<realm>` | Add a **Group Membership** mapper with the claim name `groups`. Leave **Full group path** off to get plain names. |
| authentik | `https://<host>/application/o/<app-slug>/` | Part of the `profile` scope. |

- **Google** ([OpenID Connect](https://developers.google.com/identity/openid-connect/openid-connect)): ID tokens carry `email_verified`, and `hd` for Workspace members. Mailinois checks the verified email's domain, not `hd`, so a personal Google account made with a work address, and verified, also passes `allowed_domains`.
- **Entra ID** ([ID token claims](https://learn.microsoft.com/en-us/entra/identity-platform/id-token-claims-reference)): the claims reference has no `email_verified`, so `allowed_domains` would refuse everyone. Use `allowed_groups` with the groups' object IDs. Someone in more than 200 groups gets no `groups` claim at all (Entra's "overage"), and is refused.
- **Okta** ([groups claim](https://developer.okta.com/docs/guides/customize-tokens-groups-claim/main/)).
- **Keycloak** ([OIDC endpoints](https://www.keycloak.org/securing-apps/oidc-layers)).
- **authentik** ([OAuth2 provider](https://docs.goauthentik.io/add-secure-apps/providers/oauth2/)): its docs say the `profile` scope includes group membership, without naming the claim. Check a token, and set `groups_claim` to match.

## Proxy mode

Let a reverse proxy that already signs people in tell Mailinois who they are:

```yaml
auth:
  mode: proxy
  proxy:
    user_header: Remote-User
    trusted: ["127.0.0.1/32", "10.0.0.0/8"]
```

Mailinois believes the header only on requests from the `trusted` addresses, so list the proxy's address and nothing broader than you need. Make sure the proxy removes any `Remote-User` header a client sends, and sets its own.

If `auth.users` has a user of the same name, that user's `sites` list applies to them.

### SSO through your proxy

If an authenticating proxy already sits in front of your internal tools, proxy mode gives Mailinois the same single sign-on without its own OIDC setup:

- **Authelia** sets `Remote-User`.
- **oauth2-proxy**: set `user_header` to the header it passes upstream, such as `X-Forwarded-User` or `X-Forwarded-Email`.
- **Caddy's `forward_auth`** can copy the header from your identity provider's response.
- **Cloudflare Access** adds `Cf-Access-Authenticated-User-Email`. Mailinois doesn't check Access's signed token, only the header, so use proxy mode with it only when nothing but Cloudflare can reach the proxy in front of Mailinois. Otherwise keep `oidc` or `password` mode on behind Access.

## Limiting users and tokens to sites

Users and tokens can be limited to [sites](../sites-and-search/#sites):

```yaml
sites: [storea]
```

That user or token only ever sees those sites, on every page and in every API call. An empty list, the default, means every site.

## API tokens

`mailinois token` prints a new token and its `sha256:` hash. The hash goes in the config. The token goes to whatever calls the API, and is not stored anywhere by Mailinois.

```yaml
auth:
  tokens:
    - name: grafana
      hash: "sha256:…"
      scopes: [read]
    - name: mail-2-agent
      hash: "sha256:…"
      scopes: [ingest]
      sites: []
```

| Scope | Allows |
| --- | --- |
| `read` | The [REST API](../api/). |
| `ingest` | What agents send, to `POST /api/v1/ingest`. |

Send the token as a bearer token:

```sh
curl -H "Authorization: Bearer $TOKEN" https://mailinois.example.com/api/v1/summary
```

## Layering

The modes combine with what you already run:

- **An authenticating proxy in front, and password or OIDC mode on.** Two separate checks before anyone sees mail metadata.
- **Proxy mode** behind Authelia, oauth2-proxy or Caddy, which sets `Remote-User` after its own sign-in.
- **Mode `off`** on a private network, a VPN or an SSH tunnel. The dashboard is read-only, but it still shows every address that mailed through your relays, so keep it off the open internet.
