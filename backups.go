package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// A backup is a per-run BUNDLE directory named <YYYYMMDD-HHMMSS> containing some
// of:
//   minio.tar.gz  — the whole MinIO data volume
//   mongo.gz      — mongodump --archive --gzip of the nucleus DB
// Restore rolls back MinIO + ONLY the Orbit collections from mongo.gz, so objects
// and Orbit's file metadata move together while other apps are left alone.
//
// Anchor can't `docker exec` (the socket-proxy sets EXEC=0), so both backup and
// restore drive short-lived helper containers via `docker run` (which the proxy
// allows): an alpine helper tars the MinIO volume, a mongo:4.4 helper runs
// mongodump/mongorestore against the live MongoDB (sharing its net namespace).

func backupDir() string {
	if d := os.Getenv("ANCHOR_BACKUP_DIR"); d != "" {
		return d
	}
	return "/backups"
}

// backupHostDir is the SAME directory as the host Docker daemon sees it — helper
// containers bind-mount it (a sibling's mount source resolves on the host).
func backupHostDir() string {
	if d := os.Getenv("ANCHOR_BACKUP_HOST_DIR"); d != "" {
		return d
	}
	return "/home/debian/backups/minio"
}

// restoreTargetDir is where the MinIO data volume is mounted INTO Anchor (rw).
func restoreTargetDir() string {
	if d := os.Getenv("ANCHOR_RESTORE_TARGET"); d != "" {
		return d
	}
	return "/restore/minio"
}

func minioVolume() string {
	if v := os.Getenv("ANCHOR_MINIO_VOLUME"); v != "" {
		return v
	}
	return "nucleus_minio_data"
}

func mongoDBName() string {
	if d := os.Getenv("ANCHOR_MONGO_DB"); d != "" {
		return d
	}
	return "nucleus"
}

// orbitCollections are the only MongoDB collections a restore touches — Orbit's
// file + folder metadata. Everything else in the DB is left untouched.
var orbitCollections = []string{"orbitfiles", "orbitfolders"}

// backupStamp guards against path traversal: a bundle is addressed only by its
// timestamp directory name.
var backupStamp = regexp.MustCompile(`^\d{8}-\d{6}$`)

type backupInfo struct {
	Name     string `json:"name"`     // the YYYYMMDD-HHMMSS bundle id
	Size     int64  `json:"size"`     // minio.tar.gz + mongo.gz, bytes
	Modified string `json:"modified"` // RFC3339 UTC
	HasMinio bool   `json:"hasMinio"`
	HasMongo bool   `json:"hasMongo"`
}

// GET /api/anchor/backups — list backup bundles, newest first.
func (s *Server) handleBackups(w http.ResponseWriter, r *http.Request) {
	dir := backupDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusOK, map[string]any{"dir": dir, "backups": []backupInfo{}})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	backups := []backupInfo{}
	for _, e := range entries {
		if !e.IsDir() || !backupStamp.MatchString(e.Name()) {
			continue
		}
		bdir := filepath.Join(dir, e.Name())
		var size int64
		var mod time.Time
		bi := backupInfo{Name: e.Name()}
		if fi, err := os.Stat(filepath.Join(bdir, "minio.tar.gz")); err == nil {
			bi.HasMinio = true
			size += fi.Size()
			mod = fi.ModTime()
		}
		if fi, err := os.Stat(filepath.Join(bdir, "mongo.gz")); err == nil {
			bi.HasMongo = true
			size += fi.Size()
			if fi.ModTime().After(mod) {
				mod = fi.ModTime()
			}
		}
		if !bi.HasMinio && !bi.HasMongo {
			continue // empty/incomplete bundle dir
		}
		bi.Size = size
		bi.Modified = mod.UTC().Format(time.RFC3339)
		backups = append(backups, bi)
	}
	sort.Slice(backups, func(i, j int) bool { return backups[i].Modified > backups[j].Modified })
	writeJSON(w, http.StatusOK, map[string]any{"dir": dir, "backups": backups})
}

// ── Single-flight job (backup OR restore — never both at once) ────────────────

