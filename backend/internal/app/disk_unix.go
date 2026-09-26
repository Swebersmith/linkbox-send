//go:build !windows

package app

import "golang.org/x/sys/unix"

func diskSpace(path string) (total, available int64, err error) {
	var s unix.Statfs_t
	err = unix.Statfs(path, &s)
	return int64(s.Blocks) * int64(s.Bsize), int64(s.Bavail) * int64(s.Bsize), err
}
