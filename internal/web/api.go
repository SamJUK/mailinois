package web

import (
	"bufio"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/SamJUK/mailinois/internal/auth"
	"github.com/SamJUK/mailinois/internal/capture"
	"github.com/SamJUK/mailinois/internal/store"
)

const maxIngest = 8 << 20

func (s *Server) api(mux *http.ServeMux) {
	read := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.Auth.API("read", h)) }
	read("GET /api/v1/summary", s.apiSummary)
	read("GET /api/v1/messages", s.apiMessages)
	read("GET /api/v1/messages/{id}", s.apiMessage)
	read("GET /api/v1/messages/{id}/eml", s.messageEML)
	read("GET /api/v1/queue", s.apiQueue)
	read("GET /api/v1/providers", s.apiProviders)
	read("GET /api/v1/servers", s.apiServers)
	read("GET /api/v1/ips", s.apiIPs)
	read("GET /api/v1/domains", s.apiDomains)
	read("GET /api/v1/alerts", s.apiAlerts)
	read("GET /api/v1/tail", s.apiTail)
	mux.Handle("POST /api/v1/ingest", s.Auth.API("ingest", http.HandlerFunc(s.apiIngest)))
}

// apiFilter reads period, site, server and q, within what the viewer may see.
func (s *Server) apiFilter(r *http.Request) (store.Filter, string) {
	q := r.URL.Query()
	period := q.Get("period")
	if period == "" {
		period = "24h"
	}
	return s.filter(auth.From(r.Context()), period, q.Get("server"), q.Get("site"), q.Get("q"), time.Now()), period
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func (s *Server) apiError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	slog.Error("api", "path", r.URL.Path, "err", err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "database error; see Mailinois's log"})
}

type period struct {
	Since time.Time `json:"since"`
	Until time.Time `json:"until"`
}

func (s *Server) apiSummary(w http.ResponseWriter, r *http.Request) {
	f, p := s.apiFilter(r)
	d, err := s.Store.Dashboard(r.Context(), f, step(p), s.lifetime())
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	writeJSON(w, 200, struct {
		Period period `json:"period"`
		store.Dashboard
	}{period{f.Since, f.Until}, d})
}

func (s *Server) apiMessages(w http.ResponseWriter, r *http.Request) {
	f, _ := s.apiFilter(r)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("cursor"))
	rows, err := s.Store.Messages(r.Context(), f, max(offset, 0), limit+1)
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	out := struct {
		Period     period             `json:"period"`
		Messages   []store.MessageRow `json:"messages"`
		NextCursor string             `json:"next_cursor,omitempty"`
	}{Period: period{f.Since, f.Until}, Messages: rows}
	if len(rows) > limit {
		out.Messages, out.NextCursor = rows[:limit], strconv.Itoa(offset+limit)
	}
	if out.Messages == nil {
		out.Messages = []store.MessageRow{}
	}
	writeJSON(w, 200, out)
}

func (s *Server) apiMessage(w http.ResponseWriter, r *http.Request) {
	m, c, err := s.loadMessage(r)
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	out := struct {
		*store.Message
		Capture *struct {
			Truncated   bool                 `json:"truncated"`
			Size        int64                `json:"size"`
			Headers     []capture.Header     `json:"headers"`
			Text        string               `json:"text,omitempty"`
			Attachments []capture.Attachment `json:"attachments,omitempty"`
			EML         string               `json:"eml,omitempty"`
		} `json:"capture,omitempty"`
	}{Message: m}
	if c != nil {
		p := capture.Parse(toCapture(c))
		out.Capture = &struct {
			Truncated   bool                 `json:"truncated"`
			Size        int64                `json:"size"`
			Headers     []capture.Header     `json:"headers"`
			Text        string               `json:"text,omitempty"`
			Attachments []capture.Attachment `json:"attachments,omitempty"`
			EML         string               `json:"eml,omitempty"`
		}{c.Truncated, c.Size, p.Headers, p.Text, p.Attachments, ""}
		if c.Body != nil {
			out.Capture.EML = "/api/v1/messages/" + r.PathValue("id") + "/eml"
		}
	}
	writeJSON(w, 200, out)
}

func (s *Server) apiQueue(w http.ResponseWriter, r *http.Request) {
	f, _ := s.apiFilter(r)
	queued, err := s.Store.Queue(r.Context(), f, s.lifetime(), 500)
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	reasons, err := s.Store.DeferralReasons(r.Context(), f)
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"queued": nonNil(queued), "deferral_reasons": nonNil(reasons)})
}

func (s *Server) apiProviders(w http.ResponseWriter, r *http.Request) {
	f, _ := s.apiFilter(r)
	stats, err := s.Store.Providers(r.Context(), f)
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"period": period{f.Since, f.Until}, "providers": stats})
}

func (s *Server) apiServers(w http.ResponseWriter, r *http.Request) {
	f, _ := s.apiFilter(r)
	servers, err := s.Store.Servers(r.Context(), f, s.lifetime())
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"servers": nonNil(servers)})
}

