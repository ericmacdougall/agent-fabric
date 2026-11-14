package credential

import (
	"os"
	"syscall"
)

func statNlink(fi os.FileInfo) uint64 { return uint64(fi.Sys().(*syscall.Stat_t).Nlink) }
func statGID(fi os.FileInfo) int      { return int(fi.Sys().(*syscall.Stat_t).Gid) }
