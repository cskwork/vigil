package orchestrator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"vigil/internal/agent"
	"vigil/internal/model"
	"vigil/internal/store"
)

// MaxManualInstructionsLength bounds operator prose in the same character unit
// used by the dashboard textarea's maxlength attribute.
const MaxManualInstructionsLength = 8000

// ManualRequestValidationError marks an operator input error so HTTP callers
// can return 400 without misclassifying persistence or scheduler failures.
type ManualRequestValidationError struct{ err error }

func (e *ManualRequestValidationError) Error() string        { return e.err.Error() }
func (e *ManualRequestValidationError) Unwrap() error        { return e.err }
func (e *ManualRequestValidationError) InvalidRequest() bool { return true }

func invalidManualRequest(err error) error {
	return &ManualRequestValidationError{err: err}
}

// ManualRequest is the shared input used by the CLI's full request schema.
// Browser-facing callers should use SubmitUserRequest, which cannot grant mutation.
type ManualRequest struct {
	FeatureID        string
	Summary          string
	EntryURL         string
	Routes           []string
	Accounts         []string
	Instructions     string
	Mutation         string
	Locks            []string
	MaxToolCalls     int
	TimeoutMinutes   int
	MaxContinuations int
}

// ManualRequestResult is the persisted feature and Browser Agent job receipt.
type ManualRequestResult struct {
	FeatureID string `json:"feature_id"`
	JobID     int64  `json:"job_id"`
	Created   bool   `json:"created"`
}

// SubmitUserRequest registers one bounded dashboard request with safe server defaults.
// A plain string is deliberately the entire browser-facing write contract: callers
// cannot grant URLs, accounts, mutation, or execution-budget overrides.
func (o *Orchestrator) SubmitUserRequest(ctx context.Context, instructions string) (string, int64, error) {
	result, err := o.RegisterManualRequest(ctx, ManualRequest{
		Instructions:     instructions,
		Mutation:         string(model.MutationReadOnly),
		MaxToolCalls:     o.cfg.Agent.MaxTurns,
		TimeoutMinutes:   int(o.cfg.Agent.Timeout.Duration / time.Minute),
		MaxContinuations: o.cfg.Agent.Continuations,
	})
	if err != nil {
		return "", 0, err
	}
	return result.FeatureID, result.JobID, nil
}

// NormalizeManualRequest validates and canonicalizes a manual request without persisting it.
func (o *Orchestrator) NormalizeManualRequest(in ManualRequest) (ManualRequest, error) {
	in.FeatureID = strings.TrimSpace(in.FeatureID)
	in.Summary = strings.TrimSpace(in.Summary)
	in.EntryURL = strings.TrimSpace(in.EntryURL)
	in.Instructions = strings.TrimSpace(in.Instructions)
	in.Mutation = strings.TrimSpace(in.Mutation)
	if in.Instructions == "" {
		return in, invalidManualRequest(errors.New("manual request instructions are required"))
	}
	if !utf8.ValidString(in.Instructions) {
		return in, invalidManualRequest(errors.New("manual request instructions must be valid UTF-8"))
	}
	if utf8.RuneCountInString(in.Instructions) > MaxManualInstructionsLength {
		return in, invalidManualRequest(fmt.Errorf("manual request instructions exceed %d characters", MaxManualInstructionsLength))
	}
	if in.Mutation == "" {
		in.Mutation = string(model.MutationReadOnly)
	}
	switch model.Mutation(in.Mutation) {
	case model.MutationReadOnly, model.MutationReversible:
	case model.MutationDestructive:
		if !o.cfg.Policy.AllowDestructive {
			return in, invalidManualRequest(errors.New("destructive requests need policy.allow_destructive: true"))
		}
	default:
		return in, invalidManualRequest(fmt.Errorf("invalid mutation %q", in.Mutation))
	}
	if in.EntryURL != "" {
		u, err := url.Parse(in.EntryURL)
		if err != nil || !u.IsAbs() || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || !o.cfg.HostAllowed(u.Host) {
			return in, invalidManualRequest(fmt.Errorf("entry_url host is not in target.allowed_hosts: %s", in.EntryURL))
		}
	}
	if in.MaxToolCalls < 0 || in.TimeoutMinutes < 0 || in.MaxContinuations < 0 {
		return in, invalidManualRequest(errors.New("manual request execution limits cannot be negative"))
	}
	return in, nil
}

// RegisterManualRequest persists the feature and its AGENT_DISCOVER job using the existing schema.
func (o *Orchestrator) RegisterManualRequest(ctx context.Context, in ManualRequest) (*ManualRequestResult, error) {
	var err error
	in, err = o.NormalizeManualRequest(in)
	if err != nil {
		return nil, err
	}
	if in.FeatureID == "" {
		in.FeatureID, err = o.uniqueManualFeatureID(ctx)
		if err != nil {
			return nil, err
		}
	}
	token, err := randomToken(10)
	if err != nil {
		return nil, err
	}
	now := o.now().UTC()
	sha := "manual-" + token
	if in.Summary == "" {
		in.Summary = firstLine(in.Instructions)
	}
	ev := model.FeatureEvent{FeatureID: in.FeatureID, Status: "requested", ShippedSHA: sha, ShippedAt: now, Routes: in.Routes, Summary: in.Summary, Source: "manual"}
	req := &agent.Request{
		Summary: in.Summary, EntryURL: in.EntryURL, Routes: in.Routes, Accounts: in.Accounts,
		Instructions: in.Instructions, Mutation: in.Mutation, Locks: in.Locks,
		MaxToolCalls: in.MaxToolCalls, TimeoutMinutes: in.TimeoutMinutes, MaxContinuations: in.MaxContinuations,
	}
	payload := jobPayload{FeatureID: in.FeatureID, ShippedSHA: sha, Request: req}.String()
	id, created, err := o.st.RegisterManualRequest(ctx, o.cfg.Project.ID, ev, &model.Job{
		ProjectID: o.cfg.Project.ID, Kind: model.JobAgentDiscover, Priority: model.PriorityUserRequest,
		FeatureID: in.FeatureID, MaxAttempts: 1, Payload: payload,
	}, "request:"+in.FeatureID+":"+sha)
	if err != nil {
		return nil, err
	}
	return &ManualRequestResult{FeatureID: in.FeatureID, JobID: id, Created: created}, nil
}

func (o *Orchestrator) uniqueManualFeatureID(ctx context.Context) (string, error) {
	for i := 0; i < 5; i++ {
		token, err := randomToken(6)
		if err != nil {
			return "", err
		}
		id := "user-qa-" + o.now().UTC().Format("20060102T150405") + "-" + token
		if _, err := o.st.GetFeature(ctx, o.cfg.Project.ID, id); errors.Is(err, store.ErrNotFound) {
			return id, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", errors.New("could not allocate a unique manual request id")
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate manual request id: %w", err)
	}
	return hex.EncodeToString(b), nil
}
