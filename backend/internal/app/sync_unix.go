//go:build !windows

package app

import "os"

func syncDir(f *os.File) error { return f.Sync() }
