package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// sharedRoles are infrastructure components that multiple apps depend on —
// acting on them carries broader blast radius.
var sharedRoles = map[string]bool{
	"database": true, "cache": true, "object-store": true,
	"proxy": true, "registry": true, "auth": true,
}

// impact builds the confirmation warning. For shared infrastructure it names the
// actual dependent apps (derived from nucleus.depends labels across the stack),
// falling back to a generic warning if none are labeled yet.
func (s *Server) impact(ctx context.Context, action, name string, labels map[string]string) string {
	role := labels["nucleus.role"]
	app := labels["nucleus.app"]
	if sharedRoles[role] {
		svc := labels["com.docker.compose.service"]
		if svc != "" {
			if deps := s.dependents(ctx, svc); len(deps) > 0 {
				return fmt.Sprintf("'%s' (%s) is used by: %s. This %s will disrupt them.",
					name, role, strings.Join(deps, ", "), action)
			}
		}
		return fmt.Sprintf("'%s' is shared infrastructure (%s). %s may disrupt multiple Nucleus apps.", name, role, action)
	}
	if app != "" {
		return fmt.Sprintf("The '%s' app will be affected by %s.", app, action)
	}
	return fmt.Sprintf("This will %s the container.", action)
}

// dependents returns the distinct apps whose nucleus.depends label includes the
// given backing-service name (e.g. "mongo").
func (s *Server) dependents(ctx context.Context, service string) []string {
	list, err := s.docker.ListManaged(ctx)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, c := range list {
		for _, d := range strings.Split(c.Labels["nucleus.depends"], ",") {
			if strings.TrimSpace(d) != service {
				continue
			}
			who := c.Labels["nucleus.app"]
			if who == "" && len(c.Names) > 0 {
				who = strings.TrimPrefix(c.Names[0], "/")
			}
			if who != "" && !seen[who] {
				seen[who] = true
				out = append(out, who)
			}
		}
	}
	sort.Strings(out)
	return out
}

