// CLI subcommands: config / update / health / version.
// Compiled into the same binary; `foxrouters` with no args still serves.
package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/AlecAivazis/survey/v2"
)

// cliEnvFile returns the .env path for the CLI commands.
// Priority: FOXROUTERS_ENV env → /etc/foxrouters/.env (installer default).
func cliEnvFile() string {
	if p := os.Getenv("FOXROUTERS_ENV"); p != "" {
		return p
	}
	return "/etc/foxrouters/.env"
}

// readEnvFile parses a KEY=VALUE .env file (ignores blank lines and # comments).
func readEnvFile(path string) (map[string]string, error) {
	env := map[string]string{}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return env, nil // not installed yet → empty
		}
		return nil, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.Index(line, "=")
		if eq <= 0 {
			continue
		}
		env[line[:eq]] = line[eq+1:]
	}
	return env, nil
}

// writeEnvFile persists a KEY=VALUE map back to the .env file, preserving
// the original file's comments/order for keys that already exist.
// Does NOT mutate the input map.
func writeEnvFile(path string, env map[string]string) error {
	var out []string
	if data, err := os.ReadFile(path); err == nil {
		seen := map[string]bool{}
		for _, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				continue // skip blanks — no stray empty lines on rewrite
			}
			if strings.HasPrefix(trimmed, "#") {
				out = append(out, line)
				continue
			}
			eq := strings.Index(trimmed, "=")
			if eq <= 0 {
				out = append(out, line)
				continue
			}
			key := trimmed[:eq]
			if v, ok := env[key]; ok {
				out = append(out, key+"="+v)
				seen[key] = true
			} else {
				out = append(out, line)
			}
		}
		// Append new keys (not already in the file) in sorted order
		for _, k := range sortedKeys(env) {
			if !seen[k] {
				out = append(out, k+"="+env[k])
			}
		}
	} else {
		for _, k := range sortedKeys(env) {
			out = append(out, k+"="+env[k])
		}
	}
	return os.WriteFile(path, []byte(strings.Join(out, "\n")+"\n"), 0o600)
}

// writeEnvFileFull rewrites the .env to contain EXACTLY the given keys
// (used by the interactive editor, which holds a full snapshot of the file —
// enables delete, which a partial-update writer cannot express).
func writeEnvFileFull(path string, env map[string]string) error {
	var out []string
	for _, k := range sortedKeys(env) {
		out = append(out, k+"="+env[k])
	}
	return os.WriteFile(path, []byte(strings.Join(out, "\n")+"\n"), 0o600)
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	return keys
}

// sortConfigKeys orders keys for the interactive menu: REDIS_* first (the
// most important gateway settings), then the rest alphabetically.
func sortConfigKeys(m map[string]string) []string {
	keys := sortedKeys(m)
	var redis, rest []string
	for _, k := range keys {
		if strings.HasPrefix(k, "REDIS_") {
			redis = append(redis, k)
		} else {
			rest = append(rest, k)
		}
	}
	return append(redis, rest...)
}

// maskSecret shows the first 4 chars + "…" for sensitive config values.
func maskSecret(v string) string {
	if len(v) <= 8 {
		return "•••"
	}
	return v[:4] + "…" + v[len(v)-4:]
}

// ── config ────────────────────────────────────────────────────────────────

