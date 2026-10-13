package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// appVersion is stamped at build time: -ldflags "-X main.appVersion=1.1.0"
var appVersion = "1.0.0"

const defaultManifestURL = "https://raw.githubusercontent.com/onocde/private-muse/main/update.json"

func manifestURL() string {
	if v := os.Getenv("PRIVATEMUSE_UPDATE_URL"); v != "" {
		return v
	}
	return defaultManifestURL
}

type manifest struct {
	Version string `json:"version"`
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
	Notes   string `json:"notes"`
}

func parseVer(s string) [3]int {
	var v [3]int
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	parts := strings.Split(s, ".")
	for i := 0; i < len(parts) && i < 3; i++ {
		n, _ := strconv.Atoi(parts[i])
		v[i] = n
	}
	return v
}

func isNewer(remote, local string) bool {
	r, l := parseVer(remote), parseVer(local)
	for i := 0; i < 3; i++ {
		if r[i] != l[i] {
			return r[i] > l[i]
		}
	}
	return false
}

func fetchManifest() (*manifest, error) {
	client := &http.Client{Timeout: 12 * time.Second}
	resp, err := client.Get(manifestURL())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("update check: HTTP %d", resp.StatusCode)
	}
	var m manifest
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&m); err != nil {
		return nil, err
	}
	if m.Version == "" || m.URL == "" {
		return nil, fmt.Errorf("update manifest incomplete")
	}
	return &m, nil
}

func (a *App) checkUpdate() {
	m, err := fetchManifest()
	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		a.updErr = "check failed: " + err.Error()
		return
	}
	a.updErr = ""
	a.updChecked = true
	if isNewer(m.Version, appVersion) {
		a.updAvailable = true
		a.updVersion = m.Version
		a.updNotes = m.Notes
		a.updManifest = m
	} else {
		a.updAvailable = false
		a.updVersion = ""
		a.updManifest = nil
	}
}

// applyUpdate downloads the new exe next to the current one, verifies its
// SHA-256, swaps it into the current exe's exact path, relaunches, and exits.
// The old binary is renamed to <exe>.old and cleaned up by the next launch.
func (a *App) applyUpdate() {
	a.mu.Lock()
	m := a.updManifest
	if m == nil || a.updBusy {
		a.mu.Unlock()
		return
	}
	a.updBusy = true
	a.updErr = ""
	a.updDone, a.updTotal = 0, m.Size
	a.mu.Unlock()

	fail := func(format string, args ...any) {
		a.mu.Lock()
		a.updBusy = false
		a.updErr = fmt.Sprintf(format, args...)
		a.mu.Unlock()
	}

	self, err := os.Executable()
	if err != nil {
		fail("cannot locate own exe: %v", err)
		return
	}
	self, _ = filepath.EvalSymlinks(self)
	dir := filepath.Dir(self)
	staging := filepath.Join(dir, ".privatemuse-update.exe")

	client := &http.Client{}
	resp, err := client.Get(m.URL)
	if err != nil {
		fail("update download failed: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fail("update download failed: HTTP %d", resp.StatusCode)
		return
	}
	f, err := os.OpenFile(staging, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		fail("cannot write update file: %v", err)
		return
	}
	h := sha256.New()
	pw := &updWriter{a: a, w: io.MultiWriter(f, h)}
	_, err = io.CopyBuffer(pw, resp.Body, make([]byte, 1<<20))
	f.Close()
	if err != nil {
		os.Remove(staging)
		fail("update download interrupted: %v", err)
		return
	}
	if m.Size > 0 && pw.total != m.Size {
		os.Remove(staging)
		fail("update size mismatch: got %d, want %d", pw.total, m.Size)
		return
	}
	if m.SHA256 != "" {
		got := hex.EncodeToString(h.Sum(nil))
		if !strings.EqualFold(got, m.SHA256) {
			os.Remove(staging)
			fail("update failed safety check (hash mismatch) — keeping current version")
			return
		}
	}

	// Swap: a running exe can be renamed (not overwritten) on Windows.
	a.stopEngine()
	old := self + ".old"
	os.Remove(old)
	if err := os.Rename(self, old); err != nil {
		os.Remove(staging)
		fail("cannot replace exe: %v", err)
		return
	}
	if err := os.Rename(staging, self); err != nil {
		os.Rename(old, self) // roll back
		fail("cannot install update: %v", err)
		return
	}
	os.Chmod(self, 0o755)

	cmd := exec.Command(self, "--no-open")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		fail("update installed but relaunch failed — start PrivateMuse.exe again manually")
		return
	}
	time.Sleep(400 * time.Millisecond)
	os.Exit(0)
}

type updWriter struct {
	a     *App
	w     io.Writer
	total int64
}

func (u *updWriter) Write(p []byte) (int, error) {
	n, err := u.w.Write(p)
	u.total += int64(n)
	u.a.mu.Lock()
	u.a.updDone = u.total
	u.a.mu.Unlock()
	return n, err
}

// cleanupOldExe removes the previous binary left over from an update swap.
func cleanupOldExe() {
	self, err := os.Executable()
	if err != nil {
		return
	}
	old := self + ".old"
	for i := 0; i < 5; i++ {
		if err := os.Remove(old); err == nil || os.IsNotExist(err) {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	os.Remove(filepath.Join(filepath.Dir(self), ".privatemuse-update.exe"))
}
