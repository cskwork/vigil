package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	"vigil/internal/config"
	"vigil/internal/model"
	"vigil/internal/store"
)

// execAdapter (exec) runs discovery.exec.command, which prints a JSON array of
// `{key, summary, details, at, routes, kind}` rows (examples/exec-events.example.json).
// A row is delivered once per (key, at); the marker "ingest:exec:<key>:<at>" lives in scheduler_state.
type execAdapter struct {
	st  *store.Store
	cfg config.ExecDiscovery
	now func() time.Time
}

func newExecAdapter(cfg *config.Config, st *store.Store) *execAdapter {
	e := &execAdapter{st: st, cfg: cfg.Discovery.Exec, now: func() time.Time { return time.Now().UTC() }}
	if e.cfg.Timeout.Duration <= 0 {
		e.cfg.Timeout.Duration = 60 * time.Second
	}
	return e
}

func (e *execAdapter) Name() string { return AdapterExec }

func (e *execAdapter) Poll(ctx context.Context) ([]model.FeatureEvent, error) {
	due, err := pollDue(ctx, e.st, e.Name(), e.cfg.PollInterval.Duration, e.now())
	if err != nil || !due {
		return nil, err
	}
	events, err := e.collect(ctx)
	if err != nil {
		return nil, err
	}
	return deliverOnceBy(ctx, e.st, events, func(ev model.FeatureEvent) string {
		return "ingest:exec:" + ev.Ref + ":" + strings.TrimPrefix(ev.ShippedSHA, "exec:"+ev.Ref+":")
	})
}

func (e *execAdapter) History(ctx context.Context, limit int) ([]model.FeatureEvent, error) {
	events, err := e.collect(ctx)
	if err != nil {
		return nil, err
	}
	return newestOldestFirst(events, limit), nil
}

// execRow is the stdout contract of discovery.exec.command.
type execRow struct {
	Key     string   `json:"key"`
	Kind    string   `json:"kind"`
	Summary string   `json:"summary"`
	Details string   `json:"details"`
	At      string   `json:"at"`
	Routes  []string `json:"routes"`
}

func (e *execAdapter) collect(ctx context.Context) ([]model.FeatureEvent, error) {
	if len(e.cfg.Command) == 0 {
		return nil, fmt.Errorf("ingest[exec]: discovery.exec.command is empty")
	}
	runCtx, cancel := context.WithTimeout(ctx, e.cfg.Timeout.Duration)
	defer cancel()
	cmd := exec.CommandContext(runCtx, e.cfg.Command[0], e.cfg.Command[1:]...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if runCtx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("ingest[exec]: %s timed out after %s", e.cfg.Command[0], e.cfg.Timeout.Duration)
		}
		return nil, fmt.Errorf("ingest[exec]: %s: %w: %s", e.cfg.Command[0], err, strings.TrimSpace(stderr.String()))
	}
	rows, err := parseExecRows(stdout.Bytes())
	if err != nil {
		return nil, fmt.Errorf("ingest[exec]: parse stdout: %w", err)
	}
	now := e.now()
	var events []model.FeatureEvent
	for _, r := range rows {
		if strings.TrimSpace(r.Key) == "" {
			log.Printf("ingest[%s]: skip row without key", e.Name())
			continue
		}
		ev := execEvent(r, now)
		stamp(&ev, e.Name())
		events = append(events, ev)
	}
	return events, nil
}

func parseExecRows(raw []byte) ([]execRow, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, nil
	}
	var rows []execRow
	return rows, json.Unmarshal(raw, &rows)
}

func execEvent(r execRow, now time.Time) model.FeatureEvent {
	key := strings.TrimSpace(r.Key)
	kind := strings.ToLower(strings.TrimSpace(r.Kind))
	switch kind {
	case model.FeatureKindIssue, model.FeatureKindLog:
	default:
		if kind != "" {
			log.Printf("ingest[exec]: row %s: unknown kind %q, using %s", key, r.Kind, model.FeatureKindLog)
		}
		kind = model.FeatureKindLog
	}
	at := strings.TrimSpace(r.At)
	shippedAt := now
	if t, err := time.Parse(time.RFC3339, at); err == nil {
		shippedAt = t.UTC()
	} else {
		at = now.Format(time.RFC3339)
	}
	return model.FeatureEvent{
		FeatureID:  "exec-" + safeID(key),
		ShippedSHA: "exec:" + key + ":" + at,
		ShippedAt:  shippedAt,
		Routes:     uniqueSorted(r.Routes),
		Summary:    strings.TrimSpace(r.Summary),
		Kind:       kind,
		Ref:        key,
		Details:    bound(strings.TrimSpace(r.Details), MaxDetails),
	}
}
