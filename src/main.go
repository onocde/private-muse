package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

//go:embed ui.html
var uiHTML []byte

// ---------------------------------------------------------------- models

type ModelInfo struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	File  string `json:"file"`
	URL   string `json:"-"`
	Size  int64  `json:"size"`
}

var modelList = []ModelInfo{
	{
		ID:    "7b",
		Label: "Qwen2.5-Coder 7B Abliterated · Q4_K_M · fast (recommended)",
		File:  "Qwen2.5-Coder-7B-Instruct-abliterated-Q4_K_M.gguf",
		URL:   "https://huggingface.co/bartowski/Qwen2.5-Coder-7B-Instruct-abliterated-GGUF/resolve/main/Qwen2.5-Coder-7B-Instruct-abliterated-Q4_K_M.gguf",
		Size:  4683074400,
	},
	{
		ID:    "14b",
		Label: "Qwen2.5-Coder 14B Abliterated · Q4_K_M · smarter, slower",
		File:  "Qwen2.5-Coder-14B-Instruct-abliterated-Q4_K_M.gguf",
		URL:   "https://huggingface.co/bartowski/Qwen2.5-Coder-14B-Instruct-abliterated-GGUF/resolve/main/Qwen2.5-Coder-14B-Instruct-abliterated-Q4_K_M.gguf",
		Size:  8988111200,
	},
}

func modelByID(id string) ModelInfo {
	for _, m := range modelList {
		if m.ID == id {
			return m
		}
	}
	return modelList[0]
}

// ---------------------------------------------------------------- settings

type Settings struct {
	Model string `json:"model"`
	Ctx   int    `json:"ctx"`
}

func defaultSettings() Settings { return Settings{Model: "7b", Ctx: 32768} }

// ---------------------------------------------------------------- app state

type engineCand struct {
	Dir     string
	ExeName string
	NGL     int
	Name    string
}

type sample struct {
	t    time.Time
	done int64
}

type App struct {
	mu       sync.Mutex
	dataDir  string
	settings Settings

	phase   string // need-download | downloading | starting | ready | error
	errMsg  string
	backend string

	dlDone  int64
	dlTotal int64
	samples []sample

	engineCmd  *exec.Cmd
	enginePort int
	uiPort     int

	updAvailable bool
	updVersion   string
	updNotes     string
	updBusy      bool
	updChecked   bool
	updErr       string
	updDone      int64
	updTotal     int64
	updManifest  *manifest
}

func (a *App) modelPath() string {
	return filepath.Join(a.dataDir, "models", modelByID(a.settings.Model).File)
}

func (a *App) modelPresent() bool {
	st, err := os.Stat(a.modelPath())
	return err == nil && st.Size() == modelByID(a.settings.Model).Size
}

func (a *App) snapshot() map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	mi := modelByID(a.settings.Model)
	present := a.modelPresent()
	phase := a.phase
	if phase == "" {
		if present {
			phase = "downloaded"
		} else {
			phase = "need-download"
		}
	}
	var pct, speed float64
	if a.dlTotal > 0 {
		pct = float64(a.dlDone) / float64(a.dlTotal) * 100
	}
	if n := len(a.samples); n >= 2 {
		first, last := a.samples[0], a.samples[n-1]
		if dt := last.t.Sub(first.t).Seconds(); dt > 0 {
			speed = float64(last.done-first.done) / dt / 1e6
		}
	}
	return map[string]any{
		"phase":       phase,
		"error":       a.errMsg,
		"backend":     a.backend,
		"modelId":     a.settings.Model,
		"modelLabel":  mi.Label,
		"modelSizeGB": float64(mi.Size) / 1e9,
		"downloaded":  present,
		"dlDone":      a.dlDone,
		"dlTotal":     a.dlTotal,
		"dlPct":       pct,
		"dlSpeedMBs":  speed,
		"ctx":         a.settings.Ctx,
		"models":      modelList,
		"appVersion":  appVersion,
		"updateAvailable": a.updAvailable,
		"updateVersion":   a.updVersion,
		"updateNotes":     a.updNotes,
		"updateBusy":      a.updBusy,
		"updateChecked":   a.updChecked,
		"updateErr":       a.updErr,
		"updatePct":       updPct(a),
	}
}

func updPct(a *App) float64 {
	if a.updTotal > 0 {
		return float64(a.updDone) / float64(a.updTotal) * 100
	}
	return 0
}

