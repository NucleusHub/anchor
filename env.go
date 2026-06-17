package main

import (
	"fmt"
	"os"
	"time"
)

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
