package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DockerClient is a minimal Docker Engine API client that talks to the local
// /var/run/docker.sock. We use raw net/http instead of the official SDK to
// avoid a large transitive dependency tree for the three operations we need
// (list, start, stop).
type DockerClient struct {
	hc      *http.Client
	project string
}

// NewDockerClient returns a client that resolves containers within the given
// docker-compose project (matched via the com.docker.compose.project label).
func NewDockerClient(project string) (*DockerClient, error) {
	return &DockerClient{
		project: project,
		hc: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", "/var/run/docker.sock")
				},
			},
		},
	}, nil
}

// FindContainer returns the container ID for the given compose service in
// this project. Empty string + nil error means the service has no container.
func (d *DockerClient) FindContainer(ctx context.Context, service string) (string, error) {
	filters := map[string][]string{
		"label": {
			"com.docker.compose.project=" + d.project,
			"com.docker.compose.service=" + service,
		},
	}
	fb, _ := json.Marshal(filters)
	q := url.Values{}
	q.Set("all", "true") // include stopped/created containers
	q.Set("filters", string(fb))

	req, _ := http.NewRequestWithContext(ctx, "GET", "http://unix/containers/json?"+q.Encode(), nil)
	resp, err := d.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("docker GET containers/json: %s", resp.Status)
	}
	var arr []struct {
		ID string `json:"Id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&arr); err != nil {
		return "", err
	}
	if len(arr) == 0 {
		return "", nil
	}
	return arr[0].ID, nil
}

// Start runs the named compose service's container. Returns nil if the
// container is already running (Docker returns 304).
func (d *DockerClient) Start(ctx context.Context, service string) error {
	id, err := d.FindContainer(ctx, service)
	if err != nil {
		return fmt.Errorf("find %s: %w", service, err)
	}
	if id == "" {
		return fmt.Errorf("no container for service %q in project %q", service, d.project)
	}
	return d.post(ctx, "/containers/"+id+"/start", nil)
}

// Stop sends SIGTERM, then SIGKILL after `graceSeconds`. 120s matches the
// game-server's compose stop_grace_period.
func (d *DockerClient) Stop(ctx context.Context, service string, graceSeconds int) error {
	id, err := d.FindContainer(ctx, service)
	if err != nil {
		return fmt.Errorf("find %s: %w", service, err)
	}
	if id == "" {
		return fmt.Errorf("no container for service %q in project %q", service, d.project)
	}
	q := url.Values{}
	q.Set("t", fmt.Sprintf("%d", graceSeconds))
	return d.post(ctx, "/containers/"+id+"/stop?"+q.Encode(), nil)
}

// GameServerInfo is what callers learn about each currently-running
// game-server container — its compose service name plus, if `network` was
// supplied, its IP address on that network.
type GameServerInfo struct {
	Service string
	IP      string // empty if the network wasn't requested or container isn't on it
}

// ListRunningGameServers returns one entry per running container in this
// project whose com.docker.compose.service label starts with "game-server-".
// Pass `network` (e.g. "dune-server_default") to also fill the IP field;
// pass "" to skip the IP lookup. Used by both the farm_state cleanup
// (which needs the IPs) and the on-demand recovery on startup (which only
// needs the service names).
func (d *DockerClient) ListRunningGameServers(ctx context.Context, network string) ([]GameServerInfo, error) {
	filters := map[string][]string{
		"label":  {"com.docker.compose.project=" + d.project},
		"status": {"running"},
	}
	fb, _ := json.Marshal(filters)
	q := url.Values{}
	q.Set("filters", string(fb))

	req, _ := http.NewRequestWithContext(ctx, "GET", "http://unix/containers/json?"+q.Encode(), nil)
	resp, err := d.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("docker GET containers/json: %s", resp.Status)
	}

	var arr []struct {
		Labels          map[string]string `json:"Labels"`
		NetworkSettings struct {
			Networks map[string]struct {
				IPAddress string `json:"IPAddress"`
			} `json:"Networks"`
		} `json:"NetworkSettings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&arr); err != nil {
		return nil, err
	}

	out := make([]GameServerInfo, 0, len(arr))
	for _, c := range arr {
		svc := c.Labels["com.docker.compose.service"]
		if !strings.HasPrefix(svc, "game-server-") {
			continue
		}
		info := GameServerInfo{Service: svc}
		if network != "" {
			if n, ok := c.NetworkSettings.Networks[network]; ok {
				info.IP = n.IPAddress
			}
		}
		out = append(out, info)
	}
	return out, nil
}

func (d *DockerClient) post(ctx context.Context, path string, body []byte) error {
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://unix"+path, nil)
	resp, err := d.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// 204 = action performed; 304 = container already in target state. Both fine.
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotModified {
		return fmt.Errorf("docker POST %s: %s", path, resp.Status)
	}
	return nil
}