func runConfig(args []string) int {
	path := cliEnvFile()
	env, err := readEnvFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: read %s: %v\n", path, err)
		return 1
	}

	if len(args) == 0 {
		return interactiveConfig(path, env)
	}

	switch args[0] {
	case "list":
		// list — mask known secrets
		secrets := map[string]bool{
			"REDIS_PASSWORD":    true,
			"GATEWAY_API_KEYS":  true,
			"CLOUDFLARED_TOKEN": true,
		}
		fmt.Printf("# %s\n", path)
		for _, k := range sortedKeys(env) {
			v := env[k]
			if secrets[k] {
				v = maskSecret(v)
			}
			fmt.Printf("%s=%s\n", k, v)
		}
		return 0

	case "get":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: foxrouters config get KEY")
			return 1
		}
		v, ok := env[args[1]]
		if !ok {
			return 1 // not set → silent (shell-friendly)
		}
		fmt.Println(v)
		return 0

	case "set":
		if len(args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: foxrouters config set KEY VALUE")
			return 1
		}
		env[args[1]] = args[2]
		if err := writeEnvFile(path, env); err != nil {
			fmt.Fprintf(os.Stderr, "config: write %s: %v\n", path, err)
			return 1
		}
		fmt.Printf("set %s=%s (%s)\n", args[1], maskSecret(args[2]), path)
		return 0

	default:
		fmt.Fprintf(os.Stderr, "config: unknown subcommand %q (use: list|get|set)\n", args[0])
		return 1
	}
}

// ── config (interactive) ──────────────────────────────────────────────────

var cliSecrets = map[string]bool{
	"REDIS_PASSWORD":    true,
	"GATEWAY_API_KEYS":  true,
	"CLOUDFLARED_TOKEN": true,
}

// cliEnvTemplate is the default key set offered when the .env is empty
// (first run) — mirror of .env.example so the editor never shows a blank menu.
var cliEnvTemplate = []struct{ key, def, hint string }{
	{"PORT", "20130", ""},
	{"GATEWAY_BIND", "127.0.0.1", ""},
	{"REDIS_ADDR", "127.0.0.1:6379", ""},
	{"REDIS_PASSWORD", "", "secret"},
	{"GATEWAY_API_KEYS", "", "comma-separated gw-* keys"},
	{"LOG_BACKEND", "sqlite", ""},
	{"LOG_SQLITE_PATH", "/var/lib/foxrouters/logs.db", ""},
	{"CLOUDFLARED_PATH", "/usr/local/bin/cloudflared", ""},
	{"ALIBABA_DISABLED", "", ""},
	{"GROK_DISABLED", "", ""},
	{"CODEBUDDY_DISABLED", "", ""},
	{"FREEBUFF_DISABLED", "", ""},
}

// ── config (interactive) ──────────────────────────────────────────────────
// Rendered with AlecAivazis/survey (battle-tested TUI: arrow keys, resize,
// unicode, masked passwords handled by the library — no hand-rolled raw mode).

