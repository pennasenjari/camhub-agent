package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	_ "embed"
	"runtime"
)

//go:embed web/index.html
var indexHTML []byte

//go:embed web/app.js
var appJS []byte

//go:embed web/styles.css
var stylesCSS []byte

type Config struct {
	CamhubURL         string
	AuthToken         string
	MediaMtxRtspBase  string
	HeartbeatInterval time.Duration
	DiscoveryInterval time.Duration
	FfmpegPath        string
	AgentAddr         string
	StateFile         string
	RestartDelay      time.Duration
	RegisterUserAgent string
	RegisterTimeout   time.Duration
}

type DeviceInfo struct {
	Name string `json:"name"`
	Node string `json:"node"`
}

type Camera struct {
	DeviceUID  string `json:"deviceUid"`
	Name       string `json:"name"`
	Node       string `json:"node"`
	StreamPath string `json:"streamPath"`
	RtspURL    string `json:"rtspUrl"`
	Enabled    bool   `json:"enabled"`
	Publishing bool   `json:"publishing"`
}

type Agent struct {
	cfg        Config
	hostname   string
	mu         sync.Mutex
	cameras    map[string]*Camera
	publishers map[string]*exec.Cmd
	state      map[string]bool
}

func main() {
	cfg := loadConfig()
	hostname, _ := os.Hostname()

	agent := &Agent{
		cfg:        cfg,
		hostname:   hostname,
		cameras:    make(map[string]*Camera),
		publishers: make(map[string]*exec.Cmd),
		state:      loadState(cfg.StateFile),
	}

	agent.refreshCameras()

	go agent.discoveryLoop()
	go agent.heartbeatLoop()

	mux := http.NewServeMux()
	mux.HandleFunc("/", serveIndex)
	mux.HandleFunc("/app.js", serveJS)
	mux.HandleFunc("/styles.css", serveCSS)
	mux.HandleFunc("/api/cameras", agent.handleCameras)
	mux.HandleFunc("/api/cameras/toggle", agent.handleToggle)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	server := &http.Server{
		Addr:    cfg.AgentAddr,
		Handler: mux,
	}

	logInfo("agent listening on %s", cfg.AgentAddr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logInfo("http server error: %v", err)
	}
}

func loadConfig() Config {
	envPath := filepath.Join(".", ".env")
	_ = loadDotEnv(envPath)

	return Config{
		CamhubURL:         getEnv("CAMHUB_URL", "http://localhost:3001"),
		AuthToken:         getEnv("AUTH_TOKEN", ""),
		MediaMtxRtspBase:  getEnv("MEDIAMTX_RTSP_BASE", "rtsp://localhost:8554"),
		HeartbeatInterval: getEnvDuration("HEARTBEAT_MS", 10000*time.Millisecond),
		DiscoveryInterval: getEnvDuration("DISCOVERY_INTERVAL_MS", 15000*time.Millisecond),
		FfmpegPath:        getEnv("FFMPEG_PATH", "ffmpeg"),
		AgentAddr:         getEnv("AGENT_ADDR", "0.0.0.0:8091"),
		StateFile:         getEnv("STATE_FILE", filepath.Join("data", "agent_state.json")),
		RestartDelay:      getEnvDuration("RESTART_DELAY_MS", 2000*time.Millisecond),
		RegisterUserAgent: getEnv("REGISTER_USER_AGENT", "camhub-agent/1.0"),
		RegisterTimeout:   getEnvDuration("REGISTER_TIMEOUT_MS", 5000*time.Millisecond),
	}
}

func (a *Agent) discoveryLoop() {
	ticker := time.NewTicker(a.cfg.DiscoveryInterval)
	defer ticker.Stop()

	for range ticker.C {
		a.refreshCameras()
	}
}

func (a *Agent) heartbeatLoop() {
	ticker := time.NewTicker(a.cfg.HeartbeatInterval)
	defer ticker.Stop()

	for range ticker.C {
		a.registerCameras()
	}
}

