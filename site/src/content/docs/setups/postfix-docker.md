---
title: Monitor Postfix in Docker
description: Get Postfix's log out of a boky/postfix or docker-mailserver container and into Mailinois with one mounted rsyslog file, capture headers through a milter that lets mail through when Mailinois is down, and import what docker logs already holds.
template: splash
---

In a container, Postfix's log sits one step further away than on a plain host. It goes to an rsyslog inside the container, then out as `docker logs`, which lasts only as long as the container: recreate it and the history goes with it.

Mailinois takes a copy as each line is written, from the container's own rsyslog, and keeps 30 days of it by default. Below is how to do that for [boky/postfix](https://github.com/bokysan/docker-postfix) and [docker-mailserver](https://github.com/docker-mailserver/docker-mailserver), both of which have been run against Mailinois.

## What does the container log?

On 19th October 2026 we sent two messages through docker-mailserver 16.0.1: one to a local mailbox, one to `example.com`, which publishes a null MX. Its log (trimmed to the lines that matter, long ones cut):

```txt
2026-10-19T10:41:11.395033+00:00 mail postfix/pickup[940]: 606A3966E030A: uid=0 from=<user@example.test>
2026-10-19T10:41:11.397551+00:00 mail postfix/qmgr[941]: 606A3966E030A: from=<user@example.test>, size=320, nrcpt=1 (queue active)
2026-10-19T10:41:11.436873+00:00 mail postfix/smtp[1104]: 606A3966E030A: to=<nobody@example.com>, relay=none, delay=0.06, delays=0.02/0/0.03/0, dsn=5.1.0, status=bounced …
2026-10-19T10:41:11.440100+00:00 mail postfix/bounce[1107]: 606A3966E030A: sender non-delivery notification: 6AE28966E030F
2026-10-19T10:41:11.440217+00:00 mail postfix/qmgr[941]: 606A3966E030A: removed
2026-10-19T10:41:11.460437+00:00 mail postfix/lmtp[1108]: 6AE28966E030F: to=<user@example.test>, relay=mail.example.test[/var/run/dovecot/lmtp], … status=sent …
```

One message is five or more lines from different Postfix daemons, joined only by the queue ID, and mixed in with every other message's lines. The bounce notice is a second message with its own queue ID. Mailinois joins each message's lines into one record with its recipients and attempts. In that test it stored three messages: the delivery, the bounce, and the notice.

## boky/postfix

