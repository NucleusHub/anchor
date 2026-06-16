package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sort"
	"time"
)

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func (s *Server) setCookie(w http.ResponseWriter, tok string) {
	http.SetCookie(w, &http.Cookie{
		Name:     "anchor_session",
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.secure,
		MaxAge:   int((24 * time.Hour).Seconds()),
	})
}

type pwReq struct {
	Password string `json:"password"`
}

// GET /api/anchor/status — drives the UI gate (setup vs login vs app).
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	authed := false
	if c, err := r.Cookie("anchor_session"); err == nil && s.sessions.Valid(c.Value) {
		authed = true
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"needsSetup": s.store.NeedsSetup(),
		"authed":     authed,
	})
}

// POST /api/anchor/setup — first-run root password. Succeeds only while unconfigured.
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	if !s.store.NeedsSetup() {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "already configured"})
		return
	}
	var req pwReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Password) < 8 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "password must be at least 8 characters"})
		return
	}
	h, err := hashPassword(req.Password)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "hash failed"})
		return
	}
	if err := s.store.SetPassword(h); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "save failed"})
		return
	}
	s.audit.Log(AuditEntry{Event: "setup", IP: clientIP(r)})
	// Log the operator straight in.
	s.setCookie(w, s.sessions.Create())
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// POST /api/anchor/login
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.store.NeedsSetup() {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "not configured"})
		return
	}
	if s.limiter.Locked() {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many attempts — locked, try again shortly"})
		return
	}
	var req pwReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	if !verifyPassword(req.Password, s.store.PasswordHash()) {
		s.limiter.Fail()
		s.audit.Log(AuditEntry{Event: "login_fail", IP: clientIP(r)})
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid password"})
		return
	}
	s.limiter.Reset()
	s.setCookie(w, s.sessions.Create())
	s.audit.Log(AuditEntry{Event: "login_success", IP: clientIP(r)})
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// POST /api/anchor/logout
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("anchor_session"); err == nil {
		s.sessions.Destroy(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: "anchor_session", Value: "", Path: "/", MaxAge: -1})
	s.audit.Log(AuditEntry{Event: "logout", IP: clientIP(r)})
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// GET /api/anchor/services — discovered Nucleus containers, classified + sorted.
func (s *Server) handleServices(w http.ResponseWriter, r *http.Request) {
	cs, err := s.docker.ListManaged(r.Context())
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "docker unreachable: " + err.Error()})
		return
	}
	out := make([]Service, 0, len(cs))
	for _, c := range cs {
		out = append(out, toService(c))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].App != out[j].App {
			return out[i].App < out[j].App
		}
		if out[i].Role != out[j].Role {
			return out[i].Role < out[j].Role
		}
		return out[i].Name < out[j].Name
	})
	writeJSON(w, http.StatusOK, map[string]any{"services": out})
}

// GET /api/anchor/services/{id} — raw inspect, scoped to managed containers only.
func (s *Server) handleInspect(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	raw, err := s.docker.Inspect(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	var meta struct {
		Config struct {
			Labels map[string]string
		}
	}
	if json.Unmarshal(raw, &meta) != nil || meta.Config.Labels["nucleus.managed"] != "true" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "not a Nucleus-managed container"})
		return
	}
	s.audit.Log(AuditEntry{Event: "view_inspect", Target: id, IP: clientIP(r)})
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

// GET /api/anchor/services/{id}/logs
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	managed, err := s.docker.IsManaged(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if !managed {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "not a Nucleus-managed container"})
		return
	}
	logs, err := s.docker.Logs(r.Context(), id, 300)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	s.audit.Log(AuditEntry{Event: "view_logs", Target: id, IP: clientIP(r)})
	writeJSON(w, http.StatusOK, map[string]string{"logs": logs})
}

// GET /api/anchor/audit
func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"entries": s.audit.Recent(200)})
}

// GET /api/anchor/events — Server-Sent Events; emits "refresh" on Docker events
// so the UI re-fetches the service list live.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming unsupported"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ctx := r.Context()
	ch := make(chan struct{}, 1)
	go func() {
		_ = s.docker.Events(ctx, func() {
			select {
			case ch <- struct{}{}:
			default:
			}
		})
		close(ch)
	}()

	fmt.Fprint(w, "data: connected\n\n")
	flusher.Flush()

	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case _, open := <-ch:
			if !open {
				return
			}
			fmt.Fprint(w, "data: refresh\n\n")
			flusher.Flush()
		case <-ticker.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}
