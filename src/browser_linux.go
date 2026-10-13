//go:build linux

package main

import "os/exec"

func openWindow(url string) {
	exec.Command("xdg-open", url).Start()
}
