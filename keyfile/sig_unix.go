//go:build unix

package keyfile

import (
	"io/fs"
	"syscall"
)

// inode returns the file inode so an atomic rename-replace is noticed even
// when mtime and size happen to coincide.
func inode(fi fs.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino) //nolint:unconvert // field width differs per platform
	}
	return 0
}
