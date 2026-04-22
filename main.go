package main

import (
	"archive/tar"
	"bytes"
	"context"
	"embed"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

//go:embed index.html details.html favicon.png
var tmplFS embed.FS

type ResourceKind string

const (
	KindApplication ResourceKind = "applications"
	KindService     ResourceKind = "services"
	KindDatabase    ResourceKind = "databases"
)

type Resource struct {
	Name          string       `yaml:"name"`
	UUID          string       `yaml:"uuid"`
	Kind          ResourceKind `yaml:"kind"`           // "applications", "services", "databases"
	AllowedUsers  []string     `yaml:"allowed_users"`  // usernames from Remote-User
	AllowedGroups []string     `yaml:"allowed_groups"` // groups from Remote-Groups
	Actions       []string     `yaml:"actions"`        // subset of: restart, start, stop
	EditableEnvs  []string     `yaml:"editable_envs"`   // environment variables allowed to be edited
	ExposeAllEnvs bool         `yaml:"expose_all_envs"` // if true, all environment variables are editable
	ConfigFiles   []string     `yaml:"config_files"`    // paths to config files allowed to be edited
	// Optional Docker host this resource's container runs on, e.g.
	// "tcp://docker-proxy:2375" or "tcp://192.168.1.50:2375". Intended for
	// future direct-Docker features; no handler consumes this yet. Leaving
	// it empty keeps behavior unchanged (everything via the Coolify API).
	DockerHost string `yaml:"docker_host"`
}

type Config struct {
	CoolifyURL   string     `yaml:"coolify_url"`
	CoolifyToken string     `yaml:"coolify_token"`
	Resources    []Resource `yaml:"resources"`
	// Optional: URL the "Logout" button points to. Typically the forward-auth
	// proxy's sign-out endpoint, e.g. "/outpost.goauthentik.io/sign_out".
	// If empty, the button is not rendered.
	LogoutURL string `yaml:"logout_url"`
	// If true, dev mode lets you pass ?user=alice&groups=admins instead of auth headers.
	DevMode bool `yaml:"dev_mode"`
}

var (
	cfg             Config
	indexTmpl       *template.Template
	detailsTmpl     *template.Template
	rateLimit       = make(map[string]time.Time)
	rateMu          sync.Mutex
	rateWindow      = 10 * time.Second
	restartRequired = make(map[string]time.Time)
	restartMu       sync.Mutex

	// Reused across all outbound calls to Coolify. Per-request deadlines are
	// applied via context.WithTimeout, not via the client's Timeout field.
	coolifyClient = &http.Client{Timeout: 30 * time.Second}

	// Used for Docker API calls against a socket-proxy endpoint configured
	// via Resource.DockerHost. Per-request deadlines via context.
	dockerClient = &http.Client{Timeout: 30 * time.Second}
)

// startMapCleanup runs a background loop to prune old entries from the rate
// limit and restart-required maps to prevent unbounded memory growth.
func startMapCleanup() {
	go func() {
		for range time.Tick(1 * time.Hour) {
			now := time.Now()
			rateMu.Lock()
			for k, t := range rateLimit {
				if now.Sub(t) > 1*time.Hour {
					delete(rateLimit, k)
				}
			}
			rateMu.Unlock()

			restartMu.Lock()
			for k, t := range restartRequired {
				if now.Sub(t) > 24*time.Hour {
					delete(restartRequired, k)
				}
			}
			restartMu.Unlock()
		}
	}()
}

// validActions is the closed set of verbs we let the UI trigger.
var validActions = []string{"restart", "start", "stop"}

// validateConfig checks the loaded configuration for logical errors or
// unreachable resources and logs warnings/fatals accordingly.
func validateConfig() {
	for _, res := range cfg.Resources {
		if res.Kind != KindApplication && res.Kind != KindService && res.Kind != KindDatabase {
			log.Fatalf("resource %q: unknown kind %q (must be applications, services, or databases)", res.Name, res.Kind)
		}
		if len(res.AllowedUsers) == 0 && len(res.AllowedGroups) == 0 {
			log.Printf("WARNING: resource %q (uuid=%s) has no allowed_users or allowed_groups; it will be unreachable", res.Name, res.UUID)
		}
		for _, action := range res.Actions {
			if !slices.Contains(validActions, action) {
				log.Printf("WARNING: resource %q has unknown action %q", res.Name, action)
			}
		}
	}
}

// callCoolify builds an authenticated request with a per-call timeout derived
// from the inbound request's context. Callers own closing resp.Body.
func callCoolify(ctx context.Context, method, endpoint string, body io.Reader, timeout time.Duration) (*http.Response, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	req, err := http.NewRequestWithContext(cctx, method, endpoint, body)
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.CoolifyToken)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := coolifyClient.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	// Cancel once the body is closed so the timeout context is released.
	resp.Body = &cancelOnCloseBody{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

type cancelOnCloseBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnCloseBody) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// writeJSON serializes v to w; a failed write is logged but not fatal.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	if status != 0 {
		w.WriteHeader(status)
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("writeJSON encode failed: %v", err)
	}
}