// jobState is the JSON-serializable snapshot the UI polls. Kept separate from the
// mutex so it can be copied/marshalled freely.
type jobState struct {
	Running  bool   `json:"running"`
	Kind     string `json:"kind"`   // "backup" | "restore"
	Target   string `json:"target"` // bundle stamp
	Phase    string `json:"phase"`
	Err      string `json:"error"`
	Done     bool   `json:"done"`
	Started  string `json:"startedAt"`
	Finished string `json:"finishedAt"`
}

type Job struct {
	mu sync.Mutex
	st jobState
}

func NewJob() *Job { return &Job{} }

// begin claims the job; returns false if one is already running.
func (j *Job) begin(kind, target string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.st.Running {
		return false
	}
	j.st = jobState{Running: true, Kind: kind, Target: target, Phase: "starting", Started: time.Now().UTC().Format(time.RFC3339)}
	return true
}

func (j *Job) setPhase(p string) {
	j.mu.Lock()
	j.st.Phase = p
	j.mu.Unlock()
}

func (j *Job) finish(errMsg string) {
	j.mu.Lock()
	j.st.Running = false
	j.st.Done = true
	j.st.Err = errMsg
	j.st.Phase = "done"
	if errMsg != "" {
		j.st.Phase = "failed"
	}
	j.st.Finished = time.Now().UTC().Format(time.RFC3339)
	j.mu.Unlock()
}

func (j *Job) Snapshot() jobState {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.st
}

// GET /api/anchor/backups/status — current/last job state (drives the UI).
func (s *Server) handleJobStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.job.Snapshot())
}

// ── Create a backup on demand ────────────────────────────────────────────────