func (a *Agent) refreshCameras() {
	devices := discoverDevices()
	sort.Slice(devices, func(i, j int) bool {
		return devices[i].Node < devices[j].Node
	})

	hostSlug := slugify(a.hostname)

	a.mu.Lock()
	defer a.mu.Unlock()

	next := make(map[string]*Camera)
	for idx, device := range devices {
		name := device.Name
		if name == "" {
			name = fmt.Sprintf("Camera %d", idx+1)
		}

		streamPath := fmt.Sprintf("%s-%s-%d", hostSlug, slugify(name), idx)
		deviceUID := fmt.Sprintf("%s:%s", a.hostname, device.Node)
		enabled, ok := a.state[deviceUID]
		if !ok {
			enabled = true
			a.state[deviceUID] = true
		}

		camera := &Camera{
			DeviceUID:  deviceUID,
			Name:       name,
			Node:       device.Node,
			StreamPath: streamPath,
			RtspURL:    fmt.Sprintf("%s/%s", strings.TrimRight(a.cfg.MediaMtxRtspBase, "/"), streamPath),
			Enabled:    enabled,
			Publishing: a.publishers[deviceUID] != nil,
		}

		next[deviceUID] = camera
		if enabled {
			a.ensurePublisherLocked(camera)
		} else {
			a.stopPublisherLocked(deviceUID)
		}
	}

	for uid := range a.cameras {
		if next[uid] == nil {
			a.stopPublisherLocked(uid)
		}
	}

	a.cameras = next
	_ = saveState(a.cfg.StateFile, a.state)
}

func (a *Agent) ensurePublisherLocked(camera *Camera) {
	if a.publishers[camera.DeviceUID] != nil {
		return
	}

	args := []string{
		"-f", "v4l2",
		"-i", camera.Node,
		"-vf", "format=yuv420p",
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-tune", "zerolatency",
		"-profile:v", "baseline",
		"-level:v", "3.1",
		"-pix_fmt", "yuv420p",
		"-f", "rtsp",
		"-rtsp_transport", "tcp",
		camera.RtspURL,
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, a.cfg.FfmpegPath, args...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		logInfo("ffmpeg stderr pipe error for %s: %v", camera.DeviceUID, err)
		cancel()
		return
	}

	if err := cmd.Start(); err != nil {
		logInfo("ffmpeg start failed for %s: %v", camera.DeviceUID, err)
		cancel()
		return
	}

	a.publishers[camera.DeviceUID] = cmd
	camera.Publishing = true

	go func(uid string, stream io.ReadCloser) {
		scanner := bufio.NewScanner(stream)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line != "" {
				logInfo("[ffmpeg:%s] %s", uid, line)
			}
		}
	}(camera.DeviceUID, stderr)

	go func(uid string) {
		err := cmd.Wait()
		cancel()
		a.mu.Lock()
		delete(a.publishers, uid)
		cam := a.cameras[uid]
		enabled := cam != nil && cam.Enabled
		a.mu.Unlock()

		if err != nil {
			logInfo("ffmpeg exited for %s: %v", uid, err)
		}

		if enabled {
			time.Sleep(a.cfg.RestartDelay)
			a.mu.Lock()
			cam = a.cameras[uid]
			if cam != nil && cam.Enabled {
				a.ensurePublisherLocked(cam)
			}
			a.mu.Unlock()
		}
	}(camera.DeviceUID)
}

func (a *Agent) stopPublisherLocked(uid string) {
	cmd := a.publishers[uid]
	if cmd == nil {
		return
	}

	_ = cmd.Process.Signal(os.Interrupt)
	delete(a.publishers, uid)
	if cam := a.cameras[uid]; cam != nil {
		cam.Publishing = false
	}
}

func (a *Agent) registerCameras() {
	a.mu.Lock()
	var cams []map[string]string
	for _, cam := range a.cameras {
		if !cam.Enabled {
			continue
		}
		cams = append(cams, map[string]string{
			"deviceUid":  cam.DeviceUID,
			"name":       cam.Name,
			"rtspUrl":    cam.RtspURL,
			"streamPath": cam.StreamPath,
		})
	}
	a.mu.Unlock()

	payload := map[string]interface{}{
		"host":    a.hostname,
		"cameras": cams,
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(a.cfg.CamhubURL, "/")+"/api/agents/register", bytes.NewReader(body))
	if err != nil {
		logInfo("register request error: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", a.cfg.RegisterUserAgent)
	if a.cfg.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+a.cfg.AuthToken)
	}

	client := &http.Client{Timeout: a.cfg.RegisterTimeout}
	res, err := client.Do(req)
	if err != nil {
		logInfo("register failed: %v", err)
		return
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		body, _ := io.ReadAll(res.Body)
		logInfo("register failed: %s %s", res.Status, strings.TrimSpace(string(body)))
	}
}

