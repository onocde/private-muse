//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
)

// Opens the chat UI as an app-style window (Edge/Chrome --app mode),
// falling back to the default browser.
func openWindow(url string) {
	candidates := []string{}
	for _, root := range []string{os.Getenv("ProgramFiles(x86)"), os.Getenv("ProgramFiles"), os.Getenv("LocalAppData")} {
		if root == "" {
			continue
		}
		candidates = append(candidates,
			filepath.Join(root, "Microsoft", "Edge", "Application", "msedge.exe"),
			filepath.Join(root, "Google", "Chrome", "Application", "chrome.exe"),
		)
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			exec.Command(c, "--app="+url, "--window-size=1280,880").Start()
			return
		}
	}
	exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}
