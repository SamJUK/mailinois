---
title: Inspecting a node
description: mailinois inspect is an interactive terminal view of one mail server's logs, run over SSH. The overview, every message's story, Postfix's live queue and new deliveries as they arrive. No server, no config, read-only.
---

`mailinois inspect` is for the box in front of you.
SSH in, run it, and look around: what this node has done, why one message is stuck, what is queued right now.

```sh
mailinois inspect
```

With no paths it reads `/var/log/mail.log` and its rotated copies, then keeps reading as Postfix writes.
Like [`report`](../report/), it builds a throwaway database in the temp directory and deletes it when you quit.
It needs no config and no server, and it never changes anything.

|  | For | Lasts |
| --- | --- | --- |
| `mailinois report` | an answer to send someone: a file to attach, share or upload | one file |
| `mailinois serve` | watching mail all the time, for several servers, with alerts | a database |
| `mailinois inspect` | looking around one node, interactively | nothing; gone when you quit |

## Pointing it at logs

Each path is a file, a directory, a glob or `-` for stdin.

| Path | What it reads |
| --- | --- |
| nothing | `/var/log`, as a directory |
| a directory | its `mail.log` (or `maillog`) and their rotated copies, oldest first |
| a file | that file, plain or gzipped |
| a quoted glob | every match (`mail.err` too, if it matches), with a rotation set last and in order |
| `-` | standard input, as it arrives |

A directory's rotation set is logrotate's numbered files (`mail.log.4.gz`, `.3.gz`, `.2.gz`, `.1`, then `mail.log`) or dated ones (`maillog-20261025.gz`, then `maillog`).
`mail.err`, `mail.warn` and `mail.info` are left out: Debian's rsyslog writes those as copies of lines already in `mail.log`, so failures would count twice.
Name one as a file if you do want it.

```sh
mailinois inspect                                   # /var/log/mail.log and its rotated copies
mailinois inspect /srv/copied-logs/                 # logs copied off a server
mailinois inspect /var/log/mail.log                 # today's file only
mailinois inspect '/srv/relay-*/mail.log*'          # several servers' logs at once
journalctl -f -n all SYSLOG_FACILITY=2 -o short | mailinois inspect -
docker logs -f mail 2>&1 | mailinois inspect -server mail -
```

The top line names what it read (`/srv/copied-logs/: mail.log.4.gz … mail.log (5 files)`), so a wrong guess shows.
Only the newest file is followed, through logrotate's renames and copytruncate the same way the [agent](../sending-logs/) follows it.

We tested the directory, gzip and following on a real five-file Debian rotation set on 2nd November 2026, and the dated names only with made-up files in the tests: we haven't had a RHEL box with `dateext` to try.
The `journalctl` and `docker logs` lines were not tried with `inspect`; they are the same pipes [`report`](../report/) reads.

## What's on screen

Four tabs, and a message view over any of them.

- **Overview:** received, delivered, deferred, bounced, expired, rejected and still queued; delivery times; delivered and failed over the period; why mail waited, by provider and DSN code (Enter lists those messages); and each provider's outcomes.
- **Messages:** the newest 500 that match, `s` to cycle all, failed, deferred, queued and sent. Enter opens one.
- **Queue:** Postfix's own queue from `postqueue -j` every five seconds, with each recipient's delay reason. Each row is matched to its story in the log; one marked `not in log` has none, usually because it arrived before the log starts. A big queue is matched 200 rows a refresh, and the rest say `looking up` until then.
- **Tail:** delivery outcomes as they happen.

A message shows its sender, Message-ID, client and size, every recipient with its DSN, relay, delay and the remote server's answer, and its log lines.

This is the Messages tab at 100×24 (a few rows left out), from the made-up traffic in the tests:

```
mailinois inspect  read to the end  73 lines  13 Oct 09:00 to 18 Oct 11:00  internal/store/testdata…
 1 Overview   2 Messages   3 Queue   4 Tail
 Received        Status    From             To                      Domain           Queue ID
 18 Oct 11:00:00 sent      hello@acme.test  lee@gmail.example       gmail.example    2B3C4D5E6F
 18 Oct 10:00:00 sent      hello@acme.test  gina@gmail.example      gmail.example    0DFD162666
 18 Oct 09:09:00 bounced   hello@acme.test  old@example.net         example.net      0A1B2C3D4E
 18 Oct 09:08:00 deleted   hello@acme.test  jo@tailspin.example     tailspin.example 9C0D1E2F3A
 18 Oct 09:07:00 rejected  hello@acme.test  ivy@gmail.example       gmail.example    8B9C0D1E2F
 18 Oct 09:04:00 deferred  hello@acme.test  carol@gmail.example +2  gmail.example    4D5E6F7081
 18 Oct 09:02:00 bounced   hello@acme.test  nobody@example.net      example.net      2B3C4D5E6F
 13 Oct 09:00:00 expired   hello@acme.test  frank@example.org       example.org      5E6F708192
```

| Key | |
| --- | --- |
| `1` to `4`, Tab, Shift-Tab, ←&nbsp;→, `h` `l` | tabs |
| `/` | search, in the dashboard's [search language](../sites-and-search/): `to:ann@gmail.example status:bounced after:6h` |
| Enter, Esc | open, back (Esc also clears the search) |
| `j` `k`, ↑&nbsp;↓, wheel | move |
| Space, PgDn, Ctrl-F; `b`, PgUp, Ctrl-B | a page down, up (Ctrl-D, Ctrl-U: half a page) |
| `g` `G`, Home End | top, bottom |
| `s` | status, on Messages |
| `f` | pause or resume following |
| `?` | help |
| `q`, Ctrl-C | quit |

It needs a terminal of at least 80×24.
Colour follows `NO_COLOR`.

## Is the Queue tab Postfix's queue?

Only when the log is from the machine you run it on.
With `-queue auto` (the default) it runs `postqueue -j` only if `postqueue` is installed and the log's server name matches this machine's.
Otherwise a laptop with its own Postfix, reading logs copied off a relay, would show the laptop's queue as the relay's.

When it doesn't ask Postfix, the tab says `Queue as seen in the log` and lists the messages the log never saw leave, and why.
`-queue on` asks anyway; `-queue off` never asks.

`postqueue -j` only reads the queue, and works as any user unless `authorized_mailq_users` says otherwise.

## Can it change the queue?

No.
A message still queued shows the lines to run yourself:

```
postcat -q 4D5E6F7081      # the queued file
postqueue -i 4D5E6F7081    # try it now
postsuper -h 4D5E6F7081    # hold it
postsuper -d 4D5E6F7081    # delete it
```

It reads log files, runs `postqueue -j`, and writes only its temp directory, which goes on quit, Ctrl-C, `SIGTERM` or a dropped SSH session (`SIGHUP`).
It never listens on a port.

Log lines, addresses, subjects and remote servers' answers are shown as text.
Escape sequences in them (which could retitle the window, write the clipboard or redraw the screen) show as `\x1b`, and bidi controls as `\u202e`, so a crafted subject can't drive your terminal.

## Permissions

On Debian and Ubuntu `/var/log/mail.log` is `root:adm` and mode `0640`, so run it as root or as a member of `adm`.

## How long it takes to open

The same as [`report`](../report/): it reads every file before the first screen and shows its progress meanwhile.
The five-file set above (91,736 lines, a month of a small shop's mail) opened in 4.1 seconds on the Apple M4 Max laptop in [Sizing](../sizing/).
A relay keeping a year of weekly files, about 1.1 million lines at that rate, would take nearer 50.

The database is about 320 MB per million lines.
If `TMPDIR` is a tmpfs, that is memory; the loading screen says where it is.

## Flags

| Flag | |
| --- | --- |
| `-q` | The search to start with. |
| `-follow` | Keep reading the newest file as it grows. Default on. |
| `-queue` | `auto`, `on` or `off`: whether the Queue tab asks `postqueue -j`. |
| `-server` | The server name for lines that carry none, such as `docker logs` output. |
| `-tz` | The time zone of timestamps that carry none, as in `/var/log/mail.log`. Default: this machine's. |

## Other tools

- [`qshape`](https://www.postfix.org/QSHAPE_README.html) ships with Postfix and shows the queue by destination domain and age. For "which domain is the queue full of", it is quicker.
- `mailq` and `postqueue -p` list the queue without anything installed.
- [lnav](https://lnav.org/) browses any log file, with SQL over it. Better when the question isn't only about mail.
- [pflogsumm](http://jimsun.linxnet.com/postfix_contrib.html) by Jim Seymour writes a daily summary for email.
