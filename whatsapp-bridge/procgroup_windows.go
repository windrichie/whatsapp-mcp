//go:build windows

package main

import "os/exec"

// configureProcessGroup is a no-op on Windows; exec.CommandContext kills the command.
func configureProcessGroup(cmd *exec.Cmd) {}
