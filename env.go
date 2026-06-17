package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// maskEnv replaces every non-empty value with a placeholder, preserving keys,
// comments and blank lines — so the .env structure is visible without exposing
// secrets until the operator explicitly reveals them.
func maskEnv(content string) string {
	lines := strings.Split(content, "\n")
	for i, ln := range lines {
		t := strings.TrimSpace(ln)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		eq := strings.Index(ln, "=")
		if eq < 0 {
			continue
		}
		if strings.TrimSpace(ln[eq+1:]) == "" {
			continue
		}
		lines[i] = ln[:eq+1] + "********"
	}
	return strings.Join(lines, "\n")
}

// EnvFile manages the Nucleus central .env. Writes always back up the previous
// version first (there is otherwise no undo for a fat-fingered secret). Values
// are never written to the audit log.
type EnvFile struct {
	path string
}

func NewEnvFile() *EnvFile {
	return &EnvFile{path: getenv("NUCLEUS_ENV", "/nucleus/infra/.env")}
}

func (e *EnvFile) Path() string    { return e.path }
func (e *EnvFile) Available() bool  { return fileExists(e.path) }

func (e *EnvFile) Read() (string, error) {
	b, err := os.ReadFile(e.path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Write backs up the current file to <path>.bak.<unix> then writes the new
// content at 0600. Returns the backup path (empty if there was nothing to back up).
func (e *EnvFile) Write(content string) (backup string, err error) {
	if cur, rerr := os.ReadFile(e.path); rerr == nil {
		backup = fmt.Sprintf("%s.bak.%d", e.path, time.Now().Unix())
		if werr := os.WriteFile(backup, cur, 0o600); werr != nil {
			return "", fmt.Errorf("backup failed: %w", werr)
		}
	}
	if err := os.WriteFile(e.path, []byte(content), 0o600); err != nil {
		return "", err
	}
	return backup, nil
}
