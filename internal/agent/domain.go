package agent

import (
	"os"
	"strings"
	"unicode/utf8"
)

// ReadDomainFile loads agent.domain_file for the task prompt. The text is bounded
// to MaxDomainFileBytes (cut on a rune boundary) and ends with DomainTruncatedMarker
// when cut. An empty path returns "" without an error.
func ReadDomainFile(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return BoundDomainRules(string(b)), nil
}

// BoundDomainRules applies the 12 KB bound to already-loaded rules text.
func BoundDomainRules(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= MaxDomainFileBytes {
		return s
	}
	cut := MaxDomainFileBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimSpace(s[:cut]) + DomainTruncatedMarker
}
