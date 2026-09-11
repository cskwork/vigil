package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"vigil/internal/config"
	"vigil/internal/model"
	"vigil/internal/store"
)

// lokiAdapter (loki) queries a Grafana-proxied Loki datasource
// (POST <base>/api/ds/query, Basic auth from env vars read at call time), groups the
// returned lines by signature and emits one Kind=log event per signature seen at
// least min_count times in the window. A signature is delivered once per hour
// bucket; the marker "ingest:loki:<ref>:<bucket>" lives in scheduler_state.
// Network and auth errors are logged and yield no events: the loop never crashes on a source.
type lokiAdapter struct {
	st   *store.Store
	cfg  config.LokiDiscovery
	sig  *regexp.Regexp
	http *http.Client
	now  func() time.Time
}

const (
	lokiSamplesPerSignature = 5
	lokiMaxSignatureLen     = 160
)

func newLokiAdapter(cfg *config.Config, st *store.Store) *lokiAdapter {
	l := &lokiAdapter{
		st:   st,
		cfg:  cfg.Discovery.Loki,
		http: &http.Client{Timeout: 30 * time.Second},
		now:  func() time.Time { return time.Now().UTC() },
	}
	re, err := regexp.Compile(pick(l.cfg.Signature, config.DefaultLokiSignature))
	if err != nil {
		re = regexp.MustCompile(config.DefaultLokiSignature)
	}
	l.sig = re
	if l.cfg.Window.Duration <= 0 {
		l.cfg.Window.Duration = time.Hour
	}
	if l.cfg.MinCount <= 0 {
		l.cfg.MinCount = 3
	}
	if l.cfg.MaxLines <= 0 {
		l.cfg.MaxLines = 500
	}
	if l.cfg.MaxEventsPerPoll <= 0 {
		l.cfg.MaxEventsPerPoll = 5
	}
	return l
}

func (l *lokiAdapter) Name() string { return AdapterLoki }

func (l *lokiAdapter) Poll(ctx context.Context) ([]model.FeatureEvent, error) {
	due, err := pollDue(ctx, l.st, l.Name(), l.cfg.PollInterval.Duration, l.now())
	if err != nil || !due {
		return nil, err
	}
	events := l.collect(ctx)
	return deliverOnceBy(ctx, l.st, events, func(ev model.FeatureEvent) string {
		return "ingest:loki:" + ev.Ref + ":" + strings.TrimPrefix(ev.ShippedSHA, "loki:"+ev.Ref+":")
	})
}

func (l *lokiAdapter) History(ctx context.Context, limit int) ([]model.FeatureEvent, error) {
	return newestOldestFirst(l.collect(ctx), limit), nil
}

// collect queries Loki and groups lines; every failure is logged and yields nil.
func (l *lokiAdapter) collect(ctx context.Context) []model.FeatureEvent {
	email, password := os.Getenv(l.cfg.EmailEnv), os.Getenv(l.cfg.PasswordEnv)
	if email == "" || password == "" {
		log.Printf("ingest[%s]: credentials missing: set %s and %s", l.Name(), l.cfg.EmailEnv, l.cfg.PasswordEnv)
		return nil
	}
	lines, err := l.query(ctx, email, password)
	if err != nil {
		log.Printf("ingest[%s]: query failed, no events this poll: %v", l.Name(), err)
		return nil
	}
	now := l.now()
	groups := groupBySignature(lines, l.sig, l.cfg.MinCount)
	if len(groups) > l.cfg.MaxEventsPerPoll {
		log.Printf("ingest[%s]: %d signature(s) reached min_count; keeping the top %d by count, %d dropped this poll",
			l.Name(), len(groups), l.cfg.MaxEventsPerPoll, len(groups)-l.cfg.MaxEventsPerPoll)
		groups = groups[:l.cfg.MaxEventsPerPoll]
	}
	var events []model.FeatureEvent
	for _, g := range groups {
		ev := l.event(g, now)
		stamp(&ev, l.Name())
		events = append(events, ev)
	}
	return events
}

// lokiLine is one row of a Loki frame after the promtail JSON wrapper is unwrapped.
type lokiLine struct {
	Text   string
	At     time.Time
	Labels map[string]string
}

// lokiQueryURL accepts the Grafana root or the full /api/ds/query URL.
func lokiQueryURL(base string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if strings.HasSuffix(base, "/api/ds/query") {
		return base
	}
	return base + "/api/ds/query"
}

