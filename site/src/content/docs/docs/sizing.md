---
title: Sizing
description: How much mail one Mailinois handles, and where it breaks, measured. Ingest ceilings on a laptop and a 2-CPU box, dashboard and search times up to 2,000,000 messages a month, purges, disk use, PostgreSQL, and how to run the same tests.
---

One Mailinois on SQLite takes in tens of thousands of log lines a second, far more than a busy relay writes. What it can't do quickly is look back over a long period of a lot of mail: the 30-day views read every message in the period.

## What we measured

On 19th October 2026, on an Apple M4 Max laptop with nothing else running, with `internal/store/bench_test.go`. It writes made-up traffic as Postfix logs it: messages spread evenly over 30 days, five lines each (client, Message-ID, sender, delivery, removal), one in ten deferred once first. Lines go into SQLite in batches of 500, as the live pipeline writes them.

| Messages in 30 days | Log lines | Ingest | Dashboard, 24 hours | Dashboard, 30 days | 30 days, one server | Search, 30 days | Purging a day | SQLite file |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 20,000 | 102,000 | 34,132 lines/s | 1.5 ms | 37 ms | 53 ms | 7 ms | 0.04 s | 32 MB |
| 200,000 | 1,020,000 | 31,052 lines/s | 12 ms | 412 ms | 1.1 s | 77 ms | 0.4 s | 323 MB |
| 2,000,000 | 10,200,000 | 22,340 lines/s | 143 ms | 8.9 s | 46.9 s | 4.5 s | 6.1 to 6.5 s | 3.25 GB |

- The search was `to:outlook.com status:deferred`, the first page of 50.
- The purge is the hourly one's worst case, a day out of thirty, while ingest keeps writing. Its slowest batch meanwhile waited 0.04, 0.2 and 0.7 seconds.
- The file is about 1.6 KB a message, raw log lines included. Retention keeps it from growing past 30 days of mail.
- The "one server" view is the slowest case: that test's one server sent all the mail, so narrowing to it narrows nothing and joins every outcome to its message.

## Where it breaks

### Ingest

We sent made-up Postfix lines over syslog TCP from four relays at a steady rate for 20 seconds per step (`tools/loadgen`), and watched Mailinois's own metrics.

| Rate | Laptop, all cores | 2 CPUs and 2 GB, Linux |
| --- | --- | --- |
| 5,000 lines/s | | nothing dropped, 67 MB |
| 10,000 lines/s | nothing dropped, 84 MB | nothing dropped, 67 MB |
| 20,000 lines/s | nothing dropped, 87 MB | nothing dropped, 65 MB |
| 30,000 lines/s | | 11% dropped, 77 MB |
| 40,000 lines/s | 24% dropped, 99 MB | |
| 60,000 lines/s | 51% dropped, 100 MB | |

So about 30,000 lines a second on the laptop, and 25,000 on two of its cores. A relay sending a million messages a day writes five to six million lines, 60 to 70 a second on average. Memory stays flat because the queue in front of the database is bounded at 50,000 lines.

Past the ceiling, the queue fills. What happens next depends on how the lines arrive:

- **Syslog (TCP or UDP):** new lines are dropped, never waited for, so rsyslog and Postfix never back up. `mailinois_lines_dropped_total` counts them and `mailinois_lines_buffered` shows the queue ([Metrics](../metrics/)).
- **The agent, Vector, Fluent Bit (HTTP):** Mailinois waits up to 10 seconds for room, then answers `503` with `Retry-After`. The agent keeps the batch in its spool and sends it again; lines already stored are skipped.

### Long periods at volume

The 24-hour dashboard reads one day, so it stays quick however much history there is: 143 ms at 2,000,000 messages a month. The 30-day views don't: 8.9 seconds for everything, 47 seconds narrowed to one server, 4.5 seconds for a search. Hourly pre-computed totals would fix that; they aren't built yet. Until then, past a few hundred thousand messages a month, stick to the shorter periods.

### PostgreSQL

Ingest into PostgreSQL makes a round trip per statement, several per line. From a container on the same Docker network as PostgreSQL it took 3,303 lines a second; from this laptop into that container, 1,267. That's plenty for steady volume, but bursts above it fill the queue sooner than on SQLite. Batching those round trips is on the list.

## Run it yourself

```sh
go test ./internal/store -run '^$' -bench . -benchtime 1x
MAILINOIS_BENCH_MESSAGES=200000 go test ./internal/store -run '^$' -bench . -benchtime 1x -timeout 60m
go run ./tools/loadgen -addr 127.0.0.1:5514 -rate 20000 -duration 30s -relays 4
```

`MAILINOIS_TEST_DATABASE_URL=postgres://…` runs the benchmarks on PostgreSQL, against a database you don't mind emptying. Run them where Mailinois will run: a shared VPS core is slower than a laptop's.

## What we got wrong first

The load test found four things, all fixed in v0.3.1:

- **Ingest spent most of its time parsing SQL.** Every log line parsed its statements again: 63% of the CPU on Linux. Prepared once and reused, ingest went from about 11,000 lines a second to 34,000 on the laptop, and from 11,900 to 38,000 in a Linux container. A one-off [report](../report/) of 510,000 lines went from 53 seconds to 17.5.
- **Purging scanned every event for each recipient.** A foreign key had no index behind it. Purging one day of 200,000 messages took 20 minutes 31 seconds, holding the write lock throughout, so ingest stalled and failed. With the index, and the purge in chunks of 2,000 messages, that day takes 0.4 seconds.
- **The purge could starve.** SQLite has no queue for its write lock, and an ingest running flat out took it back every time it let go: at 2,000,000 messages the purge gave up with `SQLITE_BUSY`, and the database would have grown without limit. Mailinois now queues its own writers.
- **HTTP ingest said yes to lines it threw away.** With the queue full it dropped lines and still answered `202`, so the agent deleted batches it thought were stored. It answers `503` now.

Before that, in an earlier private build, the 30-day dashboard took 2,612 ms at 200,000 messages: every count visited a row per message, and the deferral count scanned the whole events table. Indexes that hold what the counts read, no join to messages when nothing filters by them, one sort for both percentiles and the latest failures read newest first brought it to 421 ms. The first version of that fix made the one-server view slower (1,853 ms to 3,084 ms), until Mailinois started refreshing SQLite's statistics with `PRAGMA optimize` at start and hourly.

## One instance

There is no clustering or autoscaling, on purpose. Following a message means tying each line to the latest message with its queue ID on that server, which needs one writer, and the ceilings above are well past what one relay, or several, write.

What keeps an outage from losing lines is in front of Mailinois. rsyslog's forwarding queue holds 10,000 lines while Mailinois is away, and the agent spools to disk. A restart is the recovery plan.

[PostgreSQL](../databases/) suits keeping the database on a server you already back up. Mind its lower ingest ceiling, above. Retention keeps either database's size steady: messages for 30 days and captures for 7 by default ([Configuration](../configuration/)).