func (a *App) setPhase(p, errMsg string) {
	a.mu.Lock()
	a.phase = p
	a.errMsg = errMsg
	a.mu.Unlock()
}

// ---------------------------------------------------------------- engine extraction

func extractZip(data []byte, dest string) error {
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return err
	}
	for _, f := range r.File {
		target := filepath.Join(dest, filepath.Clean("/"+f.Name))
		if !strings.HasPrefix(target, dest) {
			continue
		}
		if f.FileInfo().IsDir() {
			os.MkdirAll(target, 0o755)
			continue
		}
		os.MkdirAll(filepath.Dir(target), 0o755)
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(out, rc)
		rc.Close()
		out.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func extractTarGz(data []byte, dest string) error {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(dest, filepath.Clean("/"+hdr.Name))
		if !strings.HasPrefix(target, dest) {
			continue
		}
		if hdr.Typeflag == tar.TypeDir {
			os.MkdirAll(target, 0o755)
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		os.MkdirAll(filepath.Dir(target), 0o755)
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, tr)
		out.Close()
		if err != nil {
			return err
		}
	}
}

func (a *App) ensureEngine() error {
	base := filepath.Join(a.dataDir, "engine")
	for _, b := range engineBlobs() {
		dir := filepath.Join(base, b.Kind)
		marker := filepath.Join(dir, fmt.Sprintf(".ok-%d", len(b.Data)))
		if _, err := os.Stat(marker); err == nil {
			continue
		}
		os.RemoveAll(dir)
		os.MkdirAll(dir, 0o755)
		var err error
		if b.Tar {
			err = extractTarGz(b.Data, dir)
		} else {
			err = extractZip(b.Data, dir)
		}
		if err != nil {
			return fmt.Errorf("extract engine (%s): %w", b.Kind, err)
		}
		os.WriteFile(marker, []byte("ok"), 0o644)
	}
	return nil
}

func findExe(dir, name string) string {
	var found string
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && info.Name() == name {
			found = p
			return io.EOF
		}
		return nil
	})
	return found
}

// ---------------------------------------------------------------- download

type progWriter struct {
	a     *App
	inner io.Writer
	base  int64
	total int64
}

func (w *progWriter) Write(p []byte) (int, error) {
	n, err := w.inner.Write(p)
	if n > 0 {
		w.total += int64(n)
		w.a.mu.Lock()
		w.a.dlDone = w.base + w.total
		now := time.Now()
		w.a.samples = append(w.a.samples, sample{now, w.a.dlDone})
		if len(w.a.samples) > 60 {
			w.a.samples = w.a.samples[len(w.a.samples)-60:]
		}
		// drop samples older than 8s for a stable speed reading
		cut := now.Add(-8 * time.Second)
		i := 0
		for i < len(w.a.samples) && w.a.samples[i].t.Before(cut) {
			i++
		}
		if i > 0 && len(w.a.samples)-i >= 2 {
			w.a.samples = w.a.samples[i:]
		}
		w.a.mu.Unlock()
	}
	return n, err
}

func (a *App) download() {
	mi := modelByID(a.settings.Model)
	dest := a.modelPath()
	os.MkdirAll(filepath.Dir(dest), 0o755)
	part := dest + ".part"

	a.mu.Lock()
	a.phase = "downloading"
	a.errMsg = ""
	a.dlTotal = mi.Size
	a.dlDone = 0
	a.samples = nil
	a.mu.Unlock()

	var have int64
	if st, err := os.Stat(part); err == nil {
		have = st.Size()
		if have >= mi.Size {
			have = 0
			os.Remove(part)
		}
	}

	client := &http.Client{}
	req, err := http.NewRequest("GET", mi.URL, nil)
	if err != nil {
		a.setPhase("error", err.Error())
		return
	}
	if have > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
	}
	resp, err := client.Do(req)
	if err != nil {
		a.setPhase("error", "download failed: "+err.Error())
		return
	}
	defer resp.Body.Close()

	flags := os.O_CREATE | os.O_WRONLY
	if have > 0 && resp.StatusCode == http.StatusPartialContent {
		flags |= os.O_APPEND
	} else {
		have = 0
		flags |= os.O_TRUNC
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		a.setPhase("error", fmt.Sprintf("download failed: HTTP %d", resp.StatusCode))
		return
	}
	f, err := os.OpenFile(part, flags, 0o644)
	if err != nil {
		a.setPhase("error", err.Error())
		return
	}
	a.mu.Lock()
	a.dlDone = have
	a.mu.Unlock()

	pw := &progWriter{a: a, inner: f, base: have}
	_, err = io.CopyBuffer(pw, resp.Body, make([]byte, 1<<20))
	f.Close()
	if err != nil {
		a.setPhase("error", "download interrupted: "+err.Error()+" (relaunch to resume)")
		return
	}
	st, _ := os.Stat(part)
	if st == nil || st.Size() != mi.Size {
		got := int64(0)
		if st != nil {
			got = st.Size()
		}
		a.setPhase("error", fmt.Sprintf("download incomplete: got %d of %d bytes (relaunch to resume)", got, mi.Size))
		return
	}
	if err := os.Rename(part, dest); err != nil {
		a.setPhase("error", err.Error())
		return
	}
	a.setPhase("downloaded", "")
}

