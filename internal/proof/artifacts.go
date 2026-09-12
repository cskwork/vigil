package proof

import (
	"os"
	"path/filepath"
)

func persistCriterion(dir, attempt string, r *CriterionResult) {
	folder := filepath.Join(dir, attempt)
	dirErr := os.MkdirAll(folder, 0700)
	for i := range r.Evidence {
		ev := &r.Evidence[i]
		ev.Artifact = ev.ID + ".json"
		e := dirErr
		if e == nil {
			if info, statErr := os.Lstat(filepath.Join(folder, ev.Artifact)); statErr == nil && info.Mode().IsRegular() {
				ev.Details = nil
				continue
			}
		}
		if e == nil {
			e = os.WriteFile(filepath.Join(folder, ev.Artifact), []byte(encode(ev)), 0600)
		}
		ev.Details = nil
		if e != nil {
			ev.Artifact = ""
			ev.Status = "UNKNOWN"
			ev.Reason = "evidence artifact unavailable"
			r.Status = "UNKNOWN"
			r.Reason = "evidence artifact unavailable"
		}
	}
}
