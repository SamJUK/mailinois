---
title: Monitor Postfix on Kubernetes
description: Three ways to get Postfix's log from a pod to Mailinois, by agent sidecar, by the Vector or Fluent Bit you already run, or by syslog inside the cluster, and which one gives you header capture.
template: splash
---

In a cluster, Postfix's log is container output. The node's runtime keeps it until the pod goes, and pods move. The host-based answers (an rsyslog file in `/etc/rsyslog.d/`, the agent following `/var/log/mail.log`) don't apply as they are.

There are three ways in. Which one to use depends on whether you control the Postfix pod, and whether you want subjects and Message-IDs as well as the log.

| Way in | Changes the Postfix pod | Header capture | Authenticated |
| --- | --- | --- | --- |
| The agent as a sidecar | Yes: one more container | Yes, over the pod's loopback | Yes, a token over HTTPS |
| A log shipper you already run | No | No | Yes, a token over HTTPS |
| Syslog to Mailinois's Service | Yes: an rsyslog file | Only with a milter port too | No |

## The agent as a sidecar

Run `mailinois agent` in the Postfix pod. Postfix's rsyslog forwards to it on `127.0.0.1:5514`, Postfix hands each message to its milter on `127.0.0.1:8892`, and the agent ships both to Mailinois with a token, spooling to disk while Mailinois is unreachable. Nothing listens outside the pod. The repository's `deploy/kubernetes/postfix-with-agent.yaml` does this for boky/postfix, with Postfix set to accept mail whenever the milter can't be reached.

This is the one to choose when you run Postfix yourself and want everything Mailinois shows. [Kubernetes: Postfix with the agent beside it](../../docs/kubernetes/#postfix-with-the-agent-beside-it) has the manifest and the token setup.

## A log shipper you already run

If Vector or Fluent Bit already collects container logs, send Postfix's lines to `POST /api/v1/ingest` with an ingest token, and leave the Postfix pod alone. Mailinois takes `text/plain`, one log line per line (Vector's `text` codec), or `application/x-ndjson` with the line in `message` or `log` (Fluent Bit's `json_lines`). `?server=mail-1` names the server, because container output usually has no hostname in it.

The shipper only sees the log, so there is no header capture this way. [Kubernetes: a log shipper you already run](../../docs/kubernetes/#a-log-shipper-you-already-run) has the [Vector](../../docs/kubernetes/#vector) and [Fluent Bit](../../docs/kubernetes/#fluent-bit) configs.

## Syslog to Mailinois's Service

Mailinois's own manifest, `deploy/kubernetes/mailinois.yaml`, runs it as a one-replica StatefulSet with its SQLite on a volume, and exposes syslog on its Service. A Postfix pod can mount the rsyslog file with `target` set to that Service's name. Syslog has no authentication, so this is for a cluster where a NetworkPolicy lets only the Postfix pods reach port 5514. [Kubernetes: syslog straight from a pod](../../docs/kubernetes/#syslog-straight-from-a-pod) has the details.

## Running Mailinois itself

One replica, one volume: Mailinois joins each message's lines in one place, so it doesn't run as several copies. [Sizing](../../docs/sizing/) has what one copy has been measured to handle. [Kubernetes: run Mailinois](../../docs/kubernetes/#run-mailinois) has the StatefulSet, Service and config.

For Postfix outside the cluster, see [Debian and Ubuntu](../../setups/postfix-debian-ubuntu/) or [Docker](../../setups/postfix-docker/).
