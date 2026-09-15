package ui

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type requestReceipt struct {
	FeatureID string `json:"feature_id"`
	JobID     int64  `json:"job_id"`
	Status    string `json:"status"`
	Warning   string `json:"warning,omitempty"`
}

const (
	// JSON escaping can use up to six bytes per character. Keep the transport
	// cap above the shared 8,000-character domain limit while still bounding reads.
	maxRequestBodyBytes   = 64 << 10
	requestSubmitInterval = 2 * time.Second
)

func (s *Server) submitRequest(w http.ResponseWriter, r *http.Request) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeAPIError(w, http.StatusUnsupportedMediaType, "Content-Type은 application/json이어야 합니다")
		return
	}
	if !hasSameOrigin(r) {
		writeAPIError(w, http.StatusForbidden, "다른 출처에서는 QA 요청을 제출할 수 없습니다")
		return
	}
	if s.requestSubmitter == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "이 서버에서는 QA 요청 접수가 꺼져 있습니다")
		return
	}

	var in struct {
		Situation string `json:"situation"`
		Site      string `json:"site,omitempty"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeAPIError(w, http.StatusRequestEntityTooLarge, "상황 설명이 너무 깁니다")
			return
		}
		writeAPIError(w, http.StatusBadRequest, "본문은 situation과 선택적인 site 필드를 가진 JSON이어야 합니다")
		return
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeAPIError(w, http.StatusBadRequest, "본문에는 JSON 값이 하나만 있어야 합니다")
		return
	}
	in.Situation = strings.TrimSpace(in.Situation)
	if in.Situation == "" {
		writeAPIError(w, http.StatusBadRequest, "확인할 QA 상황을 입력해 주세요")
		return
	}
	if !s.allowRequestSubmission(r.RemoteAddr) {
		w.Header().Set("Retry-After", "2")
		writeAPIError(w, http.StatusTooManyRequests, "요청이 이미 접수 중입니다. 잠시 후 다시 시도해 주세요")
		return
	}

	var featureID string
	var jobID int64
	if submitter, ok := s.requestSubmitter.(SiteRequestSubmitter); ok {
		featureID, jobID, err = submitter.SubmitUserRequestAt(r.Context(), in.Situation, in.Site)
	} else if in.Site != "" {
		writeAPIError(w, http.StatusBadRequest, "이 실행기는 사이트 선택을 지원하지 않습니다")
		return
	} else {
		featureID, jobID, err = s.requestSubmitter.SubmitUserRequest(r.Context(), in.Situation)
	}
	if err != nil {
		if featureID != "" && jobID != 0 {
			writeJSONStatus(w, http.StatusAccepted, requestReceipt{
				FeatureID: featureID, JobID: jobID, Status: "queued",
				Warning: "요청은 접수됐지만 기존 작업 중단을 확인하지 못했습니다. 대기열 우선순위로 진행합니다.",
			})
			return
		}
		var conflict interface{ BusyRequest() bool }
		if errors.As(err, &conflict) && conflict.BusyRequest() {
			writeAPIError(w, http.StatusConflict, err.Error())
			return
		}
		var invalid interface{ InvalidRequest() bool }
		if errors.As(err, &invalid) && invalid.InvalidRequest() {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "QA 요청을 접수하지 못했습니다")
		return
	}
	writeJSONStatus(w, http.StatusCreated, requestReceipt{FeatureID: featureID, JobID: jobID, Status: "queued"})
}

func (s *Server) allowRequestSubmission(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	if host == "" {
		host = "unknown"
	}
	now := s.now()
	s.submitMu.Lock()
	defer s.submitMu.Unlock()
	if last := s.lastSubmitByHost[host]; !last.IsZero() && now.Sub(last) < requestSubmitInterval {
		return false
	}
	if len(s.lastSubmitByHost) > 1024 {
		s.lastSubmitByHost = make(map[string]time.Time)
	}
	s.lastSubmitByHost[host] = now
	return true
}

func hasSameOrigin(r *http.Request) bool {
	if strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site") {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") {
		return false
	}
	scheme := r.URL.Scheme
	if scheme == "" {
		scheme = "http"
		if r.TLS != nil {
			scheme = "https"
		}
	}
	return strings.EqualFold(u.Scheme, scheme) && strings.EqualFold(canonicalHost(u.Host, u.Scheme), canonicalHost(r.Host, scheme))
}

func canonicalHost(host, scheme string) string {
	h, p, err := net.SplitHostPort(host)
	if err != nil {
		h, p = strings.Trim(host, "[]"), ""
	}
	if p == "" {
		if strings.EqualFold(scheme, "http") {
			p = "80"
		} else if strings.EqualFold(scheme, "https") {
			p = "443"
		}
	}
	return strings.ToLower(net.JoinHostPort(h, p))
}

func writeAPIError(w http.ResponseWriter, status int, message string) {
	writeJSONStatus(w, status, map[string]string{"error": message})
}

// SameOrigin exposes the dashboard write-origin policy to the admin router.
func SameOrigin(r *http.Request) bool { return hasSameOrigin(r) }