func main() {
	// Self-healthcheck mode: the distroless runtime image has no shell or
	// curl, so the Docker HEALTHCHECK runs this binary with -healthcheck,
	// which hits /healthz on localhost and exits 0/1 accordingly.
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		client := &http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get("http://127.0.0.1:8080/healthz")
		if err != nil {
			os.Exit(1)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		os.Exit(0)
	}

	// Config is loaded from a file at CONFIG_PATH (default /config/config.yaml).
	// Mount it into the container or point CONFIG_PATH at a local file.
	configPath := os.Getenv("CONFIG_PATH")
	if configPath == "" {
		configPath = "/config/config.yaml"
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		log.Fatalf("read config %s: %v (mount a config file or set CONFIG_PATH)", configPath, err)
	}
	log.Printf("loading config from %s", configPath)
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		log.Fatalf("parse config: %v", err)
	}
	// Env overrides so you can keep the token out of the yaml.
	if t := os.Getenv("COOLIFY_TOKEN"); t != "" {
		cfg.CoolifyToken = t
	}
	// Note: COOLIFY_URL is a reserved Coolify-injected variable pointing at
	// the *deployed app's own FQDN*, which is not what we want. Use
	// COOLIFY_API_URL to avoid the clash.
	if u := os.Getenv("COOLIFY_API_URL"); u != "" {
		cfg.CoolifyURL = u
	}
	cfg.CoolifyURL = strings.TrimRight(cfg.CoolifyURL, "/")
	if cfg.CoolifyToken == "" || cfg.CoolifyURL == "" {
		log.Fatal("coolify_url and coolify_token are required")
	}

	validateConfig()
	startMapCleanup()

	indexTmpl = template.Must(template.ParseFS(tmplFS, "index.html"))
	detailsTmpl = template.Must(template.ParseFS(tmplFS, "details.html"))

	http.HandleFunc("/", handleIndex)
	http.HandleFunc("/logout", handleLogout)
	http.HandleFunc("/details", handleDetails)
	http.HandleFunc("/action", handleAction)
	http.HandleFunc("/status", handleStatus)
	http.HandleFunc("/envs", handleEnvs)
	http.HandleFunc("/config-file", handleConfigFile)
	http.HandleFunc("/logs", handleLogs)
	http.HandleFunc("/stats", handleStats)
	http.HandleFunc("/coolify-status", handleCoolifyStatus)
	http.HandleFunc("/favicon.ico", handleFavicon)
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })

	addr := ":8080"
	if cfg.DevMode {
		log.Printf("WARNING: dev_mode=true — authentication is BYPASSED; do not run this in production")
	}
	log.Printf("listening on %s (dev_mode=%v, %d resources, coolify_url=%s)", addr, cfg.DevMode, len(cfg.Resources), cfg.CoolifyURL)
	log.Fatal(http.ListenAndServe(addr, nil))
}