// grafanaRange renders a duration as a Grafana relative time ("now-1h", "now-15m").
func grafanaRange(d time.Duration) string {
	switch {
	case d%time.Hour == 0:
		return fmt.Sprintf("now-%dh", int(d/time.Hour))
	case d%time.Minute == 0:
		return fmt.Sprintf("now-%dm", int(d/time.Minute))
	default:
		return fmt.Sprintf("now-%ds", int(d/time.Second))
	}
}

func (l *lokiAdapter) query(ctx context.Context, email, password string) ([]lokiLine, error) {
	body := map[string]any{
		"queries": []map[string]any{{
			"refId":      "A",
			"datasource": map[string]string{"uid": l.cfg.DatasourceUID, "type": "loki"},
			"expr":       l.cfg.Expr,
			"queryType":  "range",
			"direction":  "backward",
			"maxLines":   l.cfg.MaxLines,
		}},
		"from": grafanaRange(l.cfg.Window.Duration),
		"to":   "now",
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, lokiQueryURL(l.cfg.BaseURL), bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(email, password)
	resp, err := l.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("auth rejected (HTTP %d); check %s / %s", resp.StatusCode, l.cfg.EmailEnv, l.cfg.PasswordEnv)
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(bytes.TrimSpace(data[:min(len(data), 300)]))))
	}
	return parseLokiFrames(data)
}

// lokiResponse is the /api/ds/query shape: results.A.frames[*].{schema.fields, data.values}.
type lokiResponse struct {
	Results map[string]struct {
		Frames []struct {
			Schema struct {
				Fields []struct {
					Name string `json:"name"`
					Type string `json:"type"`
				} `json:"fields"`
			} `json:"schema"`
			Data struct {
				Values []json.RawMessage `json:"values"`
			} `json:"data"`
		} `json:"frames"`
	} `json:"results"`
}

// parseLokiFrames flattens every frame: the log text is the `Line` column (else the
// last string column), the time column holds epoch ms, `labels` is an object per row.
func parseLokiFrames(data []byte) ([]lokiLine, error) {
	var resp lokiResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	res, ok := resp.Results["A"]
	if !ok {
		return nil, nil
	}
	var out []lokiLine
	for _, fr := range res.Frames {
		lineIdx, lastString, timeIdx, labelIdx := -1, -1, -1, -1
		for i, f := range fr.Schema.Fields {
			switch {
			case f.Name == "Line":
				lineIdx = i
			case f.Type == "string":
				lastString = i
			case f.Type == "time" && timeIdx < 0:
				timeIdx = i
			case f.Name == "labels" && labelIdx < 0:
				labelIdx = i
			}
		}
		if lineIdx < 0 {
			lineIdx = lastString
		}
		if lineIdx < 0 || lineIdx >= len(fr.Data.Values) {
			continue
		}
		var texts []string
		if err := json.Unmarshal(fr.Data.Values[lineIdx], &texts); err != nil {
			return nil, fmt.Errorf("line column: %w", err)
		}
		var times []float64
		if timeIdx >= 0 && timeIdx < len(fr.Data.Values) {
			_ = json.Unmarshal(fr.Data.Values[timeIdx], &times)
		}
		var labels []map[string]any
		if labelIdx >= 0 && labelIdx < len(fr.Data.Values) {
			_ = json.Unmarshal(fr.Data.Values[labelIdx], &labels)
		}
		for i, raw := range texts {
			ln := lokiLine{Labels: map[string]string{}}
			if i < len(times) {
				ln.At = time.UnixMilli(int64(times[i])).UTC()
			}
			if i < len(labels) {
				for k, v := range labels[i] {
					if s, ok := v.(string); ok {
						ln.Labels[k] = s
					}
				}
			}
			ln.Text = raw
			// promtail wraps the line: {"ingest_time": ..., "message": ...}
			if strings.HasPrefix(strings.TrimSpace(raw), "{") {
				var wrapped struct {
					Message    string `json:"message"`
					IngestTime string `json:"ingest_time"`
				}
				if err := json.Unmarshal([]byte(raw), &wrapped); err == nil && wrapped.Message != "" {
					ln.Text = wrapped.Message
					if t, err := time.Parse(time.RFC3339Nano, wrapped.IngestTime); err == nil {
						ln.At = t.UTC()
					}
				}
			}
			out = append(out, ln)
		}
	}
	return out, nil
}

