---
title: Capturing headers and bodies
description: Keep a copy of each message's headers, or the whole message, through a Postfix milter. Off by default.
---

Out of the box Mailinois only sees log lines: addresses, queue IDs, Message-IDs and SMTP responses. To see headers or bodies, Postfix has to hand it a copy of each message, which it does through a milter. Captures are off until you turn them on.

## Modes

| `capture.mode` | What is kept |
| --- | --- |
| `off` | Nothing. The default. |
| `headers` | The message's headers. |
| `full` | Headers and body. |

`capture.max_size` caps the bytes kept per message (default `1048576`, 1 MiB). Anything past it is cut, and the capture is marked as cut.

## Turn it on in Mailinois

Give the milter an address and pick a mode:

```yaml
listen:
  milter: ":8892"
capture:
  mode: full
  max_size: 1048576
```

`MAILINOIS_MILTER=:8892` does the same as `listen.milter`. A capture mode needs a milter address: Mailinois will not start with one and not the other.

## Point Postfix at it

Add Mailinois to `main.cf`, after the milters you already have:

```ini title="/etc/postfix/main.cf"
smtpd_milters = <existing milters>, { inet:mailinois:8892, default_action=accept }
non_smtpd_milters = $smtpd_milters
milter_protocol = 6
milter_command_timeout = 5s
milter_content_timeout = 15s
```

With OpenDKIM on port 8891, for example, the first line reads `smtpd_milters = inet:localhost:8891, { inet:mailinois:8892, default_action=accept }`. Then reload Postfix with `postfix reload`.

What each line does:

- **`smtpd_milters`** sends mail that arrives over SMTP through the milters in order. List Mailinois after OpenDKIM, so it sees the `DKIM-Signature` header OpenDKIM adds and can verify it.
- **`default_action=accept`**, inside the braces, means that if Mailinois is down or does not answer, Postfix accepts the mail anyway. Mail keeps flowing. That message just has no capture. The braces form needs Postfix 3.0 or later ([MILTER_README](https://www.postfix.org/MILTER_README.html)).
- **`non_smtpd_milters = $smtpd_milters`** does the same for mail submitted locally, with `sendmail` or through the pickup directory.
- **`milter_protocol = 6`** sets the milter protocol version.

:::caution[Not the global milter_default_action]
Set `default_action` on Mailinois's milter, not `milter_default_action` for all of them. Postfix's default for every milter is `tempfail`: with only the global changed, a down Rspamd lets spam through and a down OpenDKIM sends mail unsigned, and with it left alone, a plain `inet:mailinois:8892` makes Postfix refuse mail while Mailinois is away (`4.7.1 Service unavailable`, which we saw happen in docker-mailserver). Some images set the global for you: `boky/postfix` sets it to `accept` only when its DKIM support is on. The braces form doesn't depend on any of that.
:::
- **`milter_command_timeout = 5s`** and **`milter_content_timeout = 15s`** are lower than Postfix's defaults (30s and 300s), so a stuck Mailinois costs each message seconds, not minutes.

Keep the milter port private. Only Postfix should be able to reach it.

## With the agent

When Postfix runs on another network, do not point its milter across the internet: every SMTP transaction would wait on that round trip. Run the milter in the [agent](../sending-logs/#the-agent-for-servers-on-other-networks) on the mail server instead:

```sh
mailinois agent -server https://mailinois.example.com -token <ingest token> \
  -file /var/log/mail.log -milter 127.0.0.1:8892 -capture full
```

Use `inet:127.0.0.1:8892` in `main.cf`. The agent takes the copy locally and sends it on to Mailinois over HTTPS.

## What the dashboard shows

A message with a capture shows:

- the parsed headers;
- the text part;
- the HTML part, in a sandboxed frame with scripts and remote content blocked, so opening it loads nothing from the sender;
- attachments, listed by name, type and size;
- DKIM verification of the captured message;
- a download of the `.eml`, for signed-in users.

`has:capture` in [search](../sites-and-search/#search) finds messages that have one, and `GET /api/v1/messages/{id}/eml` returns the `.eml` through the [API](../api/).

## Retention

Captures have their own retention, `retention.captures`, which defaults to `168h` (seven days) against `720h` for messages. When a capture expires, the message's log record stays until `retention.messages` removes it.

```yaml
retention:
  messages: 720h
  captures: 168h
```
