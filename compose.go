package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// ComposeTarget describes how to address a single service via `docker compose`.
// It is derived entirely from the container's OWN compose labels, so Anchor uses
// the exact project + files the stack was actually started with (dev or prod)
// instead of assuming one. Requires the Nucleus repo to be mounted at its real
// host path so these absolute file paths resolve inside Anchor.
type ComposeTarget struct {
	Project string
	Files   []string
	Dir     string
	Service string
}

type Compose struct{}

func NewCompose() *Compose { return &Compose{} }

// Target reads the compose coordinates from a container's labels.
func (c *Compose) Target(labels map[string]string) (ComposeTarget, error) {
	t := ComposeTarget{
		Project: labels["com.docker.compose.project"],
		Service: labels["com.docker.compose.service"],
		Dir:     labels["com.docker.compose.project.working_dir"],
	}
	if f := labels["com.docker.compose.project.config_files"]; f != "" {
		for _, p := range strings.Split(f, ",") {
			if p = strings.TrimSpace(p); p != "" {
				t.Files = append(t.Files, p)
			}
		}
	}
	if t.Project == "" || t.Service == "" || len(t.Files) == 0 {
		return t, fmt.Errorf("container is not a compose-managed service")
	}
	return t, nil
}

// MissingFile returns the first compose file Anchor can't read (i.e. the repo
// isn't mounted at its host path), or "" if all are present.
func (c *Compose) MissingFile(t ComposeTarget) string {
	for _, f := range t.Files {
		if !fileExists(f) {
			return f
		}
	}
	return ""
}

func (c *Compose) run(ctx context.Context, t ComposeTarget, args ...string) (string, error) {
	base := []string{"compose", "-p", t.Project}
	for _, f := range t.Files {
		base = append(base, "-f", f)
	}
	if t.Dir != "" {
		base = append(base, "--project-directory", t.Dir)
	}
	base = append(base, args...)

	cmd := exec.CommandContext(ctx, "docker", base...)
	if t.Dir != "" {
		cmd.Dir = t.Dir
	}
	// Force the classic builder. BuildKit needs a session/hijack ("upgrade to
	// tcp") endpoint the socket-proxy blocks (403); the legacy /build endpoint
	// works through the proxy with BUILD=1.
	cmd.Env = append(os.Environ(), "DOCKER_BUILDKIT=0", "COMPOSE_DOCKER_CLI_BUILD=0")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

// Recreate re-creates a single service from its existing image, applying any
// updated .env / config. --no-deps keeps it scoped to just this service.
func (c *Compose) Recreate(ctx context.Context, t ComposeTarget) (string, error) {
	return c.run(ctx, t, "up", "-d", "--no-build", "--no-deps", "--force-recreate", t.Service)
}

// Rebuild rebuilds a service's image from its build context, then recreates it.
// Note: this rebuilds the container image (e.g. a node server), not pre-built
// frontend assets — those are produced by infra/production on the host.
func (c *Compose) Rebuild(ctx context.Context, t ComposeTarget) (string, error) {
	return c.run(ctx, t, "up", "-d", "--build", "--no-deps", "--force-recreate", t.Service)
}