func handleLogout(w http.ResponseWriter, r *http.Request) {
	// Forward-auth session cookies live on the auth proxy's domain
	// (e.g. auth.domain.tld), not on the panel's. The browser won't let us
	// clear them from here — only the proxy can. So this handler just
	// redirects to the configured sign-out URL and lets the proxy do the
	// actual invalidation.
	if cfg.LogoutURL != "" {
		log.Printf("logout -> %s", cfg.LogoutURL)
		http.Redirect(w, r, cfg.LogoutURL, http.StatusFound)
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

func handleFavicon(w http.ResponseWriter, r *http.Request) {
	data, err := tmplFS.ReadFile("favicon.png")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Write(data)
}


type user struct {
	Name   string
	Groups []string
}

func getUser(r *http.Request) (user, bool) {
	if cfg.DevMode {
		return user{
			Name:   r.URL.Query().Get("user"),
			Groups: splitCSV(r.URL.Query().Get("groups")),
		}, true
	}
	// Supports both Authentik (X-authentik-*) and Authelia (Remote-*) headers.
	name := r.Header.Get("X-authentik-username")
	groups := r.Header.Get("X-authentik-groups")
	if name == "" {
		name = r.Header.Get("Remote-User")
		groups = r.Header.Get("Remote-Groups")
	}
	if name == "" {
		return user{}, false
	}
	// Authentik uses "|" as separator for groups, Authelia uses ",".
	// splitCSV already handles "," — normalize "|" first.
	groups = strings.ReplaceAll(groups, "|", ",")
	return user{
		Name:   name,
		Groups: splitCSV(groups),
	}, true
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func visibleResources(u user) []Resource {
	out := []Resource{}
	for _, res := range cfg.Resources {
		if userCanSee(u, res) {
			out = append(out, res)
		}
	}
	return out
}

func userCanSee(u user, res Resource) bool {
	if slices.Contains(res.AllowedUsers, u.Name) {
		return true
	}
	for _, g := range u.Groups {
		if slices.Contains(res.AllowedGroups, g) {
			return true
		}
	}
	return false
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	u, ok := getUser(r)
	if !ok || u.Name == "" {
		log.Printf("audit unauthenticated from=%s path=%s", r.RemoteAddr, r.URL.Path)
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	data := struct {
		User      user
		Resources []Resource
		LogoutURL string
	}{u, visibleResources(u), cfg.LogoutURL}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := indexTmpl.Execute(w, data); err != nil {
		log.Printf("template error: %v", err)
	}
}

func handleCoolifyStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	u, ok := getUser(r)
	if !ok || u.Name == "" {
		log.Printf("audit unauthenticated from=%s path=%s", r.RemoteAddr, r.URL.Path)
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}

	// Hit a lightweight auth-required Coolify endpoint to verify both
	// reachability and that our token still works. /api/v1/teams is
	// available on current Coolify and returns 200 with the team list.
	endpoint := cfg.CoolifyURL + "/api/v1/teams"
	resp, err := callCoolify(r.Context(), http.MethodGet, endpoint, nil, 5*time.Second)
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "reason": "unreachable"})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":     false,
			"reason": fmt.Sprintf("http %d", resp.StatusCode),
		})
		return
	}
	// A 200 alone isn't enough — if COOLIFY_URL points at a public domain
	// behind a forward-auth proxy, that proxy returns 200 with its login
	// page. Require a JSON body as a minimal sanity check that we actually
	// reached the Coolify API.
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("coolify status body read failed: %v", err)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") || !json.Valid(body) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "reason": "not coolify (auth proxy?)"})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	uuid := r.URL.Query().Get("uuid")
	res, u := findAndAuthorize(w, r, uuid)
	if res == nil {
		return
	}

	endpoint := fmt.Sprintf("%s/api/v1/%s/%s", cfg.CoolifyURL, res.Kind, uuid)
	resp, err := callCoolify(r.Context(), http.MethodGet, endpoint, nil, 10*time.Second)
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		log.Printf("coolify status call failed for %s (user %s): %v", uuid, u.Name, err)
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "unknown"})
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("coolify status body read failed for %s: %v", uuid, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("coolify status %d for %s (user %s): %.200s", resp.StatusCode, uuid, u.Name, string(body))
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "unknown"})
		return
	}

	var payload struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.Status == "" {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "unknown"})
		return
	}

	s := strings.ToLower(payload.Status)
	if strings.HasPrefix(s, "starting") || strings.HasPrefix(s, "restarting") {
		restartMu.Lock()
		delete(restartRequired, uuid)
		restartMu.Unlock()
	}

	restartMu.Lock()
	_, needsRestart := restartRequired[uuid]
	restartMu.Unlock()

	out := map[string]any{
		"status":           payload.Status,
		"restart_required": needsRestart,
	}
	if res.Kind == "applications" {
		if dep := fetchActiveDeployment(r.Context(), uuid); dep != "" {
			out["deployment"] = dep
		}
	}
	_ = json.NewEncoder(w).Encode(out)
}

// fetchActiveDeployment returns the latest deployment's status for an
// application if it is in an active state (queued / in_progress / pulling),
// otherwise "". Errors are swallowed — deployment info is best-effort and
// must not disturb normal status polling.
func fetchActiveDeployment(ctx context.Context, uuid string) string {
	endpoint := fmt.Sprintf("%s/api/v1/deployments/applications/%s?skip=0&take=1", cfg.CoolifyURL, uuid)
	resp, err := callCoolify(ctx, http.MethodGet, endpoint, nil, 4*time.Second)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ""
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return ""
	}
	var items []struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &items); err != nil || len(items) == 0 {
		return ""
	}
	s := strings.ToLower(strings.TrimSpace(items[0].Status))
	switch s {
	case "queued", "in_progress", "pulling":
		return s
	}
	return ""
}

func handleAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	uuid := r.FormValue("uuid")
	action := r.FormValue("action")

	res, u := findAndAuthorize(w, r, uuid)
	if res == nil {
		return
	}
	if !slices.Contains(res.Actions, action) {
		log.Printf("audit user=%s deny=action action=%s uuid=%s", u.Name, action, uuid)
		http.Error(w, "action not allowed for this resource", http.StatusForbidden)
		return
	}

	// Rate limit: one action per user+uuid per 10s
	rateKey := u.Name + "|" + uuid
	rateMu.Lock()
	if last, ok := rateLimit[rateKey]; ok && time.Since(last) < rateWindow {
		rateMu.Unlock()
		log.Printf("audit user=%s rate_limited uuid=%s action=%s", u.Name, uuid, action)
		http.Error(w, "rate limited, please wait", http.StatusTooManyRequests)
		return
	}
	rateLimit[rateKey] = time.Now()
	rateMu.Unlock()

	endpoint := fmt.Sprintf("%s/api/v1/%s/%s/%s", cfg.CoolifyURL, res.Kind, uuid, action)
	log.Printf("audit user=%s action=%s kind=%s name=%q uuid=%s", u.Name, action, res.Kind, res.Name, uuid)

	resp, err := callCoolify(r.Context(), http.MethodPost, endpoint, nil, 30*time.Second)
	if err != nil {
		log.Printf("coolify action %s failed for %s (user %s): %v", action, uuid, u.Name, err)
		http.Error(w, "coolify unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("coolify action body read failed for %s: %v", uuid, err)
	}

	w.Header().Set("Content-Type", "application/json")
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if action == "restart" || action == "start" {
			restartMu.Lock()
			delete(restartRequired, uuid)
			restartMu.Unlock()
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":      true,
			"message": fmt.Sprintf("%s: %s", action, res.Name),
		})
		return
	}
	log.Printf("coolify action %s %s -> %d (user %s): %.200s", action, uuid, resp.StatusCode, u.Name, string(body))
	w.WriteHeader(http.StatusBadGateway)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":      false,
		"message": fmt.Sprintf("Coolify error (%d)", resp.StatusCode),
	})
}

func handleDetails(w http.ResponseWriter, r *http.Request) {
	uuid := r.URL.Query().Get("uuid")
	res, u := findAndAuthorize(w, r, uuid)
	if res == nil {
		return
	}
	data := struct {
		User      user
		Resource  Resource
		LogoutURL string
	}{u, *res, cfg.LogoutURL}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := detailsTmpl.Execute(w, data); err != nil {
		log.Printf("template error: %v", err)
	}
}

