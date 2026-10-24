// Command loadgen sends made-up Postfix log lines to a Mailinois syslog port over TCP at a steady rate, as
// several relays at once, for finding where Mailinois stops keeping up. Not part of a release.
//
//	go run ./tools/loadgen -addr 127.0.0.1:5514 -rate 20000 -duration 30s -relays 4
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:5514", "Mailinois's syslog address")
	rate := flag.Int("rate", 10000, "lines a second, across all relays")
	duration := flag.Duration("duration", 30*time.Second, "how long to send")
	relays := flag.Int("relays", 4, "TCP connections, each a relay with its own hostname")
	start := flag.Int("start", 0, "first message number, so runs don't reuse queue IDs")
	flag.Parse()

	var sent atomic.Int64
	var wg sync.WaitGroup
	deadline := time.Now().Add(*duration)
	for r := range *relays {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := net.Dial("tcp", *addr)
			if err != nil {
				log.Fatal(err)
			}
			defer conn.Close()
			w := bufio.NewWriterSize(conn, 256<<10)
			host := fmt.Sprintf("relay-%02d", r)
			perTick := float64(*rate) / float64(*relays) / 100 // lines per 10 ms
			owed, n := 0.0, *start+r*100_000_000
			tick := time.NewTicker(10 * time.Millisecond)
			defer tick.Stop()
			for now := range tick.C {
				if now.After(deadline) {
					break
				}
				for owed += perTick; owed >= 6; owed -= 6 {
					n++
					for _, l := range message(host, n, now) {
						body := l
						fmt.Fprintf(w, "%d %s", len(body), body)
					}
					sent.Add(6)
				}
				w.Flush()
			}
			w.Flush()
		}()
	}
	go func() {
		last := int64(0)
		for range time.Tick(time.Second) {
			cur := sent.Load()
			fmt.Printf("sent %d lines (%d/s)\n", cur, cur-last)
			last = cur
		}
	}()
	wg.Wait()
	fmt.Printf("done: %d lines\n", sent.Load())
}

// message is six lines for message n: client, Message-ID, sender, one delivery (one in ten deferred), removal.
func message(host string, n int, at time.Time) []string {
	qid := fmt.Sprintf("%010X", n)
	ts := at.UTC().Format("2006-01-02T15:04:05.000000Z07:00")
	providers := []string{"gmail.com", "outlook.com", "yahoo.com", "icloud.com", "example.org"}
	p := providers[n%len(providers)]
	line := func(prog string, pid int, msg string) string {
		return fmt.Sprintf("<22>1 %s %s %s %d - - %s", ts, host, prog, pid, msg)
	}
	status := "dsn=2.0.0, status=sent (250 2.0.0 OK)"
	if n%10 == 0 {
		status = "dsn=4.7.650, status=deferred (451 4.7.650 temporarily rate limited)"
	}
	return []string{
		line("postfix/smtpd", 100, qid+": client=app-1[10.1.0.4]"),
		line("postfix/cleanup", 101, qid+": message-id=<"+qid+"@acme.test>"),
		line("postfix/qmgr", 102, qid+": from=<hello@acme.test>, size=4200, nrcpt=1 (queue active)"),
		line("postfix/smtp", 103, fmt.Sprintf("%s: to=<user%d@%s>, relay=mx.%s[198.51.100.7]:25, delay=1, delays=0/0/0/1, %s", qid, n%5000, p, p, status)),
		line("postfix/smtp", 103, fmt.Sprintf("%s: to=<other%d@%s>, relay=mx.%s[198.51.100.7]:25, delay=1, delays=0/0/0/1, dsn=2.0.0, status=sent (250 2.0.0 OK)", qid, n%5000, p, p)),
		line("postfix/qmgr", 102, qid+": removed"),
	}
}
