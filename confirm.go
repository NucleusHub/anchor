package main

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// ConfirmStore issues one-time, short-lived tokens that bind a destructive
// action to a specific target. The server enforces confirmation here — UI
// prompts alone are bypassable.
type ConfirmStore struct {
	mu sync.Mutex
	m  map[string]confirmEntry
}

type confirmEntry struct {
	action string
	target string
	exp    time.Time
}

func NewConfirmStore() *ConfirmStore {
	return &ConfirmStore{m: map[string]confirmEntry{}}
}

func (c *ConfirmStore) Issue(action, target string) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	tok := hex.EncodeToString(b)
	c.mu.Lock()
	c.m[tok] = confirmEntry{action: action, target: target, exp: time.Now().Add(60 * time.Second)}
	c.mu.Unlock()
	return tok
}

// Check validates and consumes a token (one-time use).
func (c *ConfirmStore) Check(tok, action, target string) bool {
	if tok == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[tok]
	if !ok {
		return false
	}
	delete(c.m, tok)
	return e.action == action && e.target == target && time.Now().Before(e.exp)
}