// isTTY reports whether stdin is an interactive terminal (not a pipe).
func isTTY() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// interactiveConfig is the arrow-key menu editor for the .env file.
// Runs only when stdin is a TTY; piped/non-TTY calls fall back to
// `config list` so scripts can't hang.
func interactiveConfig(path string, env map[string]string) int {
	if !isTTY() {
		fmt.Fprintf(os.Stderr, "config: interactive mode needs a TTY — use: foxrouters config list/get/set\n")
		return 1
	}
	keys := sortConfigKeys(env)
	if len(keys) == 0 {
		// First run on an empty/missing .env — offer the template key set so
		// the editor is usable immediately (nothing is written until edited).
		for _, t := range cliEnvTemplate {
			env[t.key] = t.def
		}
		keys = sortConfigKeys(env)
		fmt.Println("template loaded — edit & fill (written on first change)")
	}

	for {
		// main menu: keys (masked) + actions
		options := make([]string, 0, len(keys)+3)
		for _, k := range keys {
			v := env[k]
			if cliSecrets[k] {
				v = maskSecret(v)
			}
			options = append(options, fmt.Sprintf("%-24s = %s", k, v))
		}
		options = append(options, "＋ Add new key", "🗑 Delete key", "✕ Quit")

		sel := 0
		if err := survey.AskOne(&survey.Select{
			Message: "FoxRouters config — " + path,
			Options: options,
			Help:    "↑/↓ select · Enter to act",
		}, &sel); err != nil {
			return 0 // ctrl-c / esc
		}

		switch {
		case sel < len(keys):
			// edit selected key
			k := keys[sel]
			var val string
			var q survey.Prompt
			if cliSecrets[k] {
				q = &survey.Password{Message: "New value for " + k + " (empty = cancel):"}
			} else {
				q = &survey.Input{Message: "New value for " + k + " (empty = cancel):"}
			}
			if err := survey.AskOne(q, &val); err != nil {
				return 0
			}
			if val == "" {
				fmt.Println("cancelled")
				continue
			}
			env[k] = val
			writeEnvFileFull(path, env)
			fmt.Printf("✓ %s saved\n", k)

		case sel == len(keys):
			// add new key
			var name string
			if err := survey.AskOne(&survey.Input{Message: "Key name (empty = cancel):"}, &name); err != nil {
				return 0
			}
			if name == "" {
				continue
			}
			var val string
			var q survey.Prompt
			if cliSecrets[name] {
				q = &survey.Password{Message: "Value for " + name + ":"}
			} else {
				q = &survey.Input{Message: "Value for " + name + " (empty = cancel):"}
			}
			if err := survey.AskOne(q, &val); err != nil {
				return 0
			}
			if val == "" {
				continue
			}
			env[name] = val
			keys = sortConfigKeys(env)
			writeEnvFileFull(path, env)
			fmt.Printf("✓ %s saved\n", name)

		case sel == len(keys)+1:
			// delete selected key (sub-select + confirm)
			if len(keys) == 0 {
				continue
			}
			delOpts := make([]string, len(keys))
			for i, k := range keys {
				delOpts[i] = k
			}
			delOpts = append(delOpts, "— cancel")
			del := len(keys)
			if err := survey.AskOne(&survey.Select{Message: "Delete key:", Options: delOpts}, &del); err != nil {
				return 0
			}
			if del >= len(keys) {
				continue // cancel
			}
			name := keys[del]
			confirm := false
			if err := survey.AskOne(&survey.Confirm{Message: "Delete " + name + "?", Default: false}, &confirm); err != nil {
				return 0
			}
			if !confirm {
				continue
			}
			delete(env, name)
			keys = sortConfigKeys(env)
			writeEnvFileFull(path, env)
			fmt.Printf("✓ deleted %s\n", name)

		default:
			// quit
			return 0
		}
	}
}

// ── install ───────────────────────────────────────────────────────────────

// genGatewayKey mints a gateway API key (same shape as the gateway's
// AuthManager: "gw-" + 32 base64url chars).
func genGatewayKey() (string, error) {
	buf := make([]byte, 24) // 24 bytes → 32 base64 chars
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "gw-" + base64.RawURLEncoding.EncodeToString(buf), nil
}

// redisReachable does a quick TCP dial to the configured Redis address.
func redisReachable(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// copySelf copies the running binary to /usr/local/bin/foxrouters.
func copySelf() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	dst := "/usr/local/bin/foxrouters"
	if exe == dst {
		return dst, nil // already installed in place
	}
	src, err := os.Open(exe)
	if err != nil {
		return "", err
	}
	defer src.Close()
	tmp := dst + ".new"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(out, src); err != nil {
		out.Close()
		os.Remove(tmp)
		return "", err
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return "", err
	}
	return dst, nil
}

