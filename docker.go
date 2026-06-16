package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Docker is a tiny Docker Engine API client over HTTP. It talks to the
// read-only socket-proxy sidecar (DOCKER_HOST=tcp://docker-socket-proxy:2375)
// rather than mounting the raw socket — so "no mutations / no exec" is enforced
// at the proxy, not just in this code. No SDK dependency: stdlib only.
type Docker struct {
	base   string
	http   *http.Client // short calls
	stream *http.Client // long-lived (events) — no timeout
}

func NewDocker() *Docker {
	host := os.Getenv("DOCKER_HOST")
	if host == "" {
		host = "tcp://docker-socket-proxy:2375"
	}

	tr := &http.Transport{}
	var base string
	switch {
	case strings.HasPrefix(host, "unix://"):
		sock := strings.TrimPrefix(host, "unix://")
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}
		base = "http://docker"
	case strings.HasPrefix(host, "tcp://"):
		base = "http://" + strings.TrimPrefix(host, "tcp://")
	default:
		base = host
	}

	return &Docker{
		base:   base,
		http:   &http.Client{Timeout: 30 * time.Second, Transport: tr},
		stream: &http.Client{Transport: tr},
	}
}

func (d *Docker) get(ctx context.Context, client *http.Client, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.base+path, nil)
	if err != nil {
		return nil, err
	}
	return client.Do(req)
}

// Container is the subset of /containers/json we use.
type Container struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Image  string            `json:"Image"`
	State  string            `json:"State"`
	Status string            `json:"Status"`
	Labels map[string]string `json:"Labels"`
}

// Service is the classified view model the UI consumes.
type Service struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Image  string `json:"image"`
	State  string `json:"state"`
	Status string `json:"status"`
	Health string `json:"health"`
	Role   string `json:"role"`
	App    string `json:"app"`
}

const managedFilter = `{"label":["nucleus.managed=true"]}`

// ListManaged returns all containers (running or not) labeled nucleus.managed=true.
func (d *Docker) ListManaged(ctx context.Context) ([]Container, error) {
	u := "/containers/json?all=1&filters=" + url.QueryEscape(managedFilter)
	resp, err := d.get(ctx, d.http, u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("list containers: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var cs []Container
	if err := json.NewDecoder(resp.Body).Decode(&cs); err != nil {
		return nil, err
	}
	return cs, nil
}

// Inspect returns the raw /containers/{id}/json document.
func (d *Docker) Inspect(ctx context.Context, id string) (json.RawMessage, error) {
	resp, err := d.get(ctx, d.http, "/containers/"+url.PathEscape(id)+"/json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("inspect %s: %s", id, resp.Status)
	}
	return json.RawMessage(b), nil
}

// IsManaged reports whether a container carries nucleus.managed=true. This is the
// application-layer scope check enforced before any per-container operation.
func (d *Docker) IsManaged(ctx context.Context, id string) (bool, error) {
	raw, err := d.Inspect(ctx, id)
	if err != nil {
		return false, err
	}
	var meta struct {
		Config struct {
			Labels map[string]string
		}
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return false, err
	}
	return meta.Config.Labels["nucleus.managed"] == "true", nil
}

// Logs returns the last `tail` lines of a container's logs as plain text.
func (d *Docker) Logs(ctx context.Context, id string, tail int) (string, error) {
	u := fmt.Sprintf("/containers/%s/logs?stdout=1&stderr=1&timestamps=1&tail=%d", url.PathEscape(id), tail)
	resp, err := d.get(ctx, d.http, u)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("logs %s: %s: %s", id, resp.Status, strings.TrimSpace(string(b)))
	}
	return demuxLogs(resp.Body), nil
}

// Events calls onEvent for each container event for managed containers, until
// ctx is cancelled or the stream ends.
func (d *Docker) Events(ctx context.Context, onEvent func()) error {
	filter := `{"type":["container"],"label":["nucleus.managed=true"]}`
	resp, err := d.get(ctx, d.stream, "/events?filters="+url.QueryEscape(filter))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		onEvent()
	}
	return sc.Err()
}

// demuxLogs strips Docker's 8-byte stream-multiplexing frame headers (used when
// the container has no TTY). If the stream isn't framed (TTY containers), it is
// returned raw.
func demuxLogs(r io.Reader) string {
	var sb strings.Builder
	header := make([]byte, 8)
	for {
		if _, err := io.ReadFull(r, header); err != nil {
			break
		}
		// Frame header byte 0 is the stream type: 0=stdin,1=stdout,2=stderr.
		// Anything else means this isn't a framed stream — treat as raw.
		if header[0] > 2 {
			sb.Write(header)
			_, _ = io.Copy(&sb, r)
			break
		}
		size := binary.BigEndian.Uint32(header[4:8])
		if _, err := io.CopyN(&sb, r, int64(size)); err != nil {
			break
		}
	}
	return sb.String()
}

// toService classifies a container into the UI view model.
func toService(c Container) Service {
	name := ""
	if len(c.Names) > 0 {
		name = strings.TrimPrefix(c.Names[0], "/")
	}
	return Service{
		ID:     c.ID,
		Name:   name,
		Image:  c.Image,
		State:  c.State,
		Status: c.Status,
		Health: parseHealth(c.Status),
		Role:   c.Labels["nucleus.role"],
		App:    c.Labels["nucleus.app"],
	}
}

func parseHealth(status string) string {
	switch {
	case strings.Contains(status, "(healthy)"):
		return "healthy"
	case strings.Contains(status, "(unhealthy)"):
		return "unhealthy"
	case strings.Contains(status, "health: starting"):
		return "starting"
	default:
		return "none"
	}
}
