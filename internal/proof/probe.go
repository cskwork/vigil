package proof

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	_ "github.com/go-sql-driver/mysql"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

type Probe struct {
	DSNEnv string   `json:"dsn_env"`
	Query  string   `json:"query"`
	Keys   []string `json:"keys"`
	Fields []string `json:"fields"`
}
type ProbeRunner struct {
	Registry map[string]Probe
	mu       sync.Mutex
}

func (p *ProbeRunner) Read(ctx context.Context, name, entity string) ([]map[string]Value, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	def, ok := p.Registry[name]
	if !ok {
		return nil, fmt.Errorf("probe not registered")
	}
	q := strings.TrimSpace(def.Query)
	if !strings.HasPrefix(strings.ToUpper(q), "SELECT ") || strings.ContainsAny(q, ";#") || strings.Contains(q, "--") || strings.Count(q, "?") != 1 || len(def.Keys) == 0 || len(def.Fields) == 0 {
		return nil, fmt.Errorf("invalid read-only probe")
	}
	dsn := os.Getenv(def.DSNEnv)
	if dsn == "" {
		return nil, fmt.Errorf("DB credential unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	db, e := sql.Open("mysql", dsn)
	if e != nil {
		return nil, e
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	tx, e := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	rows, e := tx.QueryContext(ctx, q, entity)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	cols, e := rows.Columns()
	if e != nil {
		return nil, e
	}
	allowed := map[string]bool{}
	for _, k := range append(append([]string{}, def.Keys...), def.Fields...) {
		allowed[k] = true
	}
	if len(cols) != len(allowed) {
		return nil, fmt.Errorf("probe column contract mismatch")
	}
	for _, c := range cols {
		if !allowed[c] {
			return nil, fmt.Errorf("unexpected column")
		}
	}
	out := []map[string]Value{}
	for rows.Next() {
		if len(out) >= 100 {
			return nil, fmt.Errorf("probe row limit exceeded")
		}
		vals := make([]any, len(cols))
		ptr := make([]any, len(cols))
		for i := range vals {
			ptr[i] = &vals[i]
		}
		if e = rows.Scan(ptr...); e != nil {
			return nil, e
		}
		row := map[string]Value{}
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			b, e := json.Marshal(v)
			if e != nil {
				return nil, e
			}
			row[cols[i]] = Value{true, b}
		}
		out = append(out, row)
	}
	if e = rows.Err(); e != nil {
		return nil, e
	}
	seen := map[string]bool{}
	key := func(row map[string]Value) string {
		values := []Value{}
		for _, k := range def.Keys {
			values = append(values, row[k])
		}
		return encode(values)
	}
	for _, row := range out {
		k := key(row)
		if seen[k] {
			return nil, fmt.Errorf("duplicate probe entity key")
		}
		seen[k] = true
	}
	sort.Slice(out, func(i, j int) bool { return key(out[i]) < key(out[j]) })
	return out, nil
}
