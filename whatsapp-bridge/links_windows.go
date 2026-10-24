//go:build windows

package main

import (
	"os"
	"syscall"
)

// hardLinkCount returns how many directory entries point to the open file.
func hardLinkCount(f *os.File) (uint64, error) {
	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(syscall.Handle(f.Fd()), &info); err != nil {
		return 0, err
	}
	return uint64(info.NumberOfLinks), nil
}
