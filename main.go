package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
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
	EditableEnvs  []string     `yaml:"editable_envs"`  // environment variables allowed to be edited
	ExposeAllEnvs bool         `yaml:"expose_all_envs"` // if true, all environment variables are editable
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

	// Config can come from two places:
	//   1. The CONFIG_YAML env var (whole yaml contents as a string) — preferred
	//      in container platforms like Coolify.
	//   2. A file at CONFIG_PATH (defaults to /config/config.yaml) — useful for
	//      local development or volume-mounted setups.
	var data []byte
	if inline := os.Getenv("CONFIG_YAML"); inline != "" {
		data = []byte(inline)
		log.Print("loading config from CONFIG_YAML env var")
	} else {
		configPath := os.Getenv("CONFIG_PATH")
		if configPath == "" {
			configPath = "/config/config.yaml"
		}
		var err error
		data, err = os.ReadFile(configPath)
		if err != nil {
			log.Fatalf("read config %s: %v (set CONFIG_YAML env var or mount a config file)", configPath, err)
		}
		log.Printf("loading config from %s", configPath)
	}
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
	http.HandleFunc("/logs", handleLogs)
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

	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":           payload.Status,
		"restart_required": needsRestart,
	})
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

func handleLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	res, u := findAndAuthorize(w, r, r.URL.Query().Get("uuid"))
	if res == nil {
		return
	}
	if res.Kind != KindApplication {
		http.Error(w, "logs are only available for applications", http.StatusBadRequest)
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