func (a *Agent) handleCameras(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	a.mu.Lock()
	list := make([]*Camera, 0, len(a.cameras))
	for _, cam := range a.cameras {
		list = append(list, cam)
	}
	a.mu.Unlock()

	sort.Slice(list, func(i, j int) bool {
		return list[i].Name < list[j].Name
	})

	writeJSON(w, http.StatusOK, list)
}

func (a *Agent) handleToggle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	var payload struct {
		DeviceUID string `json:"deviceUid"`
		Enabled   bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid payload"})
		return
	}

	a.mu.Lock()
	cam := a.cameras[payload.DeviceUID]
	if cam == nil {
		a.mu.Unlock()
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "camera not found"})
		return
	}
	cam.Enabled = payload.Enabled
	a.state[payload.DeviceUID] = payload.Enabled
	if payload.Enabled {
		a.ensurePublisherLocked(cam)
	} else {
		a.stopPublisherLocked(payload.DeviceUID)
	}
	_ = saveState(a.cfg.StateFile, a.state)
	a.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func serveIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

func serveJS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	_, _ = w.Write(appJS)
}

func serveCSS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	_, _ = w.Write(stylesCSS)
}

func writeJSON(w http.ResponseWriter, status int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func discoverDevices() []DeviceInfo {
	if runtime.GOOS != "linux" {
		return nil
	}

	out, err := exec.Command("v4l2-ctl", "--list-devices").Output()
	if err == nil {
		devices := parseV4L2Output(string(out))
		if len(devices) > 0 {
			return devices
		}
	}

	matches, _ := filepath.Glob("/dev/video*")
	sort.Strings(matches)
	devices := make([]DeviceInfo, 0, len(matches))
	for idx, node := range matches {
		devices = append(devices, DeviceInfo{
			Name: fmt.Sprintf("Camera %d", idx+1),
			Node: node,
		})
	}
	return devices
}

func parseV4L2Output(output string) []DeviceInfo {
	blocks := splitBlocks(output)
	var devices []DeviceInfo
	for _, block := range blocks {
		lines := strings.Split(block, "\n")
		var name string
		var node string
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if name == "" {
				name = strings.TrimSuffix(line, ":")
				continue
			}
			if strings.HasPrefix(line, "/dev/video") {
				node = line
				break
			}
		}
		if node != "" {
			devices = append(devices, DeviceInfo{Name: name, Node: node})
		}
	}
	return devices
}

func splitBlocks(input string) []string {
	scanner := bufio.NewScanner(strings.NewReader(input))
	var blocks []string
	var current []string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			if len(current) > 0 {
				blocks = append(blocks, strings.Join(current, "\n"))
				current = nil
			}
			continue
		}
		current = append(current, line)
	}
	if len(current) > 0 {
		blocks = append(blocks, strings.Join(current, "\n"))
	}
	return blocks
}

func slugify(value string) string {
	value = strings.ToLower(value)
	re := regexp.MustCompile(`[^a-z0-9]+`)
	value = re.ReplaceAllString(value, "-")
	value = strings.Trim(value, "-")
	return value
}

func loadState(path string) map[string]bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return map[string]bool{}
	}
	var state map[string]bool
	if err := json.Unmarshal(data, &state); err != nil {
		return map[string]bool{}
	}
	return state
}

func saveState(path string, state map[string]bool) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func loadDotEnv(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		value = strings.Trim(value, `"'`)
		if key != "" {
			if _, exists := os.LookupEnv(key); !exists {
				_ = os.Setenv(key, value)
			}
		}
	}
	return scanner.Err()
}

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok && value != "" {
		return value
	}
	return fallback
}

func getEnvDuration(key string, fallback time.Duration) time.Duration {
	if value, ok := os.LookupEnv(key); ok && value != "" {
		if ms, err := time.ParseDuration(value); err == nil {
			return ms
		}
		if n, err := parseInt(value); err == nil {
			return time.Duration(n) * time.Millisecond
		}
	}
	return fallback
}

func parseInt(value string) (int64, error) {
	var n int64
	for _, r := range value {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("invalid integer: %s", value)
		}
		n = n*10 + int64(r-'0')
	}
	return n, nil
}

func logInfo(format string, args ...interface{}) {
	fmt.Printf(time.Now().Format("2006-01-02 15:04:05")+" "+format+"\n", args...)
}
