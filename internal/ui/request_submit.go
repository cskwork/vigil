package ui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"vigil/internal/attach"
	"vigil/internal/evidence"
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
	if err != nil || (mediaType != "application/json" && mediaType != "multipart/form-data") {
		writeAPIError(w, http.StatusUnsupportedMediaType, "Content-Type은 application/json 또는 multipart/form-data여야 합니다")
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
	var uploads []attach.File
	if mediaType == "multipart/form-data" {
		var ok bool
		in.Situation, in.Site, uploads, ok = readMultipartRequest(w, r)
		if !ok {
			return
		}
	} else {
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
	}
	in.Situation = strings.TrimSpace(in.Situation)
	if in.Situation == "" && len(uploads) == 0 {
		writeAPIError(w, http.StatusBadRequest, "확인할 QA 상황을 입력하거나 화면 이미지·영상을 첨부해 주세요")
		return
	}
	if len(uploads) > 0 {
		if _, ok := s.requestSubmitter.(AttachmentRequestSubmitter); !ok {
			writeAPIError(w, http.StatusBadRequest, "이 실행기는 첨부 파일을 지원하지 않습니다")
			return
		}
	}
	if !s.allowRequestSubmission(r.RemoteAddr) {
		w.Header().Set("Retry-After", "2")
		writeAPIError(w, http.StatusTooManyRequests, "요청이 이미 접수 중입니다. 잠시 후 다시 시도해 주세요")
		return
	}

	// Uploads are stored under the evidence root before the request exists, so
	// the dashboard can show them later at /evidence/requests/<token>/.
	var stored []attach.Attachment
	var storeDir string
	if len(uploads) > 0 {
		storeDir, stored, err = s.storeUploads(r.Context(), uploads)
		if err != nil {
			if errors.Is(err, attach.ErrUnsupported) {
				writeAPIError(w, http.StatusUnsupportedMediaType, err.Error())
				return
			}
			writeAPIError(w, http.StatusInternalServerError, "첨부 파일을 저장하지 못했습니다")
			return
		}
	}
	discardUploads := func() {
		if storeDir != "" {
			_ = os.RemoveAll(storeDir)
		}
	}

	var featureID string
	var jobID int64
	if submitter, ok := s.requestSubmitter.(AttachmentRequestSubmitter); ok && (len(stored) > 0 || in.Site != "") {
		featureID, jobID, err = submitter.SubmitUserRequestWith(r.Context(), in.Situation, in.Site, stored)
	} else if submitter, ok := s.requestSubmitter.(SiteRequestSubmitter); ok {
		featureID, jobID, err = submitter.SubmitUserRequestAt(r.Context(), in.Situation, in.Site)
	} else if in.Site != "" {
		discardUploads()
		writeAPIError(w, http.StatusBadRequest, "이 실행기는 사이트 선택을 지원하지 않습니다")
		return
	} else {
		featureID, jobID, err = s.requestSubmitter.SubmitUserRequest(r.Context(), in.Situation)
	}
	if err != nil {
		if featureID == "" || jobID == 0 {
			discardUploads()
		}
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

// AttachmentRequestSubmitter accepts the stored uploads next to the prose. A
// submitter that implements it also owns site selection.
type AttachmentRequestSubmitter interface {
	SubmitUserRequestWith(ctx context.Context, situation, site string, attachments []attach.Attachment) (string, int64, error)
}

// storeUploads writes the uploaded files under <evidence>/requests/<token>/ and
// expands recordings into frames.
func (s *Server) storeUploads(ctx context.Context, files []attach.File) (string, []attach.Attachment, error) {
	var token [8]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", nil, err
	}
	// Same timestamp layout the evidence store parses, so uploads age out with
	// the rest of a request's evidence.
	dir := filepath.Join(s.evRoot, evidence.RequestsDirName, time.Now().UTC().Format("20060102T150405.000Z")+"-"+hex.EncodeToString(token[:]))
	stored, err := attach.Store(ctx, dir, files)
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, err
	}
	return dir, stored, nil
}

// readMultipartRequest reads the situation, the site and the uploaded files
// without buffering more than the attachment limits allow. It answers the
// client itself and returns ok=false when the body is unusable.
func readMultipartRequest(w http.ResponseWriter, r *http.Request) (situation, site string, files []attach.File, ok bool) {
	mr, err := r.MultipartReader()
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "첨부 형식을 읽지 못했습니다")
		return "", "", nil, false
	}
	total := 0
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "첨부를 읽는 중 오류가 발생했습니다")
			return "", "", nil, false
		}
		name := part.FormName()
		if part.FileName() == "" {
			value, err := io.ReadAll(io.LimitReader(part, maxRequestBodyBytes+1))
			part.Close()
			if err != nil || len(value) > maxRequestBodyBytes {
				writeAPIError(w, http.StatusRequestEntityTooLarge, "상황 설명이 너무 깁니다")
				return "", "", nil, false
			}
			switch name {
			case "situation":
				situation = string(value)
			case "site":
				site = strings.TrimSpace(string(value))
			}
			continue
		}
		if len(files) >= attach.MaxFiles {
			part.Close()
			writeAPIError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("첨부는 %d개까지 올릴 수 있습니다", attach.MaxFiles))
			return "", "", nil, false
		}
		data, err := io.ReadAll(io.LimitReader(part, attach.MaxVideoBytes+1))
		part.Close()
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "첨부를 읽는 중 오류가 발생했습니다")
			return "", "", nil, false
		}
		if len(data) == 0 {
			continue
		}
		total += len(data)
		if len(data) > attach.MaxVideoBytes || total > attach.MaxTotalBytes {
			writeAPIError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("첨부 전체 용량은 %d MB를 넘을 수 없습니다", attach.MaxTotalBytes>>20))
			return "", "", nil, false
		}
		files = append(files, attach.File{Name: part.FileName(), Data: data})
	}
	return situation, site, files, true
}