// runInstall is the interactive first-install wizard: asks for the minimal
// settings (port + Redis + gateway key), writes /etc/foxrouters/.env,
// installs the binary and optionally the hardened systemd unit.
func runInstall(args []string) int {
	if !isTTY() {
		fmt.Fprintf(os.Stderr, "install: interactive mode needs a TTY — run it directly in a terminal (as root)\n")
		return 1
	}

	// 1) wizard — the only required settings: port + Redis.
	var cfg struct {
		Port         string
		RedisAddr    string
		RedisPass    string
		APIKeys      string
		EnableSystem bool
	}
	if err := survey.Ask([]*survey.Question{
		{Name: "Port", Prompt: &survey.Input{Message: "Gateway port", Default: "20130"}},
		{Name: "RedisAddr", Prompt: &survey.Input{Message: "Redis address", Default: "127.0.0.1:6379"}},
		{Name: "RedisPass", Prompt: &survey.Password{Message: "Redis password (empty if no password)"}},
		{Name: "APIKeys", Prompt: &survey.Input{Message: "Gateway API keys (comma-separated, empty = auto-generate)"}},
		{Name: "EnableSystem", Prompt: &survey.Confirm{Message: "Install systemd service + start now?", Default: true}},
	}, &cfg); err != nil {
		return 0 // ctrl-c
	}

	// 2) derive values
	port := strings.TrimSpace(cfg.Port)
	if port == "" {
		port = "20130"
	}
	redisAddr := strings.TrimSpace(cfg.RedisAddr)
	if redisAddr == "" {
		redisAddr = "127.0.0.1:6379"
	}
	apiKeys := strings.TrimSpace(cfg.APIKeys)
	if apiKeys == "" {
		key, err := genGatewayKey()
		if err != nil {
			fmt.Fprintf(os.Stderr, "install: key generation failed: %v\n", err)
			return 1
		}
		apiKeys = key
	}

	// 3) Redis reachability hint (not fatal)
	if !redisReachable(redisAddr) {
		fmt.Printf("⚠ Redis %s unreachable — make sure Redis is running before starting the service\n", redisAddr)
	}

	// 4) write .env
	path := cliEnvFile()
	env := map[string]string{
		"PORT":             port,
		"GATEWAY_BIND":     "127.0.0.1:" + port,
		"REDIS_ADDR":       redisAddr,
		"REDIS_PASSWORD":   cfg.RedisPass,
		"GATEWAY_API_KEYS": apiKeys,
		"LOG_BACKEND":      "sqlite",
		"LOG_SQLITE_PATH":  "/var/lib/foxrouters/logs.db",
		"CLOUDFLARED_PATH": "/usr/local/bin/cloudflared",
	}
	if err := writeEnvFileFull(path, env); err != nil {
		fmt.Fprintf(os.Stderr, "install: write %s failed: %v\n", path, err)
		return 1
	}
	os.MkdirAll("/var/lib/foxrouters", 0o755)
	fmt.Printf("✓ Config: %s\n", path)

	// 5) install binary (needs root for /usr/local/bin)
	if os.Geteuid() != 0 {
		fmt.Println("⚠ not root — skipping binary/systemd install. Copy the binary and run `foxrouters serve` manually.")
		return 0
	}
	if dst, err := copySelf(); err != nil {
		fmt.Fprintf(os.Stderr, "install: copy binary failed: %v\n", err)
		return 1
	} else {
		fmt.Printf("✓ Binary: %s\n", dst)
	}

	// 6) systemd unit (optional)
	if cfg.EnableSystem {
		if err := installSystemdUnit(path); err != nil {
			fmt.Fprintf(os.Stderr, "install: systemd failed: %v\n", err)
			return 1
		}
		if err := exec.Command("systemctl", "restart", "foxrouters").Run(); err != nil {
			fmt.Fprintf(os.Stderr, "install: start service failed: %v\n", err)
			return 1
		}
		fmt.Println("✓ Service: foxrouters.service (started)")
		time.Sleep(2 * time.Second)
		if resp, err := http.Get("http://127.0.0.1:" + port + "/health"); err == nil {
			resp.Body.Close()
			fmt.Printf("✓ Health: HTTP %d on :%s\n", resp.StatusCode, port)
		} else {
			fmt.Printf("⚠ Health check not OK yet: %v\n", err)
		}
	}

	fmt.Println("\nDone. Gateway key (keep it safe):")
	fmt.Println("  " + apiKeys)
	fmt.Println("\nManage config: foxrouters config · Update: foxrouters update")
	return 0
}

