package ui

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// fmtT renders a timestamp in the server's local zone as "YYYY-MM-DD HH:MM:SS".
// The pages slice this string for the clock part, so the layout is a contract.
func fmtT(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

// clip truncates on a rune boundary so a Korean message never ends in a torn byte.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !isRuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func itoa(i int) string { return strconv.Itoa(i) }

func itoa64(i int64) string { return strconv.FormatInt(i, 10) }

func writeJSON(w http.ResponseWriter, v any) {
	writeJSONStatus(w, http.StatusOK, v)
}

// writeJSONBody encodes v after the caller has set its own headers.
func writeJSONBody(w http.ResponseWriter, v any) {
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeJSONPolled serves a polled read endpoint with a content ETag so an
// unchanged payload costs the browser a 304 and lets the page skip its
// re-render. v is hashed BEFORE stamp runs (stamp adds the wall clock, which
// would otherwise defeat the comparison); the stamped body is what gets sent.
func writeJSONPolled(w http.ResponseWriter, r *http.Request, v any, stamp func()) {
	stable, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sum := sha256.Sum256(stable)
	etag := `"` + hex.EncodeToString(sum[:12]) + `"`
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", etag)
	if strings.Contains(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	stamp()
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(buf.Bytes())
}

// primaryBrowserOf reads the `primary:` browser pin from a scenario YAML
// without a full parse; "" when the script has no pin.
func primaryBrowserOf(yamlText string) string {
	for _, line := range strings.Split(yamlText, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "primary:") {
			v := strings.TrimSpace(strings.TrimPrefix(t, "primary:"))
			if i := strings.Index(v, "#"); i >= 0 {
				v = strings.TrimSpace(v[:i])
			}
			return strings.Trim(v, `"'`)
		}
	}
	return ""
}
