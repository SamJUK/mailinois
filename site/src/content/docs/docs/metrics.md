---
title: Metrics
description: Prometheus text at /metrics, for the queue, blocklists, delivery outcomes and Mailinois's own health.
---

`/metrics` on the dashboard's address serves Prometheus text:

- queue depth and the age of the oldest message, per server;
- blocklist listings, per IP and zone;
- delivery outcomes, by server and status;
- Mailinois's own health: syslog frames received, lines dropped, write errors and the buffer.

## The metrics

These are the names in the current version. `curl http://localhost:7480/metrics` shows exactly what yours exports.

| Metric | Type | Labels | |
| --- | --- | --- | --- |
| `mailinois_queue_messages` | gauge | `server` | Messages still in a Postfix queue. |
| `mailinois_queue_oldest_seconds` | gauge | `server` | Age of the oldest message in the queue, 0 when it is empty. |
| `mailinois_dnsbl_listed` | gauge | `ip`, `zone` | 1 when the latest check found the IP on that blocklist. |
| `mailinois_alert_firing` | gauge | `alert` | 1 while an alert is firing. |
| `mailinois_outcomes_total` | counter | `server`, `status` | Delivery attempts and rejections ingested since start. |
| `mailinois_syslog_frames_total` | counter | | Syslog frames received. |
| `mailinois_syslog_unparsed_total` | counter | | Frames that were not syslog. |
| `mailinois_lines_dropped_total` | counter | | Lines and captures dropped, because the buffer was full or the database was away too long. |
| `mailinois_ingest_errors_total` | counter | | Failed writes to the database. |
| `mailinois_captures_total` | counter | | Captured messages saved. |
| `mailinois_lines_buffered` | gauge | | Lines waiting to be written. |

## Scraping

`/metrics` sits behind the same authentication as the API. With `auth.mode` set to `off` it is open; otherwise give Prometheus a token with the `read` scope:

```yaml title="prometheus.yml"
scrape_configs:
  - job_name: mailinois
    authorization:
      type: Bearer
      credentials_file: /etc/prometheus/mailinois-token
    static_configs:
      - targets: ["mailinois.internal:7480"]
```

`/healthz`, on the same address, answers health checks without a login: `ok` while the database is reachable, and `503` when it is not.
