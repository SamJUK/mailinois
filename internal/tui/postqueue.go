package tui

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Queued is one message in Postfix's queue as `postqueue -j` lists it.
type Queued struct {
	Queue      string `json:"queue_name"`
	ID         string `json:"queue_id"`
	Arrival    int64  `json:"arrival_time"`
	Size       int64  `json:"message_size"`
	Sender     string `json:"sender"`
	Recipients []struct {
		Address string `json:"address"`
		Reason  string `json:"delay_reason"`
	} `json:"recipients"`
}

func (q Queued) Arrived() time.Time { return time.Unix(q.Arrival, 0) }

// parseQueue reads postqueue -j's one object per line, skipping lines that aren't one.
func parseQueue(r io.Reader) []Queued {
	var out []Queued
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var q Queued
		if json.Unmarshal(sc.Bytes(), &q) == nil && q.ID != "" {
			out = append(out, q)
		}
	}
	return out
}

// QueueSource runs postqueue -j. Reading the queue changes nothing, so it's safe to run as often as the screen
// wants, but it's still capped at one run per few seconds.
type QueueSource struct {
	Bin string
}

func (s QueueSource) Read(ctx context.Context) ([]Queued, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.Bin, "-j")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("%s -j: %s", s.Bin, firstLine(msg))
		}
		return nil, fmt.Errorf("%s -j: %w", s.Bin, err)
	}
	return parseQueue(strings.NewReader(string(out))), nil
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return s
}

// ChooseQueue decides whether the Queue screen asks Postfix. mode is auto, on or off. Auto asks only when
// postqueue is installed and the log came from this machine: logs copied off another server, read on a laptop
// with its own Postfix, would otherwise show the laptop's queue as if it were theirs. note says why when it's off.
func ChooseQueue(mode string, logHosts []string) (src *QueueSource, note string) {
	if mode == "off" {
		return nil, "-queue off"
	}
	bin, err := exec.LookPath("postqueue")
	if err != nil {
		for _, p := range []string{"/usr/sbin/postqueue", "/usr/bin/postqueue"} {
			if _, err := os.Stat(p); err == nil {
				bin = p
				break
			}
		}
	}
	if bin == "" {
		return nil, "postqueue isn't installed here"
	}
	if mode == "on" {
		return &QueueSource{Bin: bin}, ""
	}
	host, _ := os.Hostname()
	if fromHere(host, logHosts) {
		return &QueueSource{Bin: bin}, ""
	}
	return nil, fmt.Sprintf("the log isn't from this machine (%s); -queue on to ask its Postfix anyway", short(host))
}

func fromHere(host string, logHosts []string) bool {
	for _, h := range logHosts {
		if strings.EqualFold(short(h), short(host)) {
			return true
		}
	}
	return false
}

func short(host string) string {
	h, _, _ := strings.Cut(host, ".")
	return h
}
