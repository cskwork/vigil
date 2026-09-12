package agent

import (
	"errors"
	"strings"
	"testing"
)

func TestProofTranscriptProviderFailureIsSafeAndTyped(t *testing.T) {
	cases := []struct {
		name, event string
		want        error
	}{
		{"quota exit zero", `{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"Codex error: The usage limit has been reached. token=secret-value"}}`, ErrProofQuota},
		{"auth", `{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"401 unauthorized api_key=secret-value"}}`, ErrProofAuth},
		{"provider", `{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"backend failed secret-value"}}`, ErrProofProvider},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw, err := parseProofTranscript([]byte(c.event))
			if !errors.Is(err, c.want) || len(raw) != 0 {
				t.Fatalf("got %q, %v", raw, err)
			}
			if strings.Contains(err.Error(), "secret-value") {
				t.Fatal("raw provider secret leaked")
			}
		})
	}
}

func TestProofTranscriptKeepsValidJSONSuggestion(t *testing.T) {
	raw, err := parseProofTranscript([]byte(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"{\"criteria\":[]}"}],"stopReason":"stop"}}`))
	if err != nil || string(raw) != `{"criteria":[]}` {
		t.Fatalf("%s %v", raw, err)
	}
}
