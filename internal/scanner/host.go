package scanner

import (
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ricgrangeia/server-space-manager-ai/internal/config"
	"github.com/ricgrangeia/server-space-manager-ai/internal/store"
)

type HostScanner struct {
	Paths       []config.PathCfg
	Filesystems []string
	Ignore      []string
}

func NewHost(cfg *config.Config) *HostScanner {
	return &HostScanner{
		Paths:       cfg.HostPaths,
		Filesystems: cfg.Filesystems,
		Ignore:      cfg.Ignore,
	}
}

func (h *HostScanner) Scan(now time.Time) []store.Sample {
	var out []store.Sample
	for _, p := range h.Paths {
		out = append(out, h.walkPath(p, now)...)
	}
	for _, fsPath := range h.Filesystems {
		if s, ok := h.statFS(fsPath, now); ok {
			out = append(out, s)
		}
	}
	return out
}

// walkPath emits one sample per directory up to MaxDepth and one for the root.
func (h *HostScanner) walkPath(p config.PathCfg, now time.Time) []store.Sample {
	rootDepth := strings.Count(filepath.Clean(p.Path), string(filepath.Separator))
	sizes := map[string]int64{}

	_ = filepath.WalkDir(p.Path, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip permission errors etc.
		}
		if h.ignored(d.Name()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		// attribute file size to each ancestor up to max_depth
		dir := filepath.Dir(path)
		for {
			depth := strings.Count(dir, string(filepath.Separator)) - rootDepth
			if depth < 0 {
				break
			}
			if p.MaxDepth == 0 || depth <= p.MaxDepth {
				sizes[dir] += info.Size()
			}
			if dir == p.Path || dir == "/" {
				break
			}
			dir = filepath.Dir(dir)
		}
		return nil
	})

	out := make([]store.Sample, 0, len(sizes))
	for dir, b := range sizes {
		out = append(out, store.Sample{
			Kind:    "host_path",
			Key:     dir,
			Label:   dir,
			Bytes:   b,
			TakenAt: now,
		})
	}
	return out
}

// statFS is implemented per-platform in host_linux.go / host_other.go because
// syscall.Statfs only exists on Unix-like systems. The daemon's deployment
// target is Linux containers; the non-Linux stub returns ok=false so unit
// tests can still build on a developer's Windows/macOS machine.
func (h *HostScanner) statFS(path string, now time.Time) (store.Sample, bool) {
	total, avail, ok := statfs(path)
	if !ok {
		return store.Sample{}, false
	}
	used := total - avail
	return store.Sample{
		Kind:    "fs",
		Key:     path,
		Label:   path,
		Bytes:   used,
		Extra:   formatFSExtra(total, avail),
		TakenAt: now,
	}, true
}

func formatFSExtra(total, avail int64) string {
	return `{"total":` + strconv.FormatInt(total, 10) + `,"avail":` + strconv.FormatInt(avail, 10) + `}`
}

func (h *HostScanner) ignored(name string) bool {
	for _, pat := range h.Ignore {
		if ok, _ := filepath.Match(pat, name); ok {
			return true
		}
	}
	return false
}
