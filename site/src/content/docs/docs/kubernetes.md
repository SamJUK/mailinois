---
title: Kubernetes
description: Run Mailinois in a cluster, and get Postfix's lines to it with the agent beside Postfix or with a log shipper you already run.
---

Two manifests in the repository's `deploy/kubernetes/` cover the usual setup: `mailinois.yaml` runs Mailinois itself, and `postfix-with-agent.yaml` runs Postfix with the agent in the same pod. If you already run Vector or Fluent Bit, they can send Postfix's container lines instead.

## Run Mailinois

Make a namespace and a Secret with the dashboard password, then apply the manifest:

```sh
kubectl create namespace mailinois
kubectl -n mailinois create secret generic mailinois --from-literal=password='<a long password>'
kubectl -n mailinois apply -f deploy/kubernetes/mailinois.yaml
```

Sign in as `admin` with that password:

```sh
kubectl -n mailinois port-forward svc/mailinois 7480
```

Then open <http://localhost:7480>.

What the manifest sets up:

- **A StatefulSet with one replica** and a 5 GiB volume for SQLite. Always one: SQLite has one writer, and Mailinois ties each log line to the earlier lines for the same queue ID, which needs a single process. A restart loses nothing, because rsyslog's queue and the agent's spool hold lines until Mailinois is back.
- **The config in a ConfigMap**, mounted at `/etc/mailinois/mailinois.yaml`. Edit it, apply, then restart the pod, which doesn't pick up changes on its own: `kubectl -n mailinois rollout restart statefulset/mailinois`.
- **The password from the Secret**, read from a file (`MAILINOIS_PASSWORD_FILE`), so it never sits in the pod's environment.
- **A Service** with the dashboard, API and agent ingest on 7480, and syslog on 5514 over TCP.
- **The image's non-root user**, a read-only root filesystem and no Linux capabilities. Health checks call `/healthz`.
- **A NetworkPolicy.** Syslog has no authentication, so only pods labelled `mailinois.io/sender: "true"`, in any namespace, may reach 5514. Port 7480 is open to the cluster, since Mailinois checks tokens and passwords itself. Your network plugin has to support NetworkPolicy for this to do anything.

To reach the dashboard through an ingress controller, point the ingress at the `mailinois` Service on port 7480. Then list the controller's addresses in `auth.trusted_proxies`, so the [sign-in limits](../authentication/#sign-in-limits) count each client rather than the controller:

```yaml
auth:
  trusted_proxies: ["10.244.0.0/16"] # your pod network, or the controller's pods
```

## Postfix with the agent beside it

This is the way to use when Postfix runs in the cluster. The agent shares Postfix's pod, so Postfix's syslog and its milter only ever travel over the pod's loopback. The agent then ships to Mailinois with a token, and keeps a spool while Mailinois is away. It's the only way to get [captures](../capture/) from a pod.

Make a token with the `ingest` scope:

```sh
mailinois token
```

Its hash goes in Mailinois's ConfigMap, in place of the placeholder:

```yaml
auth:
  tokens:
    - name: postfix
      hash: "sha256:…"
      scopes: [ingest]
```

Restart Mailinois after applying the change. The token itself goes in a Secret for the agent:

```sh
kubectl -n mailinois create secret generic mailinois-agent --from-literal=token='<the token>'
```

Set `ALLOWED_SENDER_DOMAINS`, `POSTFIX_myhostname` and `POSTFIX_mynetworks` in `postfix-with-agent.yaml` for your own mail, then apply it:

```sh
kubectl -n mailinois apply -f deploy/kubernetes/postfix-with-agent.yaml
```

What's in the pod:

- **Postfix** (`boky/postfix`, pinned by digest). A ConfigMap mounted at `/etc/rsyslog.d/mailinois.conf` forwards its lines to the agent on `127.0.0.1:5514`. It's the same file as `deploy/rsyslog/mailinois.conf`, with `target="127.0.0.1"`.
- **Milters:** OpenDKIM first, then the agent's milter on `127.0.0.1:8892`, so captures carry the DKIM signature. The agent's milter has its own `default_action=accept` (`{ inet:127.0.0.1:8892, default_action=accept }`), so if the agent is down or slow, Postfix accepts the mail anyway, whatever the global `milter_default_action` says. We checked it in k3s with the global set to `tempfail` and the agent's milter on a dead port: the mail was accepted. The plain `inet:127.0.0.1:8892` form refused the same mail with `451 4.7.1`.
- **The runtime's default seccomp filter** for both containers.
- **The agent**, as the image's non-root user with a read-only root filesystem. It reads its token from the Secret (`-token-file`), captures headers only, and spools to an `emptyDir`, which outlives container restarts but not the pod.
- **`hostname: postfix`**, which names the server in Mailinois. rsyslog stamps each line with the pod's hostname, which is otherwise the pod's name and changes on every restart.
- **`enableServiceLinks: false`.** Kubernetes would otherwise add `POSTFIX_SERVICE_HOST` and the like for the `postfix` Service, and `boky/postfix` writes every `POSTFIX_*` variable into `main.cf`.

