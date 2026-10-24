---
title: REST API
description: Read Mailinois's data as JSON under /api/v1, with bearer tokens and the same search language as the dashboard.
---

The API serves JSON under `/api/v1`, on the same address as the dashboard (`listen.http`).

## Authentication

Send a token with the `read` scope as a bearer token:

```sh
curl -H "Authorization: Bearer $TOKEN" https://mailinois.example.com/api/v1/summary
```

`mailinois token` makes one; [Authentication](../authentication/#api-tokens) has the details. A token limited to sites only ever sees those sites. A missing or unknown token gets `401`, and a token without the right scope gets `403`.

When `auth.mode` is `off`, the read endpoints are open too, like the dashboard. `POST /api/v1/ingest` is not: it always needs a token with the `ingest` scope, whatever the mode, because whatever can reach it can add log lines.

## Scoping

Endpoints that list or count take the same scope as the dashboard, as query parameters:

| Parameter | |
| --- | --- |
| `site` | A site's slug, or `@` and a sending domain: `@store-a.example`. |
| `server` | A server's hostname. |
| `period` | `1h`, `24h`, `7d` or `30d`. Default `24h`. |
| `q` | A query in the [search language](../sites-and-search/#search). `after:` and `before:` in it override `period`. |

`q` needs URL encoding. curl's `-G --data-urlencode` does it for you:

```sh
curl -G -H "Authorization: Bearer $TOKEN" https://mailinois.example.com/api/v1/messages \
  --data-urlencode 'q=from:store-a.example provider:microsoft status:deferred after:2d'
```

## Endpoints

| Method and path | Returns |
| --- | --- |
| `GET /api/v1/summary` | The headline numbers for the scope and period. |
| `GET /api/v1/messages` | Messages matching `q`. Takes `limit` (default 100, at most 500) and `cursor`. |
| `GET /api/v1/messages/{id}` | One message, with its recipients, its timeline and its capture if it has one. |
| `GET /api/v1/messages/{id}/eml` | The captured message as an `.eml` file, if there is one. |
| `GET /api/v1/queue` | What is in the queue. |
| `GET /api/v1/providers` | Outcomes by provider. |
| `GET /api/v1/servers` | The Postfix servers Mailinois has seen. |
| `GET /api/v1/ips` | Sending IPs, with reverse DNS and blocklist results. |
| `GET /api/v1/domains` | Sending domains, with SPF, DKIM and DMARC results. |
| `GET /api/v1/alerts` | Alert rules and their state. |
| `POST /api/v1/ingest` | Log lines from agents and log shippers. Needs the `ingest` scope. See [Ingest](#ingest). |

`{id}` is the `id` of a message in a messages listing, not its queue ID: Postfix reuses queue IDs, so they can't name a message on their own.

### Paging

`/api/v1/messages` returns at most `limit` messages. When there are more, the response has a `next_cursor`. Pass it back as `cursor` for the next page, and stop when it is absent.

```sh
curl -G -H "Authorization: Bearer $TOKEN" https://mailinois.example.com/api/v1/messages \
  --data-urlencode 'q=site:storeb status:bounced' \
  --data-urlencode 'limit=200' \
  --data-urlencode 'cursor=200'
```

### More examples

```sh
# This week's summary for one site
curl -H "Authorization: Bearer $TOKEN" "https://mailinois.example.com/api/v1/summary?site=storea&period=7d"

# The queue on one server
curl -H "Authorization: Bearer $TOKEN" "https://mailinois.example.com/api/v1/queue?server=mail-1"

# Download a captured message
curl -H "Authorization: Bearer $TOKEN" -o message.eml "https://mailinois.example.com/api/v1/messages/<id>/eml"
```

## Ingest

`POST /api/v1/ingest` takes log lines, and captures from the agent's milter. It always needs a token with the `ingest` scope. The body can be gzip-compressed (`Content-Encoding: gzip`) and is at most 8 MiB, compressed or not. The `Content-Type` says what's in it:

| `Content-Type` | Body | Sent by |
| --- | --- | --- |
| `application/json` | `{"server": "…", "lines": ["…"], "captures": […]}` | [The agent](../sending-logs/#the-agent-for-servers-on-other-networks). |
| `text/plain` | One log line per line. | Vector's `http` sink with `encoding.codec = "text"`, or `curl`. |
| `application/x-ndjson` | One JSON object per line, with the log line in `message` or `log`. | Vector's `json` codec (`message`), Fluent Bit's `format json_lines` (`log`). |

Each line is read the way the syslog listener reads one, so it needs its timestamp: a line from `/var/log/mail.log`, `docker logs`, Postfix's own `maillog_file` or rsyslog's forward format works as it is. Lines that name no host, such as a container's output, are put under the server named by `?server=`.

```sh
grep postfix/ /var/log/mail.log | gzip | curl -sS --data-binary @- \
  -H "Authorization: Bearer $INGEST_TOKEN" -H "Content-Type: text/plain" -H "Content-Encoding: gzip" \
  "https://mailinois.example.com/api/v1/ingest?server=mail-1"
```

```json
{ "lines": 1824, "skipped": 0 }
```

`skipped` counts JSON lines that aren't an object with a `message` or `log` string.

When Mailinois is behind and its queue stays full for 10 seconds, it answers `503` with `Retry-After: 10` and how many lines it took. Send the whole batch again later: lines already taken are skipped. Lines already stored are ignored, so a shipper retrying a batch adds nothing twice. Configs for Vector and Fluent Bit are in [Kubernetes](../kubernetes/).

## Response shapes

Errors come back as JSON with an `error` field:

```json
{ "error": "not found" }
```

The examples below are real responses from the demo (`docker compose up` in the repo), trimmed where a list was long. Times are RFC 3339, durations are seconds, and lists are `[]` rather than `null` when empty.

### Summary

`buckets` holds one entry per step of the period (an hour for `24h`), and `failures` the latest ten. `p50` and `p95` are delivery times in seconds.

```json
{
  "period": {
    "since": "2026-10-17T23:00:00Z",
    "until": "2026-10-18T23:00:00Z"
  },
  "received": 407,
  "sent": 479,
  "deferrals": 65,
  "bounced": 7,
  "expired": 2,
  "rejected": 8,
  "queue": 14,
  "oldest": "2026-10-14T02:33:06.225985Z",
  "p50": 1.15,
  "p95": 1.89,
  "buckets": [
    {
      "at": "2026-10-18T21:00:00Z",
      "sent": 172,
      "deferred": 11,
      "bounced": 2,
      "rejected": 2
    },
    {
      "at": "2026-10-18T22:00:00Z",
      "sent": 7,
      "deferred": 1,
      "bounced": 0,
      "rejected": 0
    }
  ],
  "failures": [
    {
      "at": "2026-10-18T22:00:07.127461Z",
      "server": "relay-fsn-01",
      "message_id": 3993,
      "recipient": "ava@outlook.example",
      "sender": "no-reply@shop.example",
      "status": "deferred",
      "dsn": "4.7.650",
      "response": "host outlook-com.olc.protection.outlook.com[52.101.68.4] said: 451 4.7.650 The mail server [198.51.100.20] has been temporarily rate limited due to IP reputation. For e-mail delivery information, see https://aka.ms/postmaster (S775) (in reply to MAIL FROM command)"
    }
  ]
}
```

### Messages

Message rows, queue rows and details include `sender_known`. It is false when the sender has not been logged; a true value with an empty `sender` means Postfix logged the null sender `<>`. Cancelled SMTP transactions have status `cancelled` and do not contribute to Received or queue counts.

```json
{
    "period": {
        "since": "2026-10-17T23:00:00Z",
        "until": "2026-10-18T23:00:00Z"
    },
    "messages": [
        {
            "id": 3993,
            "received_at": "2026-10-18T22:00:06.263461Z",
            "server": "relay-fsn-01",
            "queue_id": "76FD2DF23D",
            "sender": "no-reply@shop.example",
            "subject": "Your data is ready",
            "status": "deferred",
            "size": 32689,
            "recipient": "ava@outlook.example",
            "recipients": 1
        }
    ],
    "next_cursor": "1"
}
```
