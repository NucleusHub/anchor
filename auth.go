package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters — memory-hard, independent of Nucleus's bcrypt PINs.
const (
	argonTime    = 3
	argonMemory  = 64 * 1024 // 64 MiB
	argonThreads = 2
	argonKeyLen  = 32
	argonSaltLen = 16
)

// hashPassword returns a PHC-formatted argon2id string.
func hashPassword(pw string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// verifyPassword checks a password against a PHC argon2id string in constant time.
func verifyPassword(pw, encoded string) bool {
	parts := strings.Split(encoded, "$")
	// ["", "argon2id", "v=19", "m=...,t=...,p=...", salt, hash]
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var version, mem, t, p int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return false
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &mem, &t, &p); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, uint32(t), uint32(mem), uint8(p), uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// ── Sessions ─────────────────────────────────────────────────────────────────

// Sessions is an in-memory token store. Sessions do not survive a restart — for
// a break-glass tool that is acceptable (you simply re-authenticate).
type Sessions struct {
	mu  sync.Mutex
	m   map[string]time.Time // token -> expiry
	ttl time.Duration
}

func NewSessions(ttl time.Duration) *Sessions {
	return &Sessions{m: map[string]time.Time{}, ttl: ttl}
}

func (s *Sessions) Create() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	tok := hex.EncodeToString(b)
	s.mu.Lock()
	s.m[tok] = time.Now().Add(s.ttl)
	s.mu.Unlock()
	return tok
}

func (s *Sessions) Valid(tok string) bool {
	if tok == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.m[tok]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(s.m, tok)
		return false
	}
	return true
}

func (s *Sessions) Destroy(tok string) {
	s.mu.Lock()
	delete(s.m, tok)
	s.mu.Unlock()
}

// ── Login rate limiting ──────────────────────────────────────────────────────

// RateLimiter is a simple global lockout — single root user, so global is fine.
type RateLimiter struct {
	mu    sync.Mutex
	fails int
	until time.Time
}

const (
	maxFails = 5
	lockDur  = 30 * time.Second
)

func (r *RateLimiter) Locked() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return time.Now().Before(r.until)
}

func (r *RateLimiter) Fail() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fails++
	if r.fails >= maxFails {
		r.until = time.Now().Add(lockDur)
		r.fails = 0
	}
}

func (r *RateLimiter) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fails = 0
	r.until = time.Time{}
}
