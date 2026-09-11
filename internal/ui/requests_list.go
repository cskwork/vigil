package ui

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"vigil/internal/model"
)

// ---- manual QA requests (dashboard form / vigil request) ----------------------

type requestView struct {
	FeatureID   string   `json:"feature_id"`
	JobID       int64    `json:"job_id,omitempty"`
	Summary     string   `json:"summary"`
	RequestedAt string   `json:"requested_at"`
	Dir         string   `json:"dir"`
	Status      string   `json:"status"` // queued | preempting | budget_waiting | running | done | failed | stale | none
	Error       string   `json:"error,omitempty"`
	Decision    string   `json:"decision,omitempty"`
	Reason      string   `json:"reason,omitempty"`
	Evidence    string   `json:"evidence,omitempty"`
	Candidates  int      `json:"candidates"`
	ToolCalls   int      `json:"tool_calls"`
	Duration    string   `json:"duration,omitempty"`
	Screenshots []string `json:"screenshots"`
	VisitedURLs []string `json:"visited_urls,omitempty"`
	Gate        string   `json:"gate,omitempty"`
}

type requestsPayload struct {
	Requests []requestView `json:"requests"`
	Now      time.Time     `json:"now"`
}

// requests creates a bounded QA request or lists earlier requests and their
// persisted queue state.
func (s *Server) requests(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listRequests(w, r)
	case http.MethodPost:
		s.submitRequest(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		writeAPIError(w, http.StatusMethodNotAllowed, "지원하지 않는 요청 방식입니다")
	}
}

func isManualSource(source string) bool {
	return source == "manual" || source == "user" || source == "ui"
}

func (s *Server) listRequests(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := s.cfg.Project.ID
	feats, err := s.st.ListFeatures(ctx, p)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "QA 요청을 불러오지 못했습니다")
		return
	}
	jobs, err := s.st.ListJobs(ctx, p, nil, 1000)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "QA 요청 상태를 불러오지 못했습니다")
		return
	}
	latestJob := make(map[string]*model.Job)
	var leasedAgentID int64
	for _, job := range jobs {
		if job.State == model.JobLeased && isAgentJob(job.Kind) {
			leasedAgentID = job.ID
		}
		if job.FeatureID == "" {
			continue
		}
		old := latestJob[job.FeatureID]
		if old == nil || job.CreatedAt.After(old.CreatedAt) || (job.CreatedAt.Equal(old.CreatedAt) && job.ID > old.ID) {
			latestJob[job.FeatureID] = job
		}
	}
	agentBudgetUsed, _ := s.st.BudgetUsed(ctx, p, "agent", time.Hour)
	out := requestsPayload{Requests: []requestView{}}
	for _, f := range feats {
		if !isManualSource(f.Source) {
			continue
		}
		rv := requestView{FeatureID: f.ID, Summary: f.Summary, RequestedAt: fmtT(&f.ShippedAt), Status: "none", Screenshots: []string{}}
		if job := latestJob[f.ID]; job != nil {
			rv.JobID = job.ID
			rv.Error = job.LastError
			switch job.State {
			case model.JobReady:
				switch {
				case job.Priority >= model.PriorityUserRequest && leasedAgentID != 0 && leasedAgentID != job.ID:
					rv.Status = "preempting"
				case job.Priority < model.PriorityUserRequest && s.cfg.Budget.AgentTasksPerHour > 0 && agentBudgetUsed >= int64(s.cfg.Budget.AgentTasksPerHour):
					rv.Status = "budget_waiting"
				default:
					rv.Status = "queued"
				}
			case model.JobLeased:
				rv.Status = "running"
			case model.JobDone:
				rv.Status = "done"
			case model.JobFailed:
				rv.Status = "failed"
			}
		}
		s.fillRequestEvidence(&rv, filepath.Join(s.evRoot, "agent", f.ID))
		out.Requests = append(out.Requests, rv)
	}
	sort.Slice(out.Requests, func(i, j int) bool { return out.Requests[i].RequestedAt > out.Requests[j].RequestedAt })
	writeJSONPolled(w, r, &out, func() { out.Now = time.Now() })
}

// fillRequestEvidence attaches the newest agent run under root (the request's
// feature directory) to rv: result, gate verdict and screenshots.
func (s *Server) fillRequestEvidence(rv *requestView, root string) {
	newest, newestT := newestTranscriptIn(root)
	if newest == "" {
		return
	}
	rv.Dir = s.rel(newest)
	if rv.Status == "none" {
		rv.Status = "running"
	}
	if b, err := os.ReadFile(filepath.Join(newest, "agent-result.json")); err == nil {
		var res struct {
			Decision    string   `json:"decision"`
			Reason      string   `json:"reason"`
			Evidence    string   `json:"evidence"`
			Candidates  []string `json:"script_candidates"`
			ToolCalls   int      `json:"tool_calls"`
			Duration    int64    `json:"duration"`
			VisitedURLs []string `json:"visited_urls"`
		}
		_ = json.Unmarshal(b, &res)
		if rv.JobID == 0 {
			rv.Status = "done"
		}
		rv.Decision, rv.Reason, rv.Evidence = res.Decision, res.Reason, clip(res.Evidence, 6000)
		rv.Candidates, rv.ToolCalls, rv.VisitedURLs = len(res.Candidates), res.ToolCalls, res.VisitedURLs
		if res.Duration > 0 {
			rv.Duration = (time.Duration(res.Duration)).Round(time.Second).String()
		}
	} else if rv.JobID == 0 && s.now().Sub(newestT) > agentStaleAfter {
		rv.Status = "stale"
	}
	if b, err := os.ReadFile(filepath.Join(newest, "gate.json")); err == nil {
		rv.Gate = clip(string(b), 2000)
	}
	if files, err := os.ReadDir(newest); err == nil {
		for _, fe := range files {
			if strings.HasSuffix(fe.Name(), ".png") {
				rv.Screenshots = append(rv.Screenshots, rv.Dir+"/"+fe.Name())
			}
		}
	}
}

func isAgentJob(kind model.JobKind) bool {
	switch kind {
	case model.JobAgentDiscover, model.JobAgentVerify, model.JobAgentRepair:
		return true
	default:
		return false
	}
}