// confirmed implements the two-step guard for destructive actions. Without a
// valid one-time token it returns 428 with a freshly issued token + impact
// summary; the UI re-submits with that token to actually execute.
func (s *Server) confirmed(w http.ResponseWriter, r *http.Request, action, id, name string, labels map[string]string) bool {
	var body struct {
		ConfirmToken string `json:"confirmToken"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if s.confirm.Check(body.ConfirmToken, action, id) {
		return true
	}
	writeJSON(w, http.StatusPreconditionRequired, map[string]any{
		"needsConfirm": true,
		"confirmToken": s.confirm.Issue(action, id),
		"action":       action,
		"impact":       s.impact(r.Context(), action, name, labels),
	})
	return false
}

// resolve loads a container's labels and enforces the nucleus.managed scope.
// Returns (labels, name, ok). On failure it has already written the response.
func (s *Server) resolve(w http.ResponseWriter, r *http.Request, id string) (map[string]string, string, bool) {
	labels, err := s.docker.Labels(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return nil, "", false
	}
	if labels["nucleus.managed"] != "true" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "not a Nucleus-managed container"})
		return nil, "", false
	}
	name := labels["com.docker.compose.service"]
	if name == "" {
		name = labels["nucleus.app"]
	}
	return labels, name, true
}

// ── Non-destructive lifecycle ────────────────────────────────────────────────

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	s.lifecycle(w, r, "start")
}
func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	s.lifecycle(w, r, "restart")
}

func (s *Server) lifecycle(w http.ResponseWriter, r *http.Request, action string) {
	id := r.PathValue("id")
	if _, _, ok := s.resolve(w, r, id); !ok {
		return
	}
	var err error
	switch action {
	case "start":
		err = s.docker.Start(r.Context(), id)
	case "restart":
		err = s.docker.Restart(r.Context(), id)
	}
	if err != nil {
		s.audit.Log(AuditEntry{Event: action + "_error", Target: id, Detail: err.Error(), IP: clientIP(r)})
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	s.audit.Log(AuditEntry{Event: action, Target: id, IP: clientIP(r)})
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ── Destructive: stop ────────────────────────────────────────────────────────

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	labels, name, ok := s.resolve(w, r, id)
	if !ok {
		return
	}
	if !s.confirmed(w, r, "stop", id, name, labels) {
		return
	}
	if err := s.docker.Stop(r.Context(), id); err != nil {
		s.audit.Log(AuditEntry{Event: "stop_error", Target: id, Detail: err.Error(), IP: clientIP(r)})
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	s.audit.Log(AuditEntry{Event: "stop", Target: id, IP: clientIP(r)})
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ── Destructive: recreate / rebuild (via compose) ────────────────────────────

func (s *Server) handleRecreate(w http.ResponseWriter, r *http.Request) {
	s.composeAction(w, r, "recreate", s.compose.Recreate)
}
func (s *Server) handleRebuild(w http.ResponseWriter, r *http.Request) {
	s.composeAction(w, r, "rebuild", s.compose.Rebuild)
}

func (s *Server) composeAction(w http.ResponseWriter, r *http.Request, action string, fn func(context.Context, ComposeTarget) (string, error)) {
	id := r.PathValue("id")
	labels, name, ok := s.resolve(w, r, id)
	if !ok {
		return
	}
	// Derive project + files from the container's own compose labels, so we act
	// against exactly the stack it was created with (dev or prod) — never an
	// assumed file.
	t, err := s.compose.Target(labels)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error() + "; cannot " + action})
		return
	}
	if miss := s.compose.MissingFile(t); miss != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "compose file not accessible inside Anchor: " + miss +
				" — set NUCLEUS_REPO to the repo's host path (see deploy/README.md)",
		})
		return
	}
	if !s.confirmed(w, r, action, id, name, labels) {
		return
	}
	out, err := fn(r.Context(), t)
	if err != nil {
		s.audit.Log(AuditEntry{Event: action + "_error", Target: t.Service, Detail: err.Error(), IP: clientIP(r)})
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error(), "output": out})
		return
	}
	s.audit.Log(AuditEntry{Event: action, Target: t.Service, IP: clientIP(r)})
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "output": out})
}

// ── .env editing ─────────────────────────────────────────────────────────────

func (s *Server) handleEnvGet(w http.ResponseWriter, r *http.Request) {
	if !s.env.Available() {
		writeJSON(w, http.StatusOK, map[string]any{"available": false, "path": s.env.Path(), "content": "", "revealed": false})
		return
	}
	content, err := s.env.Read()
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	// Values are masked unless the operator explicitly asks to reveal — and a
	// reveal is itself an audited event.
	if r.URL.Query().Get("reveal") == "1" {
		s.audit.Log(AuditEntry{Event: "env_reveal", Target: s.env.Path(), IP: clientIP(r)})
		writeJSON(w, http.StatusOK, map[string]any{"available": true, "path": s.env.Path(), "content": content, "revealed": true})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"available": true, "path": s.env.Path(), "content": maskEnv(content), "revealed": false})
}

func (s *Server) handleEnvPut(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Content      string `json:"content"`
		ConfirmToken string `json:"confirmToken"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	if !s.confirm.Check(body.ConfirmToken, "env_edit", s.env.Path()) {
		writeJSON(w, http.StatusPreconditionRequired, map[string]any{
			"needsConfirm": true,
			"confirmToken": s.confirm.Issue("env_edit", s.env.Path()),
			"action":       "env_edit",
			"impact":       "Editing .env changes secrets/config for the whole Nucleus stack. Changes apply only after the affected services are recreated.",
		})
		return
	}
	backup, err := s.env.Write(body.Content)
	if err != nil {
		s.audit.Log(AuditEntry{Event: "env_edit_error", Detail: err.Error(), IP: clientIP(r)})
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	// Never log values — only that an edit happened, and where the backup went.
	s.audit.Log(AuditEntry{Event: "env_edit", Detail: "backup=" + backup, IP: clientIP(r)})
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"backup": backup,
		"note":   "Recreate the affected services to apply these changes.",
	})
}
