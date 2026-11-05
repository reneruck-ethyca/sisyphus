package lastmile

import (
	"os"
	"syscall"
)

// sysStamp returns the inode and ctime of fi: what utimes can't forge.
func sysStamp(fi os.FileInfo) (ino uint64, ctime int64) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Ino, st.Ctimespec.Nano()
	}
	return 0, 0
}