The agent sends to `http://mailinois:7480`, which is right when both run in the same namespace. From another namespace, use `http://mailinois.<namespace>.svc:7480`. In the agent's log you'll see a warning that the URL is plain HTTP. That's expected inside the cluster; across networks, use an HTTPS ingress.

## Syslog straight from a pod

A Postfix pod can also forward syslog to the Service directly, with `deploy/rsyslog/mailinois.conf` and `target="mailinois.mailinois.svc"`. The NetworkPolicy lets it through only with the label:

```yaml
metadata:
  labels:
    mailinois.io/sender: "true"
```

There's no spool and no captures this way. Use the agent unless Postfix can't run one.

## A log shipper you already run

If Vector or Fluent Bit already collects the cluster's container logs, it can post Postfix's lines to the ingest endpoint. There are no captures this way, because those need the milter. Make a token with the `ingest` scope as above, and give it to the shipper.

`POST /api/v1/ingest` takes, besides the agent's own format:

| `Content-Type` | Body |
| --- | --- |
| `text/plain` | Log lines, one per line. Vector's `http` sink with `encoding.codec = "text"` sends this. |
| `application/x-ndjson` | One JSON object per line, with the log line in `message` (Vector's `json` codec) or `log` (Fluent Bit's `format json_lines`). |

- `Authorization: Bearer <token>`, with the `ingest` scope.
- `?server=<name>` names the server, for lines that carry no hostname, such as a container's own output. Give each Postfix pod its own name.
- `Content-Encoding: gzip` is accepted.
- At most 8 MiB per request.
- Each line still needs its timestamp, in a format Mailinois reads: classic syslog, RFC 5424, rsyslog's file format, or `boky/postfix`'s own output. A container's log line as Postfix wrote it is fine.

### Vector

```yaml title="vector.yaml"
sources:
  postfix:
    type: kubernetes_logs
    extra_label_selector: "app.kubernetes.io/name=postfix"

transforms:
  postfix_only:
    type: filter
    inputs: [postfix]
    condition: '.kubernetes.container_name == "postfix"'

sinks:
  mailinois:
    type: http
    inputs: [postfix_only]
    uri: "http://mailinois.mailinois.svc:7480/api/v1/ingest?server=postfix"
    encoding:
      codec: text
    framing:
      method: newline_delimited
    compression: gzip
    auth:
      strategy: bearer
      token: "${MAILINOIS_TOKEN}"
    batch:
      max_bytes: 4000000 # under Mailinois's 8 MiB per request
```

Vector's `http` sink batches up to 10 MB by default, over Mailinois's limit, so keep `batch.max_bytes` below 8 MiB.

### Fluent Bit

```yaml title="fluent-bit.yaml"
pipeline:
  inputs:
    - name: tail
      path: /var/log/containers/postfix-*_mailinois_postfix-*.log # <pod>_<namespace>_<container>-<id>.log
      multiline.parser: cri
      tag: postfix

  outputs:
    - name: http
      match: postfix
      host: mailinois.mailinois.svc
      port: 7480
      uri: /api/v1/ingest?server=postfix
      format: json_lines
      json_date_key: false
      compress: gzip
      header: Authorization Bearer ${MAILINOIS_TOKEN}
```

The `cri` parser puts each line as Postfix wrote it in `log`, which is where Mailinois looks.

## Check it works

The server appears on the Servers page when its first line arrives. If nothing does:

- `kubectl -n mailinois logs deploy/postfix -c mailinois-agent` says when the agent can't reach Mailinois (it spools meanwhile), and when Mailinois refuses its token.
- `kubectl -n mailinois logs mailinois-0` shows Mailinois starting, and the address it listens on.
- `postconf smtpd_milters milter_default_action`, run in the Postfix container, shows the milters Postfix actually uses.
