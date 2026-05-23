//go:build linux

package scanner

import "syscall"

// statfs returns (total, available, ok) bytes for the given mount path
// using the Linux statfs(2) system call. ok is false on any error.
func statfs(path string) (total, avail int64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, false
	}
	return int64(st.Blocks) * int64(st.Bsize),
		int64(st.Bavail) * int64(st.Bsize),
		true
}