boky/postfix logs through rsyslog and reads `/etc/rsyslog.d/*.conf`. Copy [`deploy/rsyslog/mailinois.conf`](../../docs/sending-logs/#rsyslog-on-a-postfix-host) from the repository next to your `compose.yaml` and mount it:

```yaml title="compose.yaml"
services:
  postfix:
    image: boky/postfix:5.1.0
    hostname: mail-1   # the server's name in Mailinois; without it, the container ID
    environment:
      ALLOWED_SENDER_DOMAINS: example.com
    volumes:
      - ./mailinois.conf:/etc/rsyslog.d/mailinois.conf:ro

  mailinois:
    image: ghcr.io/samjuk/mailinois:v0.1.0
    restart: unless-stopped
    ports:
      - "127.0.0.1:7480:7480"   # the dashboard, on this machine only
    volumes:
      - mailinois-data:/data

volumes:
  mailinois-data:
```

The file's `target="mailinois"` finds the `mailinois` service by name, so it works unchanged here. Syslog has no authentication, so don't publish port 5514; the `postfix` service reaches it over the Compose network.

This is what the repository's own `compose.yaml` runs, and the same file is mounted into a boky/postfix relay in production (as a Swarm config, with host networking).

### Capturing headers in boky/postfix

To see subjects and Message-IDs, Postfix hands each message to Mailinois's milter. Give Mailinois a config with capture on:

```yaml title="mailinois.yaml"
listen:
  milter: ":8892"
capture:
  mode: headers
```

Mount it at `/etc/mailinois.yaml` and run `serve -config /etc/mailinois.yaml` in the `mailinois` service. Then point Postfix at the milter. boky/postfix applies any `POSTFIX_` variable to `main.cf`:

```yaml
    environment:
      # Quoted: unquoted braces are a YAML mapping.
      POSTFIX_smtpd_milters: "{ inet:mailinois:8892, default_action=accept }"
      POSTFIX_non_smtpd_milters: "{ inet:mailinois:8892, default_action=accept }"
```

The braces give Mailinois's milter its own `default_action=accept`, so mail goes through when Mailinois is down. Don't rely on the global setting here. boky/postfix sets `milter_default_action = accept` only as part of its DKIM setup ([`functions.sh`](https://github.com/bokysan/docker-postfix/blob/master/image_root/scripts/functions.sh)). Without DKIM, Postfix's own default, `tempfail`, applies, and a Mailinois outage would hold mail. The [docker-mailserver test below](#capturing-headers-in-docker-mailserver) shows the difference.

With boky/postfix's DKIM signing on, its OpenDKIM milter is already in that list. Keep it first, so the captured headers include the signature: `"inet:localhost:8891, { inet:mailinois:8892, default_action=accept }"`. That is the order a production relay runs (with plain `inet:` entries, and DKIM's global `accept`). The braces form is the one we tested in docker-mailserver; it is Postfix's syntax, not the image's. [Capturing headers and bodies](../../docs/capture/) covers the modes, sizes and retention.

## docker-mailserver

docker-mailserver also logs through rsyslog inside the container, to `/var/log/mail/mail.log`. Its `docker logs` output is a `tail -F` of that file. The same rsyslog file works, mounted the same way:

```yaml title="compose.yaml"
services:
  mailserver:
    image: ghcr.io/docker-mailserver/docker-mailserver:16.0.1
    hostname: mail.example.com
    volumes:
      - ./mailinois.conf:/etc/rsyslog.d/mailinois.conf:ro
      - ./docker-data/dms/config/:/tmp/docker-mailserver/
      # …the rest of docker-mailserver's own compose.yaml
```

Add the `mailinois` service from the boky/postfix example to the same file. In our test the server appeared as `mail`, the first label of the container's hostname.

### Capturing headers in docker-mailserver

docker-mailserver appends `docker-data/dms/config/postfix-main.cf` to Postfix's `main.cf` at start ([its docs](https://docker-mailserver.github.io/docker-mailserver/latest/config/advanced/override-defaults/postfix/)). It already runs milters of its own, and which ones depends on what you have enabled, so check first:

```sh
docker exec mailserver postconf smtpd_milters non_smtpd_milters
```

Then add Mailinois's after them. With OpenDKIM and OpenDMARC, the defaults:

```txt title="docker-data/dms/config/postfix-main.cf"
smtpd_milters = $dkim_milter $dmarc_milter { inet:mailinois:8892, default_action=accept }
non_smtpd_milters = $dkim_milter { inet:mailinois:8892, default_action=accept }
```

The braces give Mailinois's milter its own `default_action` ([Postfix 3.0 and later](https://www.postfix.org/MILTER_README.html)): whatever the global `milter_default_action` says, mail goes through when Mailinois can't be reached. We checked both halves of that on 19th October 2026, with Postfix 3.10.13 in docker-mailserver:

- With Mailinois running, a message sent through the container arrived in Mailinois with its subject.
- With Mailinois stopped, `milter_default_action = tempfail`, and Mailinois's milter listed without the braces, Postfix refused the next message (`milter-reject: … 4.7.1 Service unavailable - try again later`) and held it.
- With the braces form, two messages were delivered, the new one and the held one, while Mailinois was still stopped. Postfix logged `connect to Milter service … Connection refused` as a warning for each.

docker-mailserver's own default is `accept`, so the braces only matter if you change it. They cost nothing, and they keep a later change from making Mailinois part of your delivery path.

### Without the rsyslog file

docker-mailserver's compose file mounts `/var/log/mail` from the host as `./docker-data/dms/mail-logs/`. When Mailinois is on another network, run [the agent](../../docs/install/#the-agent-on-a-postfix-host) on the host and follow that file over HTTPS:

```sh
mailinois agent -server https://mailinois.example.com -token-file /etc/mailinois/token \
  -file ./docker-data/dms/mail-logs/mail.log
```

## What is already in `docker logs`?

Whatever the container has logged since it was created can be loaded once, with either image:

```sh
docker logs mailserver 2>&1 | docker compose exec -T mailinois /mailinois import -server mail -
```

`import` writes straight to Mailinois's database, so it runs inside the `mailinois` container, with the same settings as the server. It skips lines that aren't syslog (docker-mailserver's own start-up messages, seven of 40 lines in our test) and lines it already has, so importing over the live stream changes nothing.

## Swarm and Kubernetes

On Swarm, mount the rsyslog file as a config ([Sending logs](../../docs/sending-logs/#bokypostfix-in-docker-or-swarm)). On Kubernetes, [Postfix on Kubernetes](../../setups/postfix-kubernetes/) covers a sidecar agent and log shippers.

## Is Mailinois the right tool here?

If all you want is a count of sent, deferred and bounced on a Grafana board, [postfix_exporter](https://github.com/kumina/postfix_exporter) (or [its maintained fork](https://github.com/Hsn723/postfix_exporter)) turns the same log into Prometheus metrics and is less to run. If you already ship container logs to [Grafana Loki](https://grafana.com/oss/loki/) or [Graylog](https://graylog.org/), you can search the raw lines there. Neither follows a message by queue ID across its lines and retries, which is the part Mailinois does: see [what that answers](../../use-cases/).
