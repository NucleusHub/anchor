package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// Config is Anchor's entire persisted state besides the audit log: the root
// password hash and a session-signing secret. Stored as JSON at 0600 on the
// anchor_data volume — independent of every Nucleus auth system.
type Config struct {
	PasswordHash  string `json:"passwordHash"`
	SessionSecret string `json:"sessionSecret"`
}

type Store struct {
	mu   sync.RWMutex
	path string
	cfg  Config
}

func dataDir() string {
	if d := os.Getenv("ANCHOR_DATA"); d != "" {
		return d
	}
	return "/data"
}

func NewStore() (*Store, error) {
	d := dataDir()
	if err := os.MkdirAll(d, 0o700); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(d, "config.json")}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) load() error {
	b, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil // unconfigured — first run
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, &s.cfg)
}

func (s *Store) save() error {
	b, err := json.MarshalIndent(s.cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// NeedsSetup reports whether no root password has been set yet (first run).
func (s *Store) NeedsSetup() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.PasswordHash == ""
}

// SetPassword stores the (already-hashed) root password and, on first use,
// mints a session secret. There is intentionally no reset path in software —
// recovery is deleting config.json on the volume via SSH (see ARCHITECTURE.md).
func (s *Store) SetPassword(hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg.PasswordHash = hash
	if s.cfg.SessionSecret == "" {
		b := make([]byte, 32)
		_, _ = rand.Read(b)
		s.cfg.SessionSecret = hex.EncodeToString(b)
	}
	return s.save()
}

func (s *Store) PasswordHash() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.PasswordHash
}
