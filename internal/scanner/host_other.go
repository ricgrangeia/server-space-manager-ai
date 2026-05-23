//go:build !linux

package scanner

// statfs stub for non-Linux developer machines. The daemon's production
// target is Linux containers; this lets the code compile on Windows/macOS
// for local development and unit tests, returning ok=false so filesystem
// capacity samples are simply omitted from the scan.
func statfs(_ string) (total, avail int64, ok bool) {
	return 0, 0, false
}
