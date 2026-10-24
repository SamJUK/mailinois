---
title: Postfix monitoring use cases
description: "What Mailinois answers about a Postfix relay, page by page: deferred mail, bounces, reading the logs, blocklists and DNS, app email that never arrived, and many client sites on one relay."
template: splash
---

Each page starts from a question someone asks about a Postfix server, shows what Postfix logs for it, and the search, alert or check in Mailinois that answers it.

| Question | Page |
| --- | --- |
| Why is this mail still in the queue? | [Why is Postfix deferring mail?](../use-cases/postfix-deferred-mail/) |
| Which addresses failed for good, and why? | [Tracking Postfix bounces](../use-cases/postfix-bounces/) |
| Is there something better than grep and pflogsumm? | [A Postfix log analyser that follows each message](../use-cases/postfix-log-analyzer/) |
| Is our sending IP on a blocklist? Is our SPF right? | [Blocklist and DNS monitoring for a mail server](../use-cases/blocklist-and-dns-monitoring/) |
| A customer says the sign-in code never came. Did it? | [Tracing app email through Postfix](../use-cases/transactional-email/) |
| We send for twenty clients. Can each see only theirs? | [Many client sites on one Postfix relay](../use-cases/many-sites-one-relay/) |

How to get the logs out of a particular setup (Debian or Ubuntu, Docker, mailcow, Kubernetes) is under [setups](../setups/). The [quick start](../docs/quick-start/) runs Mailinois locally with made-up traffic, if you'd rather click around first.