func (s *Server) apiIPs(w http.ResponseWriter, r *http.Request) {
	ips, err := s.Store.SendingIPs(r.Context(), s.Config.Checks.SendingIPs)
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ips": nonNil(ips)})
}

func (s *Server) apiDomains(w http.ResponseWriter, r *http.Request) {
	f, _ := s.apiFilter(r)
	writeJSON(w, 200, map[string]any{"domains": nonNil(s.health(r, f))})
}

func (s *Server) apiAlerts(w http.ResponseWriter, r *http.Request) {
	type rule struct {
		Name     string    `json:"name"`
		Metric   string    `json:"metric"`
		Above    float64   `json:"above"`
		State    string    `json:"state"`
		Value    float64   `json:"value"`
		Since    time.Time `json:"since"`
		Site     string    `json:"site,omitempty"`
		Provider string    `json:"provider,omitempty"`
		Server   string    `json:"server,omitempty"`
	}
	out := []rule{}
	if s.Alerts != nil {
		rules, err := s.Alerts.Rules(r.Context())
		if err != nil {
			s.apiError(w, r, err)
			return
		}
		for _, x := range rules {
			out = append(out, rule{x.Name, x.Metric, float64(x.Above), x.State.State, x.State.Value, x.State.Since, x.Site, x.Provider, x.Server})
		}
	}
	writeJSON(w, 200, map[string]any{"alerts": out})
}

// apiTail feeds the dashboard's live tail: outcomes after an event id.
func (s *Server) apiTail(w http.ResponseWriter, r *http.Request) {
	f, _ := s.apiFilter(r)
	f.Since, f.Until = time.Time{}, time.Time{}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	lines, err := s.Store.Tail(r.Context(), f, after, 20)
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"lines": nonNil(lines)})
}

// apiIngest takes what an agent ships (log lines and captures, gzip-compressed JSON), or what a log shipper
// sends: plain lines, or JSON lines.
func (s *Server) apiIngest(w http.ResponseWriter, r *http.Request) {
	if s.Pipeline == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "ingestion is off"})
		return
	}
	var body io.Reader = http.MaxBytesReader(w, r.Body, maxIngest)
	if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
		zr, err := gzip.NewReader(body)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad gzip"})
			return
		}
		body = http.MaxBytesReader(w, zr, maxIngest) // decompressed too: a small gzip can inflate to anything
	}
	wait := s.IngestWait
	if wait <= 0 {
		wait = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), wait)
	defer cancel()
	switch mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mediaType {
	case "text/plain", "application/x-ndjson":
		s.ingestLines(ctx, w, body, r.URL.Query().Get("server"), mediaType == "application/x-ndjson")
		return
	}
	// The agent's Batch, read here without importing the agent.
	var b struct {
		Server   string            `json:"server"`
		Lines    []string          `json:"lines"`
		Captures []capture.Message `json:"captures"`
	}
	if err := json.NewDecoder(body).Decode(&b); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) || strings.Contains(err.Error(), "unexpected EOF") {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "batch over 8 MiB"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad batch: " + err.Error()})
		return
	}
	for i, line := range b.Lines {
		if !s.Pipeline.FrameWait(ctx, line, b.Server) {
			busy(w, i, 0)
			return
		}
	}
	for i, c := range b.Captures {
		if !s.Pipeline.CaptureWait(ctx, store.Capture{QueueID: c.QueueID, MessageID: c.MessageID, Server: c.Server, At: c.At, Header: c.Header, Body: c.Body, Size: c.Size, Truncated: c.Truncated}) {
			busy(w, len(b.Lines), i)
			return
		}
	}
	writeJSON(w, http.StatusAccepted, map[string]int{"lines": len(b.Lines), "captures": len(b.Captures)})
}

// busy answers 503 when the queue stayed full: the sender keeps the batch and sends it whole again later, and
// whatever was taken this time is skipped then, as lines already stored are.
func busy(w http.ResponseWriter, lines, captures int) {
	w.Header().Set("Retry-After", "10")
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "busy: the database is behind; send this batch again later", "lines": lines, "captures": captures})
}

// ingestLines reads one log line per line (Vector's text codec), or one JSON object per line with the log
// line in "message" (Vector's json codec) or "log" (Fluent Bit's json_lines). server names the server for
// lines that carry no host, such as a container's output.
func (s *Server) ingestLines(ctx context.Context, w http.ResponseWriter, body io.Reader, server string, jsonLines bool) {
	lines, skipped := 0, 0
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		if jsonLines {
			var e struct {
				Message string `json:"message"`
				Log     string `json:"log"`
			}
			if json.Unmarshal([]byte(text), &e) != nil || e.Message+e.Log == "" {
				skipped++
				continue
			}
			text = e.Message
			if text == "" {
				text = e.Log
			}
		}
		if !s.Pipeline.FrameWait(ctx, text, server) {
			busy(w, lines, 0)
			return
		}
		lines++
	}
	if err := sc.Err(); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "batch over 8 MiB"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad lines: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]int{"lines": lines, "skipped": skipped})
}

func nonNil[T any](xs []T) []T {
	if xs == nil {
		return []T{}
	}
	return xs
}