// installSystemdUnit writes the hardened systemd unit (mirror of install.sh).
func installSystemdUnit(envFile string) error {
	unit := `[Unit]
Description=FoxRouters AI Gateway (Grok + CodeBuddy + Freebuff + Alibaba)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=` + envFile + `
Environment=HOME=/var/lib/foxrouters
ExecStart=/usr/local/bin/foxrouters
Restart=on-failure
RestartSec=3
# Hardening (mirrors the Docker image)
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=/var/lib/foxrouters

[Install]
WantedBy=multi-user.target
`
	if err := os.WriteFile("/etc/systemd/system/foxrouters.service", []byte(unit), 0o644); err != nil {
		return err
	}
	if err := exec.Command("systemctl", "daemon-reload").Run(); err != nil {
		return err
	}
	exec.Command("systemctl", "enable", "foxrouters").Run() // best-effort
	return nil
}

// ── stop ──────────────────────────────────────────────────────────────────

// findStopTarget detects how the gateway is running:
// returns (method, name) where method ∈ {"systemd","docker","pid",""}.
func findStopTarget(port string) (string, string) {
	// 1) systemd unit active?
	if err := exec.Command("systemctl", "is-active", "foxrouters").Run(); err == nil {
		return "systemd", "foxrouters.service"
	}
	// 2) docker container (prod name "foxrouters" or "<hash>_foxrouters";
	//    never the dev container)
	if out, err := exec.Command("docker", "ps", "--format", "{{.Names}}").Output(); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			name := strings.TrimSpace(line)
			if name == "" {
				continue
			}
			if (name == "foxrouters" || strings.HasSuffix(name, "_foxrouters")) && !strings.Contains(name, "dev") {
				return "docker", name
			}
		}
	}
	// 3) PID listening on the configured port
	out, err := exec.Command("ss", "-tlnp", "sport", "=", ":"+port).Output()
	if err == nil {
		if pid := parseSSPID(string(out)); pid != "" {
			return "pid", pid
		}
	}
	return "", ""
}

// parseSSPID extracts the PID from an `ss -tlnp` line: users:(("foxrouters",pid=N,fd=..))
func parseSSPID(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, "pid=") {
			continue
		}
		i := strings.Index(line, "pid=")
		rest := line[i+4:]
		for _, c := range rest {
			if c >= '0' && c <= '9' {
				j := 0
				for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
					j++
				}
				return rest[:j]
			}
			break
		}
	}
	return ""
}

// runStop stops the gateway server: systemd unit, Docker container, or
// PID on the configured port (in that order). --dry-run prints the target
// without touching anything.
func runStop(args []string) int {
	dryRun := false
	for _, a := range args {
		if a == "--dry-run" {
			dryRun = true
		}
	}
	port := "20130"
	if env, err := readEnvFile(cliEnvFile()); err == nil && env["PORT"] != "" {
		port = env["PORT"]
	}

	method, name := findStopTarget(port)
	switch method {
	case "systemd":
		if dryRun {
			fmt.Println("would stop: systemd foxrouters.service")
			return 0
		}
		if err := exec.Command("systemctl", "stop", "foxrouters").Run(); err != nil {
			fmt.Fprintf(os.Stderr, "stop: systemctl stop failed: %v\n", err)
			return 1
		}
		fmt.Println("✓ stopped: foxrouters.service")
		return 0
	case "docker":
		if dryRun {
			fmt.Printf("would stop: docker container %s\n", name)
			return 0
		}
		if err := exec.Command("docker", "stop", name).Run(); err != nil {
			fmt.Fprintf(os.Stderr, "stop: docker stop %s failed: %v\n", name, err)
			return 1
		}
		fmt.Printf("✓ stopped: docker container %s\n", name)
		return 0
	case "pid":
		if dryRun {
			fmt.Printf("would stop: PID %s on :%s\n", name, port)
			return 0
		}
		proc, err := os.FindProcess(atoiOrZero(name))
		if err != nil {
			fmt.Fprintf(os.Stderr, "stop: process lookup failed: %v\n", err)
			return 1
		}
		if err := proc.Signal(syscall.SIGTERM); err != nil {
			fmt.Fprintf(os.Stderr, "stop: kill %s failed: %v\n", name, err)
			return 1
		}
		fmt.Printf("✓ stopped: PID %s on :%s (SIGTERM)\n", name, port)
		return 0
	default:
		fmt.Printf("foxrouters doesn't seem to be running (no active systemd unit, no docker container, nothing on :%s)\n", port)
		return 1
	}
}

