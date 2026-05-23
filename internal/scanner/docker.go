package scanner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ricgrangeia/server-space-manager-ai/internal/config"
	"github.com/ricgrangeia/server-space-manager-ai/internal/store"
)

// DockerScanner inspects Docker via plain HTTP against either:
//   - a TCP endpoint (typical: the tecnativa/docker-socket-proxy sidecar)
//   - a Unix domain socket (when the manager is given direct socket access)
//
// We talk to the Engine API by hand rather than importing the Docker SDK,
// which would drag in a large dependency tree (go-connections, OpenTelemetry,
// containerd/log, etc.) for the four endpoints we actually need:
//
//   - GET /containers/json?all=true   — list containers (incl. stopped)
//   - GET /containers/{id}/json       — inspect for log path, mounts, labels
//   - GET /system/df                  — disk-usage breakdown for volumes/images
//   - GET /volumes                    — volume list incl. orphan detection
type DockerScanner struct {
	cfg     config.DockerCfg
	http    *http.Client
	baseURL string // http://host:port — host is ignored for unix sockets
}

// NewDocker builds a scanner against cfg.Host. Accepted forms:
//
//   - "tcp://host:port"        — standard TCP, used with socket-proxy
//   - "http://host:port"       — alias for tcp://
//   - "unix:///path/to/sock"   — direct Unix socket access
//   - ""                        — defaults to /var/run/docker.sock
func NewDocker(cfg config.DockerCfg) (*DockerScanner, error) {
	host := cfg.Host
	if host == "" {
		host = "unix:///var/run/docker.sock"
	}

	d := &DockerScanner{cfg: cfg}
	switch {
	case strings.HasPrefix(host, "unix://"):
		sockPath := strings.TrimPrefix(host, "unix://")
		d.http = &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var dialer net.Dialer
					return dialer.DialContext(ctx, "unix", sockPath)
				},
			},
		}
		// Host portion is ignored when dialing the socket; URL just needs to parse.
		d.baseURL = "http://docker"
	case strings.HasPrefix(host, "tcp://"):
		d.baseURL = "http://" + strings.TrimPrefix(host, "tcp://")
		d.http = &http.Client{Timeout: 15 * time.Second}
	case strings.HasPrefix(host, "http://"), strings.HasPrefix(host, "https://"):
		d.baseURL = host
		d.http = &http.Client{Timeout: 15 * time.Second}
	default:
		return nil, fmt.Errorf("docker host: unsupported scheme %q", host)
	}
	return d, nil
}

// containerSummary mirrors the fields we use from GET /containers/json.
// Anything else in the response is ignored.
type containerSummary struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	State  string            `json:"State"`
	Labels map[string]string `json:"Labels"`
	Mounts []containerMount  `json:"Mounts"`
}

type containerMount struct {
	Type   string `json:"Type"`
	Source string `json:"Source"`
	// Name is set for Type=="volume" mounts; we use it to mark named
	// volumes as "in use" while iterating containers.
	Name string `json:"Name"`
}

// containerInspect carries the LogPath field needed to sum log sizes.
type containerInspect struct {
	LogPath string `json:"LogPath"`
}

// diskUsage is the relevant subset of GET /system/df.
type diskUsage struct {
	Images  []duImage  `json:"Images"`
	Volumes []duVolume `json:"Volumes"`
}

type duImage struct {
	ID         string   `json:"Id"`
	RepoTags   []string `json:"RepoTags"`
	Size       int64    `json:"Size"`
	SharedSize int64    `json:"SharedSize"`
	Containers int64    `json:"Containers"`
}

type duVolume struct {
	Name      string         `json:"Name"`
	Driver    string         `json:"Driver"`
	UsageData *duVolumeUsage `json:"UsageData,omitempty"`
}

type duVolumeUsage struct {
	Size     int64 `json:"Size"`
	RefCount int64 `json:"RefCount"`
}

