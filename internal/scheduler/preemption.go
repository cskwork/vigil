package scheduler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"vigil/internal/model"
)

var errAgentPreempted = errors.New("agent job preempted by user request")

type activeAgentJob struct {
	jobID  int64
	cancel context.CancelCauseFunc
}

// PrioritizeAgentJob makes one READY AGENT_* job the next job handled by this
// scheduler's agent worker. It grants only that job a single budget bypass and
// cooperatively cancels an older in-process agent job, if one is active.
// Deterministic workers and their contexts are never referenced here.
func (s *Scheduler) PrioritizeAgentJob(ctx context.Context, jobID int64) error {
	var kind model.JobKind
	var state model.JobState
	err := s.st.DB().QueryRowContext(ctx,
		`SELECT kind, state FROM jobs WHERE id=? AND project_id=?`, jobID, s.project(),
	).Scan(&kind, &state)
	if err != nil {
		return fmt.Errorf("prioritize agent job %d: %w", jobID, err)
	}
	if !isAgentKind(kind) {
		return fmt.Errorf("prioritize agent job %d: %s is not an AGENT_* job", jobID, kind)
	}

	s.agentMu.Lock()
	// Cancel different local Agent work even if another loop process won the
	// race to lease this user job.
	if state == model.JobLeased {
		if s.activeAgent != nil && s.activeAgent.jobID != jobID {
			s.activeAgent.cancel(errAgentPreempted)
		}
		s.agentMu.Unlock()
		return nil
	}
	if state != model.JobReady {
		s.agentMu.Unlock()
		return fmt.Errorf("prioritize agent job %d: job is %s, want READY", jobID, state)
	}
	queued := false
	for _, id := range s.priorityAgent {
		if id == jobID {
			queued = true
			break
		}
	}
	if !queued {
		s.priorityAgent = append(s.priorityAgent, jobID)
	}
	if s.activeAgent != nil && s.activeAgent.jobID != jobID {
		s.activeAgent.cancel(errAgentPreempted)
	}
	s.agentMu.Unlock()
	s.wakeAgentWorker()
	return nil
}

func isAgentKind(kind model.JobKind) bool {
	switch kind {
	case model.JobAgentDiscover, model.JobAgentVerify, model.JobAgentRepair:
		return true
	default:
		return false
	}
}

func (s *Scheduler) wakeAgentWorker() {
	select {
	case s.agentWake <- struct{}{}:
	default:
	}
}

func (s *Scheduler) nextPriorityAgent() (int64, bool) {
	s.agentMu.Lock()
	defer s.agentMu.Unlock()
	if len(s.priorityAgent) == 0 {
		return 0, false
	}
	return s.priorityAgent[0], true
}

// nextPersistedUserRequest makes the budget bypass survive a loop restart.
// The claim atomically consumes priority 110 by lowering it to normal direct
// coverage priority before execution begins.
func (s *Scheduler) nextPersistedUserRequest(ctx context.Context) (int64, bool) {
	var id int64
	err := s.st.DB().QueryRowContext(ctx, `SELECT id FROM jobs
		WHERE project_id=? AND state='READY' AND scheduled_at<=? AND priority>=?
		AND kind IN (?,?,?,?) ORDER BY priority DESC, scheduled_at, id LIMIT 1`,
		s.project(), s.Now().UnixMilli(), model.PriorityUserRequest,
		string(model.JobAgentDiscover), string(model.JobAgentVerify), string(model.JobAgentRepair), string(model.JobAgentReproduce),
	).Scan(&id)
	return id, err == nil
}

func (s *Scheduler) consumePriorityAgent(jobID int64) {
	s.agentMu.Lock()
	defer s.agentMu.Unlock()
	for i, id := range s.priorityAgent {
		if id == jobID {
			s.priorityAgent = append(s.priorityAgent[:i], s.priorityAgent[i+1:]...)
			return
		}
	}
}

func (s *Scheduler) waitForWorker(ctx context.Context, idle time.Duration, agent bool) {
	t := time.NewTimer(idle)
	defer t.Stop()
	if !agent {
		select {
		case <-ctx.Done():
		case <-t.C:
		}
		return
	}
	select {
	case <-ctx.Done():
	case <-s.agentWake:
	case <-t.C:
	}
}
