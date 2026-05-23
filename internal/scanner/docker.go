package scanner

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"

	"github.com/ricgrangeia/server-space-manager-ai/internal/config"
	"github.com/ricgrangeia/server-space-manager-ai/internal/store"
)

type DockerScanner struct {
	cli *client.Client
	cfg config.DockerCfg
}

func NewDocker(cfg config.DockerCfg) (*DockerScanner, error) {
	opts := []client.Opt{client.WithAPIVersionNegotiation()}
	if cfg.Host != "" {
		opts = append(opts, client.WithHost(cfg.Host))
	} else {
		opts = append(opts, client.FromEnv)
	}
	cli, err := client.NewClientWithOpts(opts...)
	if err != nil {
		return nil, err
	}
	return &DockerScanner{cli: cli, cfg: cfg}, nil
}

func (d *DockerScanner) Scan(ctx context.Context, now time.Time) []store.Sample {
	var out []store.Sample

	containers, err := d.cli.ContainerList(ctx, container.ListOptions{All: true})
	if err == nil {
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
			if d.cfg.TrackBindMounts {
				for _, m := range c.Mounts {
					if m.Type != "bind" {
						continue
					}
					src := m.Source
					if size, ok := dirSize(src); ok {
						out = append(out, store.Sample{
							Kind: "bind_mount", Key: c.ID + ":" + src,
							Label: name + " -> " + src,
							Bytes: size, Extra: extra, TakenAt: now,
						})
					}
				}
			}
		}
	}

	if d.cfg.TrackVolumes || d.cfg.TrackImages {
		du, err := d.cli.DiskUsage(ctx, types.DiskUsageOptions{})
		if err == nil {
			if d.cfg.TrackVolumes {
				for _, v := range du.Volumes {
					if v == nil {
						continue
					}
					size, refs := int64(0), int64(-1)
					if v.UsageData != nil {
						size = v.UsageData.Size
						refs = v.UsageData.RefCount
					}
					ex, _ := json.Marshal(map[string]any{"refs": refs, "driver": v.Driver})
					kind := "volume"
					if refs == 0 {
						kind = "orphan_volume"
					}
					out = append(out, store.Sample{
						Kind: kind, Key: v.Name, Label: v.Name,
						Bytes: size, Extra: string(ex), TakenAt: now,
					})
				}
			}
			if d.cfg.TrackImages {
				for _, img := range du.Images {
					if img == nil {
						continue
					}
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
	}

	return out
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

// logSize returns the size of LogPath plus rotated siblings. Tries /host-prefixed
// path as a fallback if the host root is mounted under /host inside the container.
func (d *DockerScanner) logSize(ctx context.Context, id string) (int64, bool) {
	info, err := d.cli.ContainerInspect(ctx, id)
	if err != nil || info.LogPath == "" {
		return 0, false
	}
	for _, base := range []string{info.LogPath, "/host" + info.LogPath} {
		if _, err := os.Stat(base); err != nil {
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
