package web

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	gauges, err := s.Store.QueueGauges(ctx, s.lifetime())
	if err != nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	var b strings.Builder
	metric := func(name, kind, help string) { fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind) }

	metric("mailinois_queue_messages", "gauge", "Messages still in a Postfix queue.")
	for _, g := range gauges {
		fmt.Fprintf(&b, "mailinois_queue_messages{server=%q} %d\n", g.Server, g.Depth)
	}
	metric("mailinois_queue_oldest_seconds", "gauge", "Age of the oldest message in a queue, 0 when empty.")
	for _, g := range gauges {
		age := 0.0
		if g.Oldest != nil {
			age = time.Since(*g.Oldest).Seconds()
		}
		fmt.Fprintf(&b, "mailinois_queue_oldest_seconds{server=%q} %.0f\n", g.Server, age)
	}
	if ips, err := s.Store.SendingIPs(ctx, s.Config.Checks.SendingIPs); err == nil {
		metric("mailinois_dnsbl_listed", "gauge", "1 when the latest check found the IP on the blocklist.")
		for _, ip := range ips {
			for _, c := range ip.Checks {
				fmt.Fprintf(&b, "mailinois_dnsbl_listed{ip=%q,zone=%q} %d\n", c.IP, c.Zone, boolInt(c.Status == "listed"))
			}
		}
	}
	if s.Alerts != nil {
		if rules, err := s.Alerts.Rules(ctx); err == nil {
			metric("mailinois_alert_firing", "gauge", "1 while an alert is firing.")
			for _, rule := range rules {
				fmt.Fprintf(&b, "mailinois_alert_firing{alert=%q} %d\n", rule.Name, boolInt(rule.State.State == "firing"))
			}
		}
	}
	if p := s.Pipeline; p != nil {
		metric("mailinois_outcomes_total", "counter", "Delivery attempts and rejections ingested since start, by server and status.")
		for k, v := range p.Outcomes() {
			fmt.Fprintf(&b, "mailinois_outcomes_total{server=%q,status=%q} %d\n", k[0], k[1], v)
		}
		for _, c := range []struct {
			name, help string
			v          int64
		}{
			{"mailinois_syslog_frames_total", "Syslog frames received.", p.Frames.Load()},
			{"mailinois_syslog_unparsed_total", "Frames that were not syslog.", p.Unparsed.Load()},
			{"mailinois_lines_dropped_total", "Lines and captures dropped: buffer full or database away too long.", p.Dropped.Load()},
			{"mailinois_ingest_errors_total", "Failed writes to the database.", p.Errors.Load()},
			{"mailinois_captures_total", "Captured messages saved.", p.Captured.Load()},
		} {
			metric(c.name, "counter", c.help)
			fmt.Fprintf(&b, "%s %d\n", c.name, c.v)
		}
		metric("mailinois_lines_buffered", "gauge", "Lines waiting to be written.")
		fmt.Fprintf(&b, "mailinois_lines_buffered %d\n", p.Buffered())
	}
	fmt.Fprint(w, b.String())
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