// POST /api/anchor/backups/create — body {minio:bool, mongo:bool}. Non-destructive,
// so no confirm/password gate; just single-flight.
func (s *Server) handleBackupCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Minio bool `json:"minio"`
		Mongo bool `json:"mongo"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if !body.Minio && !body.Mongo {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "select MinIO, Mongo, or both"})
		return
	}
	stamp := time.Now().UTC().Format("20060102-150405")
	if !s.job.begin("backup", stamp) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "a backup or restore is already running"})
		return
	}
	go s.runBackup(stamp, body.Minio, body.Mongo, clientIP(r))
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "started", "name": stamp})
}

// runBackup writes a bundle dir via helper containers (Anchor can't exec). A
// .partial dir is promoted to the final name only on success, so a half-written
// bundle never appears in the list.
func (s *Server) runBackup(stamp string, doMinio, doMongo bool, ip string) {
	// Helpers write to the bundle via the HOST path; Anchor manages the dir
	// lifecycle via its own (read-write) /backups mount of the same directory.
	hostWork := filepath.Join(backupHostDir(), "."+stamp+".partial")
	localWork := filepath.Join(backupDir(), "."+stamp+".partial")
	localDest := filepath.Join(backupDir(), stamp)

	if err := os.MkdirAll(localWork, 0o755); err != nil {
		s.finishJob("backup", stamp, ip, "could not create backup dir: "+err.Error())
		return
	}

	if doMinio {
		s.job.setPhase("backing up object store")
		if err := s.backupMinio(hostWork); err != nil {
			_ = os.RemoveAll(localWork)
			s.finishJob("backup", stamp, ip, "object store backup failed: "+err.Error())
			return
		}
	}
	if doMongo {
		s.job.setPhase("backing up database")
		if err := s.backupMongo(localWork); err != nil {
			_ = os.RemoveAll(localWork)
			s.finishJob("backup", stamp, ip, "database backup failed: "+err.Error())
			return
		}
	}

	if err := os.Rename(localWork, localDest); err != nil {
		s.finishJob("backup", stamp, ip, "could not finalize backup: "+err.Error())
		return
	}
	s.job.finish("")
	s.audit.Log(AuditEntry{Event: "backup", Target: stamp, Detail: backupKinds(doMinio, doMongo), IP: ip})
}

func backupKinds(m, g bool) string {
	switch {
	case m && g:
		return "minio+mongo"
	case m:
		return "minio"
	default:
		return "mongo"
	}
}

// backupMinio tars the MinIO volume into <work>/minio.tar.gz via an alpine helper
// (volume read-only; output written to the host bundle dir, which Docker creates).
func (s *Server) backupMinio(hostWork string) error {
	return dockerRun(
		"--rm",
		"-v", minioVolume()+":/data:ro",
		"-v", hostWork+":/out",
		"alpine:latest",
		"tar", "czf", "/out/minio.tar.gz", "-C", "/data", ".",
	)
}

// backupMongo dumps the nucleus DB into <workDir>/mongo.gz. The mongo:4.4 helper
// runs as a non-root user and can't write into the root-owned bundle dir, so it
// streams the archive to STDOUT and Anchor writes the file through its own (rw)
// mount — the same shape as the cron script's `mongodump … > mongo.gz`.
func (s *Server) backupMongo(workDir string) error {
	mongoCtr, err := s.containerByRole(context.Background(), "database", "ANCHOR_MONGO_CONTAINER")
	if err != nil {
		return err
	}
	out, err := os.Create(filepath.Join(workDir, "mongo.gz"))
	if err != nil {
		return err
	}
	defer out.Close()

	cmd := exec.Command("docker", "run", "--rm",
		"--network", "container:"+mongoCtr,
		"mongo:4.4",
		"mongodump", "--host", "127.0.0.1:27017",
		"--db", mongoDBName(), "--archive", "--gzip",
	)
	cmd.Env = os.Environ()
	cmd.Stdout = out // stream the gzipped archive straight to disk
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(errb.String()))
	}
	return nil
}

// ── Restore ──────────────────────────────────────────────────────────────────

// POST /api/anchor/backups/{name}/restore — two-step-confirmed + password-gated.
func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !backupStamp.MatchString(name) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid backup name"})
		return
	}
	bundle := filepath.Join(backupDir(), name)
	_, mErr := os.Stat(filepath.Join(bundle, "minio.tar.gz"))
	_, gErr := os.Stat(filepath.Join(bundle, "mongo.gz"))
	if mErr != nil && gErr != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "backup not found"})
		return
	}

	var body struct {
		ConfirmToken string `json:"confirmToken"`
		Password     string `json:"password"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	if body.ConfirmToken == "" {
		writeJSON(w, http.StatusPreconditionRequired, map[string]any{
			"needsConfirm": true,
			"confirmToken": s.confirm.Issue("restore", name),
			"action":       "restore",
			"impact": "This rolls Orbit back to " + name + ": it OVERWRITES all MinIO objects and replaces " +
				"Orbit's file records (orbitfiles, orbitfolders) in MongoDB with this backup's. MinIO is stopped " +
				"briefly, so Orbit is unavailable during the restore. Other apps' data is NOT touched. Anything " +
				"added to Orbit since this backup is permanently lost. This cannot be undone.",
		})
		return
	}

	if s.limiter.Locked() {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many attempts — locked, try again shortly"})
		return
	}
	if !verifyPassword(body.Password, s.store.PasswordHash()) {
		s.limiter.Fail()
		s.audit.Log(AuditEntry{Event: "restore_auth_fail", Target: name, IP: clientIP(r)})
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "incorrect password"})
		return
	}
	s.limiter.Reset()

	if !s.confirm.Check(body.ConfirmToken, "restore", name) {
		writeJSON(w, http.StatusPreconditionRequired, map[string]any{
			"needsConfirm": true,
			"confirmToken": s.confirm.Issue("restore", name),
			"action":       "restore",
			"impact":       "Confirmation expired — please confirm again.",
		})
		return
	}

	if !s.job.begin("restore", name) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "a backup or restore is already running"})
		return
	}
	s.audit.Log(AuditEntry{Event: "restore_start", Target: name, IP: clientIP(r)})
	go s.runRestore(name, clientIP(r))
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "started"})
}