// Scan collects samples from Docker. Best-effort: any failed sub-call logs
// nothing and is simply omitted from the result, so a partial outage of the
// socket-proxy degrades gracefully rather than dropping the whole scan.
func (d *DockerScanner) Scan(ctx context.Context, now time.Time) []store.Sample {
	var out []store.Sample

	// volumesInUse is filled during the container loop below so we can
	// distinguish "in use" from "orphan" when listing volumes afterwards.
	volumesInUse := map[string]struct{}{}

	var containers []containerSummary
	if err := d.get(ctx, "/containers/json?all=true", &containers); err == nil {
		for _, c := range containers {
			name := strings.TrimPrefix(firstName(c.Names), "/")
			stack := c.Labels["com.docker.compose.project"]
			service := c.Labels["com.docker.compose.service"]
			extra := labelExtra(stack, service, c.State)

			if d.cfg.TrackLogs {
				if logBytes, ok := d.logSize(ctx, c.ID); ok {
					out = append(out, store.Sample{
						Kind: "container_log", Key: c.ID, Label: name,
						Bytes: logBytes, Extra: extra, TakenAt: now,
					})
				}
			}
			for _, m := range c.Mounts {
				if m.Type == "volume" && m.Name != "" {
					volumesInUse[m.Name] = struct{}{}
				}
			}
			if d.cfg.TrackBindMounts {
				for _, m := range c.Mounts {
					if m.Type != "bind" || skipBindSource(m.Source) {
						continue
					}
					// Read through the host-mount view first (/host<src>) so
					// we measure the actual host path via our read-only mount,
					// not whatever happens to exist at that path inside ssm's
					// own namespace. Fall back to the raw path for the case
					// where ssm runs directly on the host without /host.
					size, ok := dirSize("/host" + m.Source)
					if !ok {
						size, ok = dirSize(m.Source)
					}
					if !ok {
						continue
					}
					out = append(out, store.Sample{
						Kind: "bind_mount", Key: c.ID + ":" + m.Source,
						Label: name + " -> " + m.Source,
						Bytes: size, Extra: extra, TakenAt: now,
					})
				}
			}
		}
	}

	if d.cfg.TrackVolumes {
		// /system/df returns UsageData: null for volumes on newer Docker
		// engines (the walk is expensive and skipped by default). Instead
		// we enumerate names via /volumes and compute sizes ourselves by
		// walking /host/var/lib/docker/volumes/<name>/_data, which is
		// already reachable through our read-only host mount.
		var vols struct {
			Volumes []struct {
				Name       string `json:"Name"`
				Driver     string `json:"Driver"`
				Mountpoint string `json:"Mountpoint"`
			} `json:"Volumes"`
		}
		if err := d.get(ctx, "/volumes", &vols); err == nil {
			for _, v := range vols.Volumes {
				// Prefer the path the daemon reports; fall back to the
				// conventional layout if Mountpoint is unset.
				mp := v.Mountpoint
				if mp == "" {
					mp = "/var/lib/docker/volumes/" + v.Name + "/_data"
				}
				size, ok := dirSize("/host" + mp)
				if !ok {
					size, _ = dirSize(mp)
				}
				_, inUse := volumesInUse[v.Name]
				kind := "volume"
				if !inUse {
					kind = "orphan_volume"
				}
				ex, _ := json.Marshal(map[string]any{
					"in_use": inUse, "driver": v.Driver,
				})
				out = append(out, store.Sample{
					Kind: kind, Key: v.Name, Label: v.Name,
					Bytes: size, Extra: string(ex), TakenAt: now,
				})
			}
		}
	}

	if d.cfg.TrackImages {
		// /system/df's Images section *does* reliably include sizes, so
		// we still use it for image accounting.
		var du diskUsage
		if err := d.get(ctx, "/system/df", &du); err == nil {
			for _, img := range du.Images {
				tag := "<none>"
				if len(img.RepoTags) > 0 {
					tag = img.RepoTags[0]
				}
				ex, _ := json.Marshal(map[string]any{
					"shared": img.SharedSize, "containers": img.Containers,
				})
				out = append(out, store.Sample{
					Kind: "image", Key: img.ID, Label: tag,
					Bytes: img.Size, Extra: string(ex), TakenAt: now,
				})
			}
		}
	}

	return out
}

// get performs a GET against the Docker Engine API and decodes the JSON body.
// Returns an error on non-2xx response (with the body trimmed for context).
func (d *DockerScanner) get(ctx context.Context, path string, into any) error {
	u, err := url.Parse(d.baseURL + path)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("docker %s: status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(resp.Body).Decode(into)
}

// skipBindSource returns true for bind-mount sources that produce nonsense
// or runaway walks if we try to size them:
//   - "/" mounts (typical for tools like ssm itself that need host
//     visibility — walking re-enters /host and counts overlay layers many
//     times, producing absurd byte totals).
//   - Kernel pseudo-filesystems (/proc, /sys, /dev) — not real bytes.
//   - The Docker socket file itself.
func skipBindSource(src string) bool {
	switch src {
	case "", "/", "/proc", "/sys", "/dev", "/var/run/docker.sock", "/run/docker.sock":
		return true
	}
	return strings.HasPrefix(src, "/proc/") ||
		strings.HasPrefix(src, "/sys/") ||
		strings.HasPrefix(src, "/dev/")
}

func firstName(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

func labelExtra(stack, service, state string) string {
	b, _ := json.Marshal(map[string]string{
		"stack": stack, "service": service, "state": state,
	})
	return string(b)
}

// logSize returns the total size of a container's JSON-file log set
// (the active file plus any rotated *.1, *.2, ... siblings).
//
// The Engine reports LogPath as a host path (e.g.
// /var/lib/docker/containers/<id>/<id>-json.log). When the host root is
// mounted read-only at /host inside the manager (the recommended layout),
// the file is reachable at the same path prefixed with /host.
func (d *DockerScanner) logSize(ctx context.Context, id string) (int64, bool) {
	var info containerInspect
	if err := d.get(ctx, "/containers/"+url.PathEscape(id)+"/json", &info); err != nil || info.LogPath == "" {
		return 0, false
	}
	for _, base := range []string{info.LogPath, "/host" + info.LogPath} {
		if _, err := os.Stat(base); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			continue
		}
		var total int64
		dir := filepath.Dir(base)
		prefix := filepath.Base(base)
		entries, err := os.ReadDir(dir)
		if err != nil {
			if st, e := os.Stat(base); e == nil {
				return st.Size(), true
			}
			continue
		}
		for _, e := range entries {
			if !strings.HasPrefix(e.Name(), prefix) {
				continue
			}
			if fi, err := e.Info(); err == nil {
				total += fi.Size()
			}
		}
		return total, true
	}
	return 0, false
}

func dirSize(path string) (int64, bool) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	if !st.IsDir() {
		return st.Size(), true
	}
	var total int64
	_ = filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total, true
}