// ---------------------------------------------------------------- engine process

func freePort(start int) int {
	for p := start; p < start+50; p++ {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err == nil {
			ln.Close()
			return p
		}
	}
	return start
}

func (a *App) stopEngine() {
	a.mu.Lock()
	cmd := a.engineCmd
	a.engineCmd = nil
	a.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		cmd.Process.Kill()
		cmd.Wait()
	}
}

func waitHealthy(port int, cmd *exec.Cmd, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			return false
		}
		resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/health", port))
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
		time.Sleep(700 * time.Millisecond)
	}
	return false
}

func (a *App) startEngine() {
	if !a.modelPresent() {
		a.setPhase("error", "model file missing")
		return
	}
	a.stopEngine()
	a.setPhase("starting", "")
	port := freePort(18101)
	threads := runtime.NumCPU()
	var lastErr string
	for _, cand := range engineCandidates() {
		exe := findExe(filepath.Join(a.dataDir, "engine", cand.Dir), cand.ExeName)
		if exe == "" {
			lastErr = "engine binary not found (" + cand.Dir + ")"
			continue
		}
		args := []string{
			"-m", a.modelPath(),
			"-c", fmt.Sprint(a.settings.Ctx),
			"--host", "127.0.0.1",
			"--port", fmt.Sprint(port),
			"-ngl", fmt.Sprint(cand.NGL),
			"--cache-type-k", "q8_0",
			"--cache-type-v", "q8_0",
			"-t", fmt.Sprint(threads),
			"--jinja",
			"-np", "1",
		}
		cmd := exec.Command(exe, args...)
		cmd.Dir = filepath.Dir(exe)
		logFile, _ := os.Create(filepath.Join(a.dataDir, "engine.log"))
		if logFile != nil {
			cmd.Stdout = logFile
			cmd.Stderr = logFile
		}
		if err := cmd.Start(); err != nil {
			lastErr = err.Error()
			continue
		}
		if waitHealthy(port, cmd, 90*time.Second) {
			a.mu.Lock()
			a.engineCmd = cmd
			a.enginePort = port
			a.backend = cand.Name
			a.phase = "ready"
			a.errMsg = ""
			a.mu.Unlock()
			fmt.Printf("[engine] running on %s backend, port %d\n", cand.Name, port)
			return
		}
		lastErr = "engine did not become ready (" + cand.Name + ") — see engine.log"
		cmd.Process.Kill()
		cmd.Wait()
		if logFile != nil {
			logFile.Close()
		}
	}
	a.setPhase("error", lastErr)
}

// ---------------------------------------------------------------- http handlers

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (a *App) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, a.snapshot())
}

