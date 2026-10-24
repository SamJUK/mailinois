---
title: Postfix monitoring setups
description: How to get Postfix's log into Mailinois on Debian and Ubuntu, in Docker with boky/postfix or docker-mailserver, under mailcow, and on Kubernetes, with what each has been tested against.
template: splash
---

Mailinois reads Postfix's standard log lines, so the work in any setup is getting a copy of those lines to it. Each page below starts from where that setup keeps Postfix's log, gives the config to send it on, and says plainly what has been run and what hasn't.

| Where Postfix runs | Page | Run against Mailinois |
| --- | --- | --- |
| A Debian or Ubuntu host, with rsyslog or the journal only | [Postfix on Debian and Ubuntu](../setups/postfix-debian-ubuntu/) | The agent's unit, on Debian 13 |
| Docker: boky/postfix or docker-mailserver | [Postfix in Docker](../setups/postfix-docker/) | Both, end to end |
| mailcow | [A Postfix log dashboard for mailcow](../setups/mailcow/) | The syslog-ng hook, in mailcow's Postfix image |
| Kubernetes | [Postfix on Kubernetes](../setups/postfix-kubernetes/) | See [Kubernetes](../docs/kubernetes/) |

[Works with](../docs/works-with/) is the reference version: RHEL, iRedMail, Mail-in-a-Box and Modoboa too, with links to the source each answer came from.

[Use cases](../use-cases/) covers what Mailinois answers once the lines arrive: deferrals, bounces, blocklists and more.
