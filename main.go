package main

import (
	"embed"
	"io/fs"
	"log"
	"net/http"
	"os"
	"time"
)

//go:embed web
var webFiles embed.FS

// Server holds Anchor's runtime dependencies.
type Server struct {
	store    *Store
	sessions *Sessions
	audit    *Audit
	docker   *Docker
	limiter  *RateLimiter
	secure   bool // set ANCHOR_SECURE=1 when served over HTTPS (Tailscale)
}

func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("anchor_session")
		if err != nil || !s.sessions.Valid(c.Value) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next(w, r)
	}
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		log.Printf("%s %s", r.Method, r.URL.Path)
	})
}

func main() {
	store, err := NewStore()
	if err != nil {
		log.Fatalf("anchor: store init: %v", err)
	}

	srv := &Server{
		store:    store,
		sessions: NewSessions(24 * time.Hour),
		audit:    NewAudit(),
		docker:   NewDocker(),
		limiter:  &RateLimiter{},
		secure:   os.Getenv("ANCHOR_SECURE") == "1",
	}

	webRoot, err := fs.Sub(webFiles, "web")
	if err != nil {
		log.Fatalf("anchor: embed: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/anchor/status", srv.handleStatus)
	mux.HandleFunc("POST /api/anchor/setup", srv.handleSetup)
	mux.HandleFunc("POST /api/anchor/login", srv.handleLogin)
	mux.HandleFunc("POST /api/anchor/logout", srv.handleLogout)
	mux.HandleFunc("GET /api/anchor/services", srv.requireAuth(srv.handleServices))
	mux.HandleFunc("GET /api/anchor/services/{id}", srv.requireAuth(srv.handleInspect))
	mux.HandleFunc("GET /api/anchor/services/{id}/logs", srv.requireAuth(srv.handleLogs))
	mux.HandleFunc("GET /api/anchor/events", srv.requireAuth(srv.handleEvents))
	mux.HandleFunc("GET /api/anchor/audit", srv.requireAuth(srv.handleAudit))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	mux.Handle("GET /", http.FileServer(http.FS(webRoot)))

	addr := os.Getenv("ANCHOR_ADDR")
	if addr == "" {
		addr = ":8888"
	}
	log.Printf("Anchor control plane listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, logRequests(mux)))
}
