package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// AuditEntry is one line of the append-only JSONL audit log. Secrets are never
// recorded.
type AuditEntry struct {
	TS     string `json:"ts"`
	Event  string `json:"event"`
	Target string `json:"target,omitempty"`
	Detail string `json:"detail,omitempty"`
	IP     string `json:"ip,omitempty"`
}

type Audit struct {
	mu   sync.Mutex
	path string
}

func NewAudit() *Audit {
	return &Audit{path: filepath.Join(dataDir(), "audit.jsonl")}
}

func (a *Audit) Log(e AuditEntry) {
	e.TS = time.Now().UTC().Format(time.RFC3339)
	a.mu.Lock()
	defer a.mu.Unlock()
	f, err := os.OpenFile(a.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(e)
	_, _ = f.Write(append(b, '\n'))
}

// Recent returns up to limit entries, newest first.
func (a *Audit) Recent(limit int) []AuditEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	f, err := os.Open(a.path)
	if err != nil {
		return []AuditEntry{}
	}
	defer f.Close()

	all := []AuditEntry{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		var e AuditEntry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			all = append(all, e)
		}
	}
	if len(all) > limit {
		all = all[len(all)-limit:]
	}
	for i, j := 0, len(all)-1; i < j; i, j = i+1, j-1 {
		all[i], all[j] = all[j], all[i]
	}
	return all
}
