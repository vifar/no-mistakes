//go:build !unix

package main

import "os/exec"

func configureEscape(cmd *exec.Cmd) {}