// runRestore: stop MinIO → wipe + extract object data → restore Orbit's Mongo
// collections → start MinIO. Always tries to bring MinIO back up on failure.
func (s *Server) runRestore(name, ip string) {
	ctx := context.Background()
	bundle := filepath.Join(backupDir(), name)
	hasMinio := fileExists(filepath.Join(bundle, "minio.tar.gz"))
	hasMongo := fileExists(filepath.Join(bundle, "mongo.gz"))

	var minioID string
	if hasMinio {
		id, err := s.containerByRole(ctx, "object-store", "ANCHOR_MINIO_CONTAINER")
		if err != nil {
			s.finishJob("restore", name, ip, "could not locate the MinIO container: "+err.Error())
			return
		}
		minioID = id
		s.job.setPhase("stopping MinIO")
		if err := s.docker.Stop(ctx, id); err != nil {
			s.finishJob("restore", name, ip, "failed to stop MinIO: "+err.Error())
			return
		}
		s.job.setPhase("restoring object data")
		if err := extractBackup(filepath.Join(bundle, "minio.tar.gz"), restoreTargetDir()); err != nil {
			_ = s.docker.Start(ctx, id)
			s.finishJob("restore", name, ip, "object restore failed (MinIO restarted): "+err.Error())
			return
		}
	}

	if hasMongo {
		s.job.setPhase("restoring database")
		if err := s.restoreMongo(ctx, name); err != nil {
			if minioID != "" {
				_ = s.docker.Start(ctx, minioID)
			}
			s.finishJob("restore", name, ip, "database restore failed: "+err.Error())
			return
		}
	}

	if minioID != "" {
		s.job.setPhase("starting MinIO")
		if err := s.docker.Start(ctx, minioID); err != nil {
			s.finishJob("restore", name, ip, "data restored but failed to start MinIO: "+err.Error())
			return
		}
	}

	s.job.finish("")
	s.audit.Log(AuditEntry{Event: "restore", Target: name, IP: ip})
}

func (s *Server) finishJob(kind, name, ip, msg string) {
	s.job.finish(msg)
	s.audit.Log(AuditEntry{Event: kind + "_error", Target: name, Detail: msg, IP: ip})
}

// restoreMongo runs mongorestore in a helper that shares MongoDB's network
// namespace; --drop + --nsInclude scope it to exactly the Orbit collections.
func (s *Server) restoreMongo(ctx context.Context, stamp string) error {
	mongoCtr, err := s.containerByRole(ctx, "database", "ANCHOR_MONGO_CONTAINER")
	if err != nil {
		return err
	}
	db := mongoDBName()
	args := []string{
		"--rm",
		"--network", "container:" + mongoCtr,
		"-v", filepath.Join(backupHostDir(), stamp) + ":/b:ro",
		"mongo:4.4",
		"mongorestore", "--host", "127.0.0.1:27017",
		"--gzip", "--archive=/b/mongo.gz", "--drop",
	}
	for _, c := range orbitCollections {
		args = append(args, "--nsInclude="+db+"."+c)
	}
	return dockerRun(args...)
}

// dockerRun executes `docker run <args>` against the socket-proxy (inherited
// DOCKER_HOST), returning combined output on failure.
func dockerRun(args ...string) error {
	cmd := exec.Command("docker", append([]string{"run"}, args...)...)
	cmd.Env = os.Environ()
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(out.String()))
	}
	return nil
}

// containerByRole resolves a managed container by nucleus.role (overridable by
// env), so backup/restore act on exactly the running MinIO / MongoDB.
func (s *Server) containerByRole(ctx context.Context, role, envVar string) (string, error) {
	if v := os.Getenv(envVar); v != "" {
		return v, nil
	}
	list, err := s.docker.ListManaged(ctx)
	if err != nil {
		return "", err
	}
	for _, c := range list {
		if c.Labels["nucleus.role"] == role {
			return c.ID, nil
		}
	}
	return "", fmt.Errorf("no nucleus.managed container with role=%s found", role)
}

// extractBackup wipes targetDir's contents and re-extracts the gzipped tar into
// it. The archive is one our own backup produced (entries rooted at "./"), but we
// still reject any entry that would escape targetDir (zip-slip).
func extractBackup(archive, targetDir string) error {
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(targetDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(targetDir, e.Name())); err != nil {
			return fmt.Errorf("clearing %s: %w", e.Name(), err)
		}
	}

	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	cleanTarget := filepath.Clean(targetDir)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		clean := filepath.Clean(hdr.Name)
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
			return fmt.Errorf("unsafe path in archive: %s", hdr.Name)
		}
		dest := filepath.Join(cleanTarget, clean)
		if dest != cleanTarget && !strings.HasPrefix(dest, cleanTarget+string(os.PathSeparator)) {
			return fmt.Errorf("unsafe path in archive: %s", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			out.Close()
		default:
			// MinIO's on-disk format is only files + dirs; ignore anything else.
		}
	}
	return nil
}