func handleEnvs(w http.ResponseWriter, r *http.Request) {
	uuid := r.URL.Query().Get("uuid")
	if uuid == "" {
		uuid = r.FormValue("uuid")
	}
	res, u := findAndAuthorize(w, r, uuid)
	if res == nil {
		return
	}

	if r.Method == http.MethodGet {
		handleEnvsGet(w, r, u, res)
	} else if r.Method == http.MethodPost {
		handleEnvsPost(w, r, u, res)
	} else {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func handleEnvsGet(w http.ResponseWriter, r *http.Request, u user, res *Resource) {
	endpoint := fmt.Sprintf("%s/api/v1/%s/%s/envs?decrypt=true", cfg.CoolifyURL, res.Kind, res.UUID)
	resp, err := callCoolify(r.Context(), http.MethodGet, endpoint, nil, 10*time.Second)
	if err != nil {
		log.Printf("coolify envs call failed for %s (user %s): %v", res.UUID, u.Name, err)
		http.Error(w, "coolify unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("coolify envs body read failed for %s: %v", res.UUID, err)
	}
	if resp.StatusCode != http.StatusOK {
		log.Printf("coolify envs get %d for %s (user %s): %.200s", resp.StatusCode, res.UUID, u.Name, string(body))
		http.Error(w, "coolify error", http.StatusBadGateway)
		return
	}

	type envVar struct {
		Key   string  `json:"key"`
		Value *string `json:"value"`
	}

	var allEnvs []envVar
	if err := json.Unmarshal(body, &allEnvs); err != nil {
		// Some Coolify versions/resources might return a wrapper object
		var wrapper struct {
			Data []envVar `json:"data"`
		}
		if err2 := json.Unmarshal(body, &wrapper); err2 == nil {
			allEnvs = wrapper.Data
		} else {
			log.Printf("failed to decode envs from %s: %v. Body: %.500s", res.UUID, err, string(body))
			http.Error(w, "failed to decode envs", http.StatusInternalServerError)
			return
		}
	}

	// Filter based on EditableEnvs or expose all
	var filtered []map[string]any
	for _, env := range allEnvs {
		if res.ExposeAllEnvs || slices.Contains(res.EditableEnvs, env.Key) {
			val := ""
			if env.Value != nil {
				val = *env.Value
			}
			filtered = append(filtered, map[string]any{
				"key":   env.Key,
				"value": val,
			})
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"envs": filtered})
}

func handleEnvsPost(w http.ResponseWriter, r *http.Request, u user, res *Resource) {
	key := r.FormValue("key")
	value := r.FormValue("value")

	if !res.ExposeAllEnvs && !slices.Contains(res.EditableEnvs, key) {
		http.Error(w, "env var not editable", http.StatusForbidden)
		return
	}

	endpoint := fmt.Sprintf("%s/api/v1/%s/%s/envs", cfg.CoolifyURL, res.Kind, res.UUID)
	// Sending is_literal: true and is_shown_on_frontend: true (where applicable)
	// can help ensure values are stored as raw strings and returned in future calls.
	payload, _ := json.Marshal(map[string]any{
		"key":                  key,
		"value":                value,
		"is_literal":           true,
		"is_shown_on_frontend": true,
	})

	resp, err := callCoolify(r.Context(), http.MethodPatch, endpoint, strings.NewReader(string(payload)), 10*time.Second)
	if err != nil {
		log.Printf("coolify envs update failed for %s (user %s): %v", res.UUID, u.Name, err)
		http.Error(w, "coolify unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		log.Printf("coolify envs update %d for %s (user %s): %.200s", resp.StatusCode, res.UUID, u.Name, string(body))
		http.Error(w, fmt.Sprintf("coolify error %d", resp.StatusCode), http.StatusBadGateway)
		return
	}

	restartMu.Lock()
	restartRequired[res.UUID] = time.Now()
	restartMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func handleConfigFile(w http.ResponseWriter, r *http.Request) {
	uuid := r.URL.Query().Get("uuid")
	if uuid == "" {
		uuid = r.FormValue("uuid")
	}
	res, u := findAndAuthorize(w, r, uuid)
	if res == nil {
		return
	}

	if res.DockerHost == "" {
		http.Error(w, "config file editing requires docker_host", http.StatusBadRequest)
		return
	}

	if r.Method == http.MethodGet {
		handleConfigFileGet(w, r, u, res)
	} else if r.Method == http.MethodPost {
		handleConfigFilePost(w, r, u, res)
	} else {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func handleConfigFileGet(w http.ResponseWriter, r *http.Request, u user, res *Resource) {
	filePath := r.URL.Query().Get("file")
	if !slices.Contains(res.ConfigFiles, filePath) {
		http.Error(w, "config file not allowed", http.StatusForbidden)
		return
	}

	id, base, err := getContainerID(r.Context(), res.DockerHost, res.UUID)
	if err != nil {
		status := http.StatusBadGateway
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		}
		http.Error(w, "failed to find container: "+err.Error(), status)
		return
	}

	archiveURL := base + "/containers/" + id + "/archive?path=" + url.QueryEscape(filePath)
	resp, err := dockerDo(r.Context(), http.MethodGet, archiveURL, nil, 15*time.Second)
	if err != nil {
		http.Error(w, "failed to get archive: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		http.Error(w, fmt.Sprintf("docker api error (status %d): %s", resp.StatusCode, string(body)), http.StatusBadGateway)
		return
	}

	tr := tar.NewReader(resp.Body)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			http.Error(w, "tar read error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if !hdr.FileInfo().IsDir() {
			content, err := io.ReadAll(tr)
			if err != nil {
				http.Error(w, "read error: "+err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"content": string(content)})
			return
		}
	}
	http.Error(w, "file not found in archive", http.StatusNotFound)
}

func handleConfigFilePost(w http.ResponseWriter, r *http.Request, u user, res *Resource) {
	filePath := r.FormValue("file")
	if !slices.Contains(res.ConfigFiles, filePath) {
		http.Error(w, "config file not allowed", http.StatusForbidden)
		return
	}
	content := r.FormValue("content")

	id, base, err := getContainerID(r.Context(), res.DockerHost, res.UUID)
	if err != nil {
		status := http.StatusBadGateway
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		}
		http.Error(w, "failed to find container: "+err.Error(), status)
		return
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hdr := &tar.Header{
		Name:    strings.TrimPrefix(filePath, "/"),
		Mode:    0644,
		Size:    int64(len(content)),
		ModTime: time.Now(),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		http.Error(w, "tar header error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		http.Error(w, "tar write error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tw.Close(); err != nil {
		http.Error(w, "tar close error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	archiveURL := base + "/containers/" + id + "/archive?path=/"
	resp, err := dockerDo(r.Context(), http.MethodPut, archiveURL, &buf, 15*time.Second)
	if err != nil {
		http.Error(w, "docker put request failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		http.Error(w, fmt.Sprintf("docker api error (status %d): %s", resp.StatusCode, string(body)), http.StatusBadGateway)
		return
	}

	restartMu.Lock()
	restartRequired[res.UUID] = time.Now()
	restartMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// findAndAuthorize does the user/resource/whitelist dance shared by every
// resource-scoped handler. On failure it writes the HTTP error and returns nil.
func findAndAuthorize(w http.ResponseWriter, r *http.Request, uuid string) (*Resource, user) {
	u, ok := getUser(r)
	if !ok || u.Name == "" {
		log.Printf("audit unauthenticated from=%s path=%s", r.RemoteAddr, r.URL.Path)
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return nil, u
	}
	for i := range cfg.Resources {
		if cfg.Resources[i].UUID == uuid {
			res := &cfg.Resources[i]
			if !userCanSee(u, *res) {
				log.Printf("audit user=%s deny=resource uuid=%s path=%s", u.Name, uuid, r.URL.Path)
				http.Error(w, "not found", http.StatusNotFound)
				return nil, u
			}
			return res, u
		}
	}
	log.Printf("audit user=%s deny=resource uuid=%s path=%s", u.Name, uuid, r.URL.Path)
	http.Error(w, "not found", http.StatusNotFound)
	return nil, u
}

// dockerAPIBase converts a user-supplied DockerHost ("tcp://docker-proxy:2375")
// into the HTTP base URL the Docker Engine API expects ("http://docker-proxy:2375").
// Only tcp:// is supported — unix sockets would need a custom Transport and
// the proxy-over-TCP model is the point of this feature.
func dockerAPIBase(host string) (string, error) {
	u, err := url.Parse(host)
	if err != nil {
		return "", fmt.Errorf("parse docker_host %q: %w", host, err)
	}
	if u.Scheme != "tcp" || u.Host == "" {
		return "", fmt.Errorf("docker_host %q must be tcp://host:port", host)
	}
	return "http://" + u.Host, nil
}

// getContainerID finds the container ID for a given resource UUID on a
// specific Docker host. Returns container ID, base URL, and error.
func getContainerID(ctx context.Context, host, uuid string) (string, string, error) {
	base, err := dockerAPIBase(host)
	if err != nil {
		return "", "", err
	}

	filters, _ := json.Marshal(map[string][]string{"name": {uuid}})
	listURL := base + "/containers/json?all=true&filters=" + url.QueryEscape(string(filters))
	listResp, err := dockerDo(ctx, http.MethodGet, listURL, nil, 8*time.Second)
	if err != nil {
		return "", "", fmt.Errorf("docker list: %w", err)
	}
	defer listResp.Body.Close()

	if listResp.StatusCode < 200 || listResp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(listResp.Body, 512))
		return "", "", fmt.Errorf("docker api error (status %d): %s", listResp.StatusCode, strings.TrimSpace(string(body)))
	}

	var containers []struct {
		ID    string   `json:"Id"`
		Names []string `json:"Names"`
	}
	if err := json.NewDecoder(listResp.Body).Decode(&containers); err != nil {
		return "", "", fmt.Errorf("docker list decode: %w", err)
	}

	// Try to find an exact or prefix match in the names
	for _, c := range containers {
		for _, name := range c.Names {
			// Docker names start with /
			trimmed := strings.TrimPrefix(name, "/")
			if trimmed == uuid || strings.HasPrefix(trimmed, uuid+"-") || strings.HasPrefix(trimmed, uuid) {
				return c.ID, base, nil
			}
		}
	}

	if len(containers) > 0 {
		return containers[0].ID, base, nil
	}

	return "", "", fmt.Errorf("container not found for uuid %s", uuid)
}

// dockerContainerLogs fetches the tail of a container's logs via the Docker
// Engine API, finding the container by Coolify's UUID-prefixed name. Returns
// a plain-text string suitable for the existing log-view UI.
func dockerContainerLogs(ctx context.Context, host, uuid string, lines int) (string, error) {
	id, base, err := getContainerID(ctx, host, uuid)
	if err != nil {
		return "", err
	}

	// Fetch logs. Non-TTY containers (the default) return a multiplexed
	// stream with 8-byte frame headers; TTY containers return raw bytes.
	// Tty flag is on the inspect payload, not the list payload — to avoid
	// a third round-trip, we always try to demultiplex and fall back to
	// the raw bytes if the stream doesn't look framed.
	logsURL := fmt.Sprintf("%s/containers/%s/logs?stdout=1&stderr=1&tail=%d&timestamps=0",
		base, id, lines)
	logsResp, err := dockerDo(ctx, http.MethodGet, logsURL, nil, 10*time.Second)
	if err != nil {
		return "", fmt.Errorf("docker logs request: %w", err)
	}
	defer logsResp.Body.Close()
	if logsResp.StatusCode < 200 || logsResp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(logsResp.Body, 512))
		return "", fmt.Errorf("docker logs api error (status %d): %s", logsResp.StatusCode, strings.TrimSpace(string(body)))
	}
	raw, err := io.ReadAll(io.LimitReader(logsResp.Body, 8<<20)) // 8 MiB cap
	if err != nil {
		return "", fmt.Errorf("docker logs read: %w", err)
	}
	return demuxDockerLogs(raw), nil
}

// dockerDo issues an request against the proxy with a context deadline
// wired up the same way callCoolify does it.
func dockerDo(ctx context.Context, method, endpoint string, body io.Reader, timeout time.Duration) (*http.Response, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	req, err := http.NewRequestWithContext(cctx, method, endpoint, body)
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/x-tar")
	}
	resp, err := dockerClient.Do(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("proxy request failed: %w", err)
	}
	resp.Body = &cancelOnCloseBody{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

// demuxDockerLogs strips Docker's 8-byte multiplex frame headers
// (stream_type, 0, 0, 0, size_uint32_be) and concatenates the payloads. If
// the stream doesn't look framed (TTY containers), the raw bytes are
// returned unchanged.
func demuxDockerLogs(raw []byte) string {
	var out strings.Builder
	i := 0
	for i < len(raw) {
		if len(raw)-i < 8 {
			// Trailing non-framed bytes — treat as raw.
			out.Write(raw[i:])
			break
		}
		streamType := raw[i]
		// Valid multiplex stream types are 0 (stdin), 1 (stdout), 2 (stderr).
		// Anything else means this isn't a framed stream.
		if streamType > 2 || raw[i+1] != 0 || raw[i+2] != 0 || raw[i+3] != 0 {
			return string(raw)
		}
		size := binary.BigEndian.Uint32(raw[i+4 : i+8])
		i += 8
		end := i + int(size)
		if end > len(raw) {
			// Truncated frame — write what we have and stop.
			out.Write(raw[i:])
			break
		}
		out.Write(raw[i:end])
		i = end
	}
	return out.String()
}

func handleLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	res, u := findAndAuthorize(w, r, r.URL.Query().Get("uuid"))
	if res == nil {
		return
	}

	lines := 200
	if s := r.URL.Query().Get("lines"); s != "" {
		if n, err := fmt.Sscanf(s, "%d", &lines); err != nil || n != 1 {
			lines = 200
		}
	}
	if lines < 1 {
		lines = 1
	}
	if lines > 1000 {
		lines = 1000
	}

	// Docker-socket-proxy path: when docker_host is set on the resource we
	// pull logs directly from the Docker Engine API. No Coolify fallback —
	// a misconfigured docker_host surfaces as a visible error rather than
	// silently hiding logs.
	if res.DockerHost != "" {
		logs, err := dockerContainerLogs(r.Context(), res.DockerHost, res.UUID, lines)
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			log.Printf("docker logs failed for %s (host=%s user=%s): %v", res.UUID, res.DockerHost, u.Name, err)
			status := http.StatusBadGateway
			if strings.Contains(err.Error(), "not found") {
				status = http.StatusNotFound
			}
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"logs": "", "error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"logs": logs})
		return
	}

	// Coolify-API fallback: only applications expose a logs endpoint.
	if res.Kind != KindApplication {
		http.Error(w, "logs are only available for applications (set docker_host to enable logs for services/databases)", http.StatusBadRequest)
		return
	}

	endpoint := fmt.Sprintf("%s/api/v1/applications/%s/logs?lines=%d", cfg.CoolifyURL, res.UUID, lines)
	resp, err := callCoolify(r.Context(), http.MethodGet, endpoint, nil, 15*time.Second)
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		log.Printf("coolify logs call failed for %s (user %s): %v", res.UUID, u.Name, err)
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]any{"logs": "", "error": "unreachable"})
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("coolify logs body read failed for %s: %v", res.UUID, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("coolify logs %d for %s (user %s): %.200s", resp.StatusCode, res.UUID, u.Name, string(body))
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]any{"logs": "", "error": fmt.Sprintf("http %d", resp.StatusCode)})
		return
	}
	w.Write(body)
}

func handleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	res, _ := findAndAuthorize(w, r, r.URL.Query().Get("uuid"))
	if res == nil {
		return
	}

	if res.DockerHost == "" {
		http.Error(w, "metrics require docker_host", http.StatusBadRequest)
		return
	}

	id, base, err := getContainerID(r.Context(), res.DockerHost, res.UUID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	statsURL := fmt.Sprintf("%s/containers/%s/stats?stream=false", base, id)
	statsResp, err := dockerDo(r.Context(), http.MethodGet, statsURL, nil, 10*time.Second)
	if err != nil {
		http.Error(w, "stats failed", http.StatusBadGateway)
		return
	}
	defer statsResp.Body.Close()

	var ds struct {
		CPUStats struct {
			CPUUsage struct {
				TotalUsage  uint64   `json:"total_usage"`
				PercpuUsage []uint64 `json:"percpu_usage"`
			} `json:"cpu_usage"`
			SystemCPUUsage uint64 `json:"system_cpu_usage"`
			OnlineCPUs     uint64 `json:"online_cpus"`
		} `json:"cpu_stats"`
		PreCPUStats struct {
			CPUUsage struct {
				TotalUsage  uint64   `json:"total_usage"`
				PercpuUsage []uint64 `json:"percpu_usage"`
			} `json:"cpu_usage"`
			SystemCPUUsage uint64 `json:"system_cpu_usage"`
			OnlineCPUs     uint64 `json:"online_cpus"`
		} `json:"precpu_stats"`
		MemoryStats struct {
			Usage uint64 `json:"usage"`
			Stats struct {
				Cache uint64 `json:"cache"`
			} `json:"stats"`
			Limit uint64 `json:"limit"`
		} `json:"memory_stats"`
		Networks map[string]struct {
			RxBytes uint64 `json:"rx_bytes"`
			TxBytes uint64 `json:"tx_bytes"`
		} `json:"networks"`
	}

	if err := json.NewDecoder(statsResp.Body).Decode(&ds); err != nil {
		http.Error(w, "decode failed", http.StatusInternalServerError)
		return
	}

	// Calculate CPU
	cpuDelta := float64(ds.CPUStats.CPUUsage.TotalUsage) - float64(ds.PreCPUStats.CPUUsage.TotalUsage)
	systemDelta := float64(ds.CPUStats.SystemCPUUsage) - float64(ds.PreCPUStats.SystemCPUUsage)
	onlineCPUs := float64(ds.CPUStats.OnlineCPUs)
	if onlineCPUs == 0 {
		onlineCPUs = float64(len(ds.CPUStats.CPUUsage.PercpuUsage))
	}
	if onlineCPUs == 0 {
		onlineCPUs = 1
	}

	cpuPercent := 0.0
	if systemDelta > 0.0 && cpuDelta > 0.0 {
		cpuPercent = (cpuDelta / systemDelta) * onlineCPUs * 100.0
	}

	// Calculate per-core CPU
	perCore := []float64{}
	if systemDelta > 0.0 && len(ds.CPUStats.CPUUsage.PercpuUsage) > 0 && len(ds.CPUStats.CPUUsage.PercpuUsage) == len(ds.PreCPUStats.CPUUsage.PercpuUsage) {
		for i := range ds.CPUStats.CPUUsage.PercpuUsage {
			coreDelta := float64(ds.CPUStats.CPUUsage.PercpuUsage[i]) - float64(ds.PreCPUStats.CPUUsage.PercpuUsage[i])
			if coreDelta > 0 {
				p := (coreDelta / systemDelta) * onlineCPUs * 100.0
				perCore = append(perCore, p)
			} else {
				perCore = append(perCore, 0.0)
			}
		}
	}

	// Calculate Memory (Usage - Cache)
	memUsage := ds.MemoryStats.Usage - ds.MemoryStats.Stats.Cache

	// Calculate Network
	var rx, tx uint64
	for _, n := range ds.Networks {
		rx += n.RxBytes
		tx += n.TxBytes
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"cpu_percent":  cpuPercent,
		"cpu_per_core": perCore,
		"memory_usage": memUsage,
		"memory_limit": ds.MemoryStats.Limit,
		"network_rx":   rx,
		"network_tx":   tx,
	})
}
