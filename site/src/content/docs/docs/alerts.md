---
title: Alerts and webhooks
description: Rules over delivery rates, the queue, blocklists, silent servers and DNS, sent to JSON, Slack or Discord webhooks.
---

An alert is a rule over one metric, scoped as narrowly as you like, that notifies one or more webhooks when it fires and again when it resolves.

```yaml
webhooks:
  - name: ops
    url: "https://hooks.slack.com/services/XXX"
    format: slack

alerts:
  - name: Bounce rate over 5%
    metric: bounce_rate
    above: 0.05
    window: 1h
    min_messages: 20
    site: storea
    notify: [ops]
    repeat: 6h
```

## Metrics

| Metric | `above` is | The value |
| --- | --- | --- |
| `bounce_rate` | a fraction (`0.05` is 5%) | The share of final outcomes in the window that failed. |
| `deferral_rate` | a fraction | The share of delivery attempts in the window that were deferred. |
| `rejected` | a count | Messages Postfix refused in the window. |
| `queue_depth` | a count | Messages in the queue now. |
| `queue_oldest` | a duration (`"2h"`) | How long the oldest message in the queue has waited. |
| `blocklisted` | a count | Blocklist listings of your sending IPs. `above: 0` fires on the first one. |
| `server_silent` | a duration | The longest time since a server's last log line. |
| `dns_failing` | a count | SPF, DKIM, DMARC and reverse DNS checks that are failing. |

An alert fires when the value goes above `above`, and resolves when it comes back down to it or below.

For `server_silent`, a server that has sent nothing for over a week counts as retired, not silent, unless the rule names it with `server`.

## Options

| Key | |
| --- | --- |
| `window` | The period the metric is worked out over. Default `1h`. |
| `min_messages` | Rates wait for at least this many outcomes. Until then the alert stays as it was, so two messages and one bounce at 3am don't page anyone. |
| `site` | Only this site's mail. |
| `provider` | Rates only: `google`, `microsoft`, `yahoo`, `apple` or `other`. |
| `server` | Only this server. |
| `notify` | The webhooks to send to, by name. |
| `repeat` | Send a reminder this often while it stays firing. `0`, the default, notifies only on firing and on resolving. |

Some rules worth having, as a starting point:

```yaml
alerts:
  - name: Microsoft deferring Store A
    metric: deferral_rate
    above: 0.2
    window: 30m
    min_messages: 50
    site: storea
    provider: microsoft
    notify: [ops]

  - name: Mail stuck in the queue
    metric: queue_oldest
    above: "2h"
    notify: [ops]

  - name: Listed on a blocklist
    metric: blocklisted
    above: 0
    notify: [ops]

  - name: A relay stopped logging
    metric: server_silent
    above: "30m"
    notify: [ops]
```

The Alerts page shows each rule's state and its history.

## Webhooks

```yaml
webhooks:
  - name: ops
    url: "https://hooks.slack.com/services/XXX"
    format: slack
  - name: chat
    url: "https://discord.com/api/webhooks/…"
    format: discord
  - name: pager
    url: "https://ops.example.com/hooks/mailinois"
    format: json
    secret: "a long random string"
```

| `format` | Sends |
| --- | --- |
| `json` | The payload below. The default. |
| `slack` | A message to a Slack incoming webhook. |
| `discord` | A message to a Discord webhook. |

Each notification is an HTTP `POST`, tried up to three times.

### The JSON payload

```json
{
  "alert": "Bounce rate over 5%",
  "state": "firing",
  "metric": "bounce_rate",
  "value": 0.083,
  "threshold": 0.05,
  "scope": { "site": "storea", "provider": "", "server": "" },
  "since": "2026-10-18T09:00:00Z",
  "url": "https://mailinois.example.com/alerts"
}
```

`state` is `firing`, or `resolved` when the alert clears. `since` is when it entered that state. `url`, when present, links to the Alerts page.

### Verifying the signature

With `secret` set, `json` webhooks carry an `X-Mailinois-Signature` header:

```txt
X-Mailinois-Signature: sha256=<hex>
```

The hex is the HMAC-SHA256 of the raw request body, keyed with the secret. Compute it over the bytes exactly as they arrived, before parsing the JSON, and compare in constant time.

```python
import hashlib
import hmac

def verify(body: bytes, header: str, secret: str) -> bool:
    expected = "sha256=" + hmac.new(secret.encode(), body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(expected, header)
```

```go
func verify(body []byte, header, secret string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(header))
}
```

Slack and Discord webhooks are not signed: the secret in their URL is what protects them.