// signatureGroup is one distinct error signature with its sample lines.
type signatureGroup struct {
	Signature string
	Count     int
	Samples   []string
	Latest    time.Time
	Labels    map[string]string
}

var (
	urlRe   = regexp.MustCompile(`https?://\S+`)
	uuidRe  = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	hexRe   = regexp.MustCompile(`\b[0-9a-fA-F]{6,}\b`)
	digitRe = regexp.MustCompile(`\d`)
	numRe   = regexp.MustCompile(`\d+`)
	wsRe    = regexp.MustCompile(`\s+`)
)

// signatureOf is the first regexp match on the text (else its first line), with
// the volatile parts removed so thread names, request ids and URLs do not split
// one error into many signatures: URLs, uuids, hex runs (≥ 6, containing a
// digit) and digits are replaced, whitespace collapsed, and the result bounded.
func signatureOf(text string, sig *regexp.Regexp) string {
	s := ""
	if sig != nil {
		s = sig.FindString(text)
	}
	if s == "" {
		s = text
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[:i]
		}
	}
	return normalizeSignature(s)
}

func normalizeSignature(s string) string {
	s = urlRe.ReplaceAllString(s, "<url>")
	s = uuidRe.ReplaceAllString(s, "<uuid>")
	s = hexRe.ReplaceAllStringFunc(s, func(m string) string {
		if digitRe.MatchString(m) {
			return "<hex>"
		}
		return m // a plain word that happens to be hex letters (e.g. "decade")
	})
	s = numRe.ReplaceAllString(s, "#")
	s = strings.TrimSpace(wsRe.ReplaceAllString(s, " "))
	if len(s) > lokiMaxSignatureLen {
		s = bound(s, lokiMaxSignatureLen)
	}
	return s
}

// groupBySignature buckets lines by signature and keeps the ones reaching minCount, sorted by count desc.
func groupBySignature(lines []lokiLine, sig *regexp.Regexp, minCount int) []signatureGroup {
	groups := map[string]*signatureGroup{}
	var order []string
	for _, ln := range lines {
		s := signatureOf(ln.Text, sig)
		if s == "" {
			continue
		}
		g, ok := groups[s]
		if !ok {
			g = &signatureGroup{Signature: s, Labels: map[string]string{}}
			groups[s] = g
			order = append(order, s)
		}
		g.Count++
		if len(g.Samples) < lokiSamplesPerSignature {
			g.Samples = append(g.Samples, ln.Text)
		}
		if ln.At.After(g.Latest) {
			g.Latest = ln.At
		}
		for _, k := range []string{"file", "app", "pod_name"} {
			if v := ln.Labels[k]; v != "" && g.Labels[k] == "" {
				g.Labels[k] = v
			}
		}
	}
	var out []signatureGroup
	for _, s := range order {
		if g := groups[s]; g.Count >= minCount {
			out = append(out, *g)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return out
}

func (l *lokiAdapter) event(g signatureGroup, now time.Time) model.FeatureEvent {
	ref := shortHash(g.Signature)
	source := pick(g.Labels["file"], g.Labels["app"])
	summary := fmt.Sprintf("%d log lines in %s: %s", g.Count, l.cfg.Window.Duration, g.Signature)
	if source != "" {
		summary = fmt.Sprintf("[%s] %s", source, summary)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "signature: %s\ncount: %d (window %s, expr %s)\n", g.Signature, g.Count, l.cfg.Window.Duration, l.cfg.Expr)
	if len(g.Labels) > 0 {
		keys := make([]string, 0, len(g.Labels))
		for k := range g.Labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString("labels:")
		for _, k := range keys {
			fmt.Fprintf(&b, " %s=%s", k, g.Labels[k])
		}
		b.WriteByte('\n')
	}
	b.WriteString("\nsamples:\n")
	for _, s := range g.Samples {
		b.WriteString("- " + bound(strings.TrimSpace(s), 1500) + "\n")
	}
	shippedAt := g.Latest
	if shippedAt.IsZero() {
		shippedAt = now
	}
	return model.FeatureEvent{
		FeatureID:  "log-" + ref,
		ShippedSHA: "loki:" + ref + ":" + now.UTC().Format("2006-01-02T15"),
		ShippedAt:  shippedAt,
		Summary:    bound(summary, 300),
		Kind:       model.FeatureKindLog,
		Ref:        ref,
		Details:    bound(strings.TrimRight(b.String(), "\n"), MaxDetails),
	}
}
