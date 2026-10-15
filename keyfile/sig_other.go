//go:build !unix

package keyfile

import "io/fs"

// inode is unavailable off unix; mtime and size still drive reloads.
func inode(fs.FileInfo) uint64 { return 0 }