func (a *App) handleDownload(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	phase := a.phase
	a.mu.Unlock()
	if phase != "downloading" && !a.modelPresent() {
		go a.download()
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (a *App) handleStart(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	phase := a.phase
	a.mu.Unlock()
	if phase != "starting" && phase != "ready" {
		go a.startEngine()
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (a *App) handleSettings(w http.ResponseWriter, r *http.Request) {
	var s Settings
	if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	if s.Model != "7b" && s.Model != "14b" {
		s.Model = "7b"
	}
	switch {
	case s.Ctx < 4096:
		s.Ctx = 4096
	case s.Ctx > 131072:
		s.Ctx = 131072
	}
	a.stopEngine()
	a.mu.Lock()
	a.settings = s
	a.phase = ""
	a.errMsg = ""
	a.dlDone, a.dlTotal = 0, 0
	a.mu.Unlock()
	a.saveSettings()
	writeJSON(w, map[string]any{"ok": true})
}

func (a *App) handleCheckUpdate(w http.ResponseWriter, r *http.Request) {
	go a.checkUpdate()
	writeJSON(w, map[string]any{"ok": true})
}

func (a *App) handleUpdate(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	avail := a.updAvailable && a.updManifest != nil
	a.mu.Unlock()
	if avail {
		go a.applyUpdate()
	}
	writeJSON(w, map[string]any{"ok": avail})
}

func (a *App) handleQuit(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"ok": true})
	go func() {
		time.Sleep(300 * time.Millisecond)
		a.stopEngine()
		os.Exit(0)
	}()
}

func (a *App) handleChat(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	port, phase := a.enginePort, a.phase
	a.mu.Unlock()
	if phase != "ready" || port == 0 {
		http.Error(w, "engine not ready", http.StatusServiceUnavailable)
		return
	}
	var in struct {
		Messages []map[string]string `json:"messages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	payload := map[string]any{
		"model":    "local",
		"messages": in.Messages,
		"stream":   true,
	}
	body, _ := json.Marshal(payload)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", port),
		bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, "engine unreachable: "+err.Error(), 502)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	fl, _ := w.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
			if fl != nil {
				fl.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// ---------------------------------------------------------------- settings persistence

func (a *App) settingsPath() string { return filepath.Join(a.dataDir, "settings.json") }

func (a *App) loadSettings() {
	a.settings = defaultSettings()
	if b, err := os.ReadFile(a.settingsPath()); err == nil {
		var s Settings
		if json.Unmarshal(b, &s) == nil && s.Ctx >= 4096 && s.Ctx <= 131072 {
			if s.Model == "7b" || s.Model == "14b" {
				a.settings = s
			}
		}
	}
}

func (a *App) saveSettings() {
	b, _ := json.MarshalIndent(a.settings, "", "  ")
	os.WriteFile(a.settingsPath(), b, 0o644)
}

// ---------------------------------------------------------------- main

func main() {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		cacheDir = "."
	}
	app := &App{dataDir: filepath.Join(cacheDir, "PrivateMuse")}
	os.MkdirAll(filepath.Join(app.dataDir, "models"), 0o755)
	app.loadSettings()

	fmt.Println("Private Muse — setting up the built-in engine…")
	if err := app.ensureEngine(); err != nil {
		fmt.Println("Engine setup failed:", err)
	}
	fmt.Println("Engine ready. Model:", modelByID(app.settings.Model).File)

	app.uiPort = 18080
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(uiHTML)
	})
	mux.HandleFunc("/api/status", app.handleStatus)
	mux.HandleFunc("/api/download", app.handleDownload)
	mux.HandleFunc("/api/start", app.handleStart)
	mux.HandleFunc("/api/settings", app.handleSettings)
	mux.HandleFunc("/api/check-update", app.handleCheckUpdate)
	mux.HandleFunc("/api/update", app.handleUpdate)
	mux.HandleFunc("/api/quit", app.handleQuit)
	mux.HandleFunc("/api/chat", app.handleChat)

	url := fmt.Sprintf("http://127.0.0.1:%d", app.uiPort)
	srv := &http.Server{Handler: mux}
	go func() {
		// After an update swap the old process may still hold the port for a
		// moment — retry the preferred port before falling back to a scan.
		var ln net.Listener
		var err error
		for i := 0; i < 24; i++ {
			ln, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", app.uiPort))
			if err == nil {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		if err != nil {
			p := freePort(app.uiPort + 1)
			ln, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
			if err == nil {
				app.uiPort = p
			}
		}
		if err != nil {
			fmt.Println("UI server error:", err)
			return
		}
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			fmt.Println("UI server error:", err)
		}
	}()

	fmt.Println("Open:", url)
	fmt.Println("Keep this window open while you chat. Close it (or use Settings → Quit) to shut down.")
	noOpen := false
	for _, arg := range os.Args[1:] {
		if arg == "--no-open" {
			noOpen = true
		}
	}
	if !noOpen {
		openWindow(url)
	}
	go cleanupOldExe()
	go func() {
		time.Sleep(6 * time.Second)
		app.checkUpdate()
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	<-sig
	app.stopEngine()
	srv.Close()
}
