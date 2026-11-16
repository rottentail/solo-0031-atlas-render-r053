//go:build !(linux || darwin)

package persist

import "errors"

func fsSpace(path string) (free, total, freeInodes uint64, err error) {
	return 0, 0, 0, errors.New("filesystem space probing is only supported on Linux and macOS")
}

func isNoSpace(e error) bool {
	return false
}