// atoiOrZero is a tiny int parse for PIDs (never errors).
func atoiOrZero(s string) int {
	var n int
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// ── update ────────────────────────────────────────────────────────────────

const ghAPI = "https://api.github.com/repos/rilspratama/Foxrouters"

type ghRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

func ghGet(url string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "foxrouters-updater/"+Version)
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github %s: HTTP %d", url, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// ghLatestRelease fetches release metadata for the given tag ("" → latest).
// FOXROUTERS_GH_API overrides the base for testing (e.g. a local mock server).
func ghLatestRelease(tag string) (*ghRelease, error) {
	base := os.Getenv("FOXROUTERS_GH_API")
	if base == "" {
		base = ghAPI
	}
	url := base + "/releases/latest"
	if tag != "" {
		url = base + "/releases/tags/" + tag
	}
	body, err := ghGet(url)
	if err != nil {
		return nil, err
	}
	var rel ghRelease
	if err := json.Unmarshal(body, &rel); err != nil {
		return nil, err
	}
	return &rel, nil
}

func runUpdate(args []string) int {
	tag := ""
	for _, a := range args {
		if strings.HasPrefix(a, "--tag=") {
			tag = strings.TrimPrefix(a, "--tag=")
		}
	}

	fmt.Printf("foxrouters %s — checking %s release…\n", Version,
		map[bool]string{true: tag, false: "latest"}[tag != ""])
	rel, err := ghLatestRelease(tag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "update: %v\n", err)
		return 1
	}
	if rel.TagName == "" {
		fmt.Fprintln(os.Stderr, "update: release has no tag")
		return 1
	}

	// GoReleaser asset name: foxrouters_<version>_<os>_<arch>.tar.gz
	ver := strings.TrimPrefix(rel.TagName, "v")
	want := fmt.Sprintf("foxrouters_%s_%s_%s.tar.gz", ver, runtime.GOOS, runtime.GOARCH)
	var assetURL, checksumsURL string
	for _, a := range rel.Assets {
		if a.Name == want {
			assetURL = a.BrowserDownloadURL
		}
		if a.Name == "checksums.txt" {
			checksumsURL = a.BrowserDownloadURL
		}
	}
	if assetURL == "" {
		fmt.Fprintf(os.Stderr, "update: no asset %s in release %s\n", want, rel.TagName)
		return 1
	}

	tmp, err := os.MkdirTemp("", "foxrouters-update-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "update: %v\n", err)
		return 1
	}
	defer os.RemoveAll(tmp)

	// Download + verify against checksums.txt (when present)
	archivePath := filepath.Join(tmp, want)
	if err := downloadFile(assetURL, archivePath); err != nil {
		fmt.Fprintf(os.Stderr, "update: download: %v\n", err)
		return 1
	}
	if checksumsURL != "" {
		csPath := filepath.Join(tmp, "checksums.txt")
		if err := downloadFile(checksumsURL, csPath); err == nil {
			if err := verifyChecksum(csPath, want, archivePath); err != nil {
				fmt.Fprintf(os.Stderr, "update: %v\n", err)
				return 1
			}
			fmt.Println("checksum verified ✓")
		} else {
			fmt.Printf("update: warning: checksums.txt unavailable (%v) — continuing\n", err)
		}
	}

	// Extract the new binary
	newBin, err := extractBinary(archivePath, "foxrouters")
	if err != nil {
		fmt.Fprintf(os.Stderr, "update: extract: %v\n", err)
		return 1
	}
	defer os.Remove(newBin)

	// Replace the running binary path
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "update: resolve executable: %v\n", err)
		return 1
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		self, _ = os.Executable()
	}
	if err := replaceBinary(newBin, self); err != nil {
		fmt.Fprintf(os.Stderr, "update: replace: %v\n", err)
		return 1
	}
	fmt.Printf("installed %s → %s\n", rel.TagName, self)

	// Restart systemd service when present; otherwise tell the user.
	if code := restartService(); code != 0 {
		fmt.Println("restart manually: systemctl restart foxrouters")
		return code
	}
	return 0
}

