package app

import "golang.org/x/sys/windows"

func diskSpace(path string) (total, available int64, err error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	var a, t, f uint64
	err = windows.GetDiskFreeSpaceEx(p, &a, &t, &f)
	return int64(t), int64(a), err
}
