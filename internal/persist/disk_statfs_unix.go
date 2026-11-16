//go:build linux || darwin

package persist

import (
	"errors"
	"syscall"
)

// fsSpace reports available bytes (for the unprivileged caller), total bytes
// and free inodes of the filesystem backing path.
func fsSpace(path string) (free, total, freeInodes uint64, err error) {
	var st syscall.Statfs_t
	if e := syscall.Statfs(path, &st); e != nil {
		return 0, 0, 0, e
	}
	return st.Bavail * uint64(st.Bsize), st.Blocks * uint64(st.Bsize), st.Ffree, nil
}

func isNoSpace(e error) bool {
	return errors.Is(e, syscall.ENOSPC) || errors.Is(e, syscall.EDQUOT)
}