func downloadFile(url, dest string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d for %s", resp.StatusCode, url)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

// verifyChecksum checks that the downloaded archive matches its entry in
// checksums.txt (GoReleaser format: "<sha256>  <filename>").
func verifyChecksum(checksumsPath, name, filePath string) error {
	data, err := os.ReadFile(checksumsPath)
	if err != nil {
		return err
	}
	want := ""
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasSuffix(line, "  "+name) || strings.HasSuffix(line, " "+name) {
			want = strings.Fields(line)[0]
			break
		}
	}
	if want == "" {
		return fmt.Errorf("checksums.txt: no entry for %s", name)
	}
	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("SHA-256 mismatch for %s: got %s, want %s", name, got, want)
	}
	return nil
}

// extractBinary pulls the named file out of a GoReleaser .tar.gz archive.
func extractBinary(archivePath, want string) (string, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		if hdr.Typeflag != tar.TypeReg || filepath.Base(hdr.Name) != want {
			continue
		}
		out, err := os.CreateTemp("", "foxrouters-new-")
		if err != nil {
			return "", err
		}
		if _, err := io.Copy(out, tr); err != nil {
			out.Close()
			os.Remove(out.Name())
			return "", err
		}
		out.Close()
		os.Chmod(out.Name(), 0o755)
		return out.Name(), nil
	}
	return "", fmt.Errorf("archive %s: %s not found", archivePath, want)
}

// replaceBinary atomically swaps in the new binary (rename; Windows-safe
// fallback via remove+rename).
func replaceBinary(newBin, self string) error {
	if err := os.Rename(newBin, self); err == nil {
		return nil
	}
	// Old binary may be running on Windows → can't overwrite; drop-in replace
	// via temp name + rename is still the best effort.
	backup := self + ".old"
	os.Remove(backup)
	if err := os.Rename(self, backup); err != nil {
		return err
	}
	if err := os.Rename(newBin, self); err != nil {
		os.Rename(backup, self) // rollback
		return err
	}
	os.Remove(backup)
	return nil
}

// restartService restarts the systemd unit when it exists AND is active;
// returns 0 on success/absence (Docker/dev-managed instances are skipped),
// non-zero only when an active systemd unit failed to restart.
func restartService() int {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return 0
	}
	if _, err := os.Stat("/etc/systemd/system/foxrouters.service"); err != nil {
		return 0 // not systemd-managed (docker/dev) → nothing to do
	}
	// Only restart an ACTIVE unit. A leftover unit file (e.g. Docker
	// deployment) must never be touched — restarting it can port-conflict.
	if err := exec.Command("systemctl", "is-active", "--quiet", "foxrouters").Run(); err != nil {
		fmt.Println("update: foxrouters.service not active — skipping systemd restart (docker-managed?)")
		return 0
	}
	if err := exec.Command("systemctl", "restart", "foxrouters").Run(); err != nil {
		fmt.Printf("update: systemctl restart failed: %v\n", err)
		return 1
	}
	fmt.Println("service restarted ✓")
	return 0
}

// ── health ────────────────────────────────────────────────────────────────

func runHealth(args []string) int {
	_ = args
	port := os.Getenv("PORT")
	if port == "" {
		if env, err := readEnvFile(cliEnvFile()); err == nil {
			port = env["PORT"]
		}
	}
	if port == "" {
		port = DEFAULT_PORT
	}
	url := "http://127.0.0.1:" + port + "/health"
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "health: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	fmt.Printf("HTTP %d %s\n", resp.StatusCode, strings.TrimSpace(string(body)))
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
