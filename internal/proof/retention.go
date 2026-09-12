package proof

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// Prune removes expired raw evidence and terminal summaries. Only files named
// in this service's immutable manifests are eligible for removal.
func (s *Service) Prune(ctx context.Context) error {
	rows, e := s.Repo.DB.QueryContext(ctx, `SELECT body FROM proof_attempts WHERE state IN ('DONE','INTERRUPTED','CANCELLED')`)
	if e != nil {
		return e
	}
	var list []Attempt
	for rows.Next() {
		var b string
		if e = rows.Scan(&b); e != nil {
			rows.Close()
			return e
		}
		var a Attempt
		if e = json.Unmarshal([]byte(b), &a); e != nil {
			rows.Close()
			return e
		}
		list = append(list, a)
	}
	rows.Close()
	for _, a := range list {
		for _, cr := range a.Results {
			for _, ev := range cr.Evidence {
				if (ev.Artifact == "" && ev.Screenshot == "") || time.Since(ev.At) <= s.RawRetention {
					continue
				}
				for _, name := range []string{ev.Artifact, ev.Screenshot} {
					if name == "" {
						continue
					}
					path := filepath.Join(s.EvidenceDir, a.ID, filepath.Base(name))
					info, e := os.Lstat(path)
					if os.IsNotExist(e) {
						continue
					}
					if e != nil {
						return e
					}
					if info.Mode().IsRegular() {
						if e = os.Remove(path); e != nil {
							return e
						}
					}
				}

			}
		}
		if a.FinishedAt != nil && time.Since(*a.FinishedAt) > s.SummaryRetention {

			if _, e = s.Repo.DB.ExecContext(ctx, `DELETE FROM proof_attempts WHERE id=? AND state IN ('DONE','INTERRUPTED','CANCELLED')`, a.ID); e != nil {
				return e
			}
		}
	}
	return nil
}
