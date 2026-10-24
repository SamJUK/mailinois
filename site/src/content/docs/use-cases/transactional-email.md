---
title: Did the password reset email arrive? Tracing app email through Postfix
description: When a customer says the sign-in code or password reset never came, search Postfix's history by their address, read what the receiving server answered, and keep headers only so codes and reset links are never stored.
template: splash
---

"I never got the email" is the support ticket every app with a login gets. The app says it sent the password reset. The customer says it never came. Somewhere between the two is a Postfix relay that knows exactly what happened, in a log nobody on the support side can read.

That's the setup Mailinois first ran against: copilot-seo.com's Laravel app has sent its sign-in codes and password resets through a Postfix relay since 18th October 2026, and Mailinois has watched that relay since the 19th.

## How the app hands mail to Postfix

Most apps talk SMTP to the relay. In Laravel it's four lines of `.env` (copilot-seo's, with the host and address changed):

```ini title=".env"
MAIL_MAILER=smtp
MAIL_HOST=10.0.0.25
MAIL_PORT=587
MAIL_FROM_ADDRESS=hello@acme.example
```

WordPress does the same through a plugin such as [WP Mail SMTP](https://wordpress.org/plugins/wp-mail-smtp/). Without one, PHP's `mail()` calls the local `sendmail`, and Postfix logs the message as picked up from the web server's user:

```txt
postfix/pickup[118]: 7A8B9C0D1E: uid=33 from=<www-data>
```

Either way, Postfix logs each step under one queue ID, and Mailinois follows it.

## Finding the customer's message

Search by the address the customer gave you. Addresses are matched without case, so `Ann@Gmail.example` and `ann@gmail.example` are the same:

```txt
to:ann@gmail.example after:2d
```

Narrow it if they get a lot of mail from you:

```txt
to:ann@gmail.example subject:"password reset" after:2d
from:hello@acme.example to:ann@gmail.example -status:sent
```

Subjects are only there if Postfix logs them (a `header_checks` table with the line `/^Subject:/ INFO`) or headers are captured, below.

### Without running Mailinois

On the mail server, the same search works against the log you already have, with no service and no database:

```sh
mailinois report -q "to:ann@gmail.example" /var/log/mail.log.1 /var/log/mail.log
```

It writes one HTML file that opens straight onto Ann's messages: every attempt, the receiving server's answer, and the log lines, ready to attach to the ticket ([One-off reports](../../docs/report/)). `-o ann.md` makes Markdown for a chat instead.

For an exact match, log the Message-ID in your app when it sends. In Laravel the `MessageSent` event carries it (`$event->sent->getMessageId()`), and Mailinois searches it with `msgid:`. Then a support ticket can link straight to the message.

## Reading the answer

Open the message and its timeline shows every step: the app that handed it over, each delivery attempt, and the receiving server's reply. The status says which question you're actually answering:

| Status | What to tell the customer |
| --- | --- |
| `sent` | Their provider accepted it. The `250` reply is on the timeline. If it isn't in their inbox, look in spam: past this point Mailinois can't see. |
| `deferred` | Their provider said "not yet" and Postfix is retrying. The reply says why. See [why Postfix defers mail](../../use-cases/postfix-deferred-mail/). |
| `bounced` | Their provider refused it for good, often `5.1.1`: the address doesn't exist. Ask them to check it. See [tracking bounces](../../use-cases/postfix-bounces/). |
| `rejected` | Your own relay refused it (or a milter on it did). That's a problem on your side, not theirs. |

There's no message at all? Then it never reached Postfix, and the place to look is the app: a failed queue job, or an exception on send.

`rejected` deserves its own alert, because it means the app is producing mail the relay won't take (a sender domain the relay doesn't allow, for instance). The relay for copilot-seo.com runs this one:

```yaml
alerts:
  - name: Relay refusing app mail
    metric: rejected
    above: 5
    window: 1h
    notify: [ops]
```

## Why we capture headers only

Mailinois can keep a copy of each message through a Postfix milter, either the headers or the whole thing. For app mail, the body is the problem: it holds the sign-in code, the password reset link, the magic login URL. A store of full bodies would be a store of ways into your customers' accounts.

So the copilot-seo.com relay keeps headers only:

```yaml
capture:
  mode: headers
  max_size: 262144
```

Headers are enough to answer support questions: the subject, the `From` and `To`, the Message-ID, and the `DKIM-Signature`, which shows the message was signed before it left. Captures have their own retention (seven days by default), shorter than the log history.

If your mail carries nothing secret (order confirmations, newsletters), `mode: full` keeps the body as well and gives each message a **Download .eml** button. [Capturing headers and bodies](../../docs/capture/) covers both, and the three lines Postfix needs, including `milter_default_action = accept` so that Mailinois being down never stops mail.

## Giving support access without giving them everything

A support team needs to search for one customer's mail, not read the server's whole history. Two ways to keep it narrow:

- **A login limited to a site.** In password mode, each user can be limited to some [sites](../../docs/sites-and-search/#sites), so they only see those sending domains. See [Authentication](../../docs/authentication/#limiting-users-and-tokens-to-sites).
- **A read token for your own support tool.** Look the customer up from your admin panel instead:

```sh
curl -G -H "Authorization: Bearer $TOKEN" https://mailinois.example.com/api/v1/messages \
  --data-urlencode 'q=to:ann@gmail.example after:7d'
```

The [API reference](../../docs/api/) has the response shape, with each message's status and queue ID.
