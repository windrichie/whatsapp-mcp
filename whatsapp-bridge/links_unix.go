//go:build unix

package main

import (
	"errors"
	"os"
	"syscall"
)

// hardLinkCount returns how many directory entries point to the open file.
func hardLinkCount(f *os.File) (uint64, error) {
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, errors.New("cannot read the file's link count")
	}
	return uint64(st.Nlink), nil
}
