package main

import (
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
	cfg         Config
	indexTmpl   *template.Template
	detailsTmpl *template.Template
	rateLimit   = make(map[string]time.Time)
	rateMu      sync.Mutex
	rateWindow  = 10 * time.Second
)

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

	indexTmpl = template.Must(template.ParseFS(tmplFS, "index.html"))
	detailsTmpl = template.Must(template.ParseFS(tmplFS, "details.html"))

	http.HandleFunc("/", handleIndex)
	http.HandleFunc("/details", handleDetails)
	http.HandleFunc("/action", handleAction)
	http.HandleFunc("/status", handleStatus)
	http.HandleFunc("/envs", handleEnvs)
	http.HandleFunc("/coolify-status", handleCoolifyStatus)
	http.HandleFunc("/favicon.ico", handleFavicon)
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })

	addr := ":8080"
	log.Printf("listening on %s (dev_mode=%v, %d resources, coolify_url=%s)", addr, cfg.DevMode, len(cfg.Resources), cfg.CoolifyURL)
	log.Fatal(http.ListenAndServe(addr, nil))
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
		http.Error(w, "unauthenticated (no auth headers)", http.StatusUnauthorized)
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
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}

	// Hit a lightweight auth-required Coolify endpoint to verify both
	// reachability and that our token still works. /api/v1/teams is
	// available on current Coolify and returns 200 with the team list.
	endpoint := cfg.CoolifyURL + "/api/v1/teams"
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, endpoint, nil)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Authorization", "Bearer "+cfg.CoolifyToken)
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
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
	body, _ := io.ReadAll(resp.Body)
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
	u, ok := getUser(r)
	if !ok || u.Name == "" {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}

	uuid := r.URL.Query().Get("uuid")
	var res *Resource
	for i := range cfg.Resources {
		if cfg.Resources[i].UUID == uuid {
			res = &cfg.Resources[i]
			break
		}
	}
	if res == nil || !userCanSee(u, *res) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	endpoint := fmt.Sprintf("%s/api/v1/%s/%s", cfg.CoolifyURL, res.Kind, uuid)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, endpoint, nil)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Authorization", "Bearer "+cfg.CoolifyToken)
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		log.Printf("coolify status call failed: %v", err)
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "unknown"})
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("coolify status %d for %s: %.200s", resp.StatusCode, uuid, string(body))
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
	_ = json.NewEncoder(w).Encode(map[string]any{"status": payload.Status})
}

func handleAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	u, ok := getUser(r)
	if !ok || u.Name == "" {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}

	uuid := r.FormValue("uuid")
	action := r.FormValue("action")

	// Find the resource and verify the user is allowed to touch it + action is whitelisted.
	var res *Resource
	for i := range cfg.Resources {
		if cfg.Resources[i].UUID == uuid {
			res = &cfg.Resources[i]
			break
		}
	}
	if res == nil || !userCanSee(u, *res) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if !slices.Contains(res.Actions, action) {
		http.Error(w, "action not allowed for this resource", http.StatusForbidden)
		return
	}

	// Rate limit: one action per user+uuid per 10s
	rateKey := u.Name + "|" + uuid
	rateMu.Lock()
	if last, ok := rateLimit[rateKey]; ok && time.Since(last) < rateWindow {
		rateMu.Unlock()
		http.Error(w, "rate limited, please wait", http.StatusTooManyRequests)
		return
	}
	rateLimit[rateKey] = time.Now()
	rateMu.Unlock()

	endpoint := fmt.Sprintf("%s/api/v1/%s/%s/%s", cfg.CoolifyURL, res.Kind, uuid, action)
	log.Printf("audit user=%s action=%s kind=%s name=%q uuid=%s", u.Name, action, res.Kind, res.Name, uuid)

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint, nil)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Authorization", "Bearer "+cfg.CoolifyToken)
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("coolify call failed: %v", err)
		http.Error(w, "coolify unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	w.Header().Set("Content-Type", "application/json")
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":      true,
			"message": fmt.Sprintf("%s: %s", action, res.Name),
		})
		return
	}
	log.Printf("coolify action %s %s -> %d: %.200s", action, uuid, resp.StatusCode, string(body))
	w.WriteHeader(http.StatusBadGateway)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":      false,
		"message": fmt.Sprintf("Coolify error (%d)", resp.StatusCode),
	})
}

func handleDetails(w http.ResponseWriter, r *http.Request) {
	u, ok := getUser(r)
	if !ok || u.Name == "" {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	uuid := r.URL.Query().Get("uuid")
	var res *Resource
	for i := range cfg.Resources {
		if cfg.Resources[i].UUID == uuid {
			res = &cfg.Resources[i]
			break
		}
	}
	if res == nil || !userCanSee(u, *res) {
		http.Error(w, "not found", http.StatusNotFound)
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
	u, ok := getUser(r)
	if !ok || u.Name == "" {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}

	if r.Method == http.MethodGet {
		handleEnvsGet(w, r, u)
	} else if r.Method == http.MethodPost {
		handleEnvsPost(w, r, u)
	} else {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func handleEnvsGet(w http.ResponseWriter, r *http.Request, u user) {
	uuid := r.URL.Query().Get("uuid")
	var res *Resource
	for i := range cfg.Resources {
		if cfg.Resources[i].UUID == uuid {
			res = &cfg.Resources[i]
			break
		}
	}
	if res == nil || !userCanSee(u, *res) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	endpoint := fmt.Sprintf("%s/api/v1/%s/%s/envs", cfg.CoolifyURL, res.Kind, uuid)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, endpoint, nil)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Authorization", "Bearer "+cfg.CoolifyToken)
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, "coolify unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	var allEnvs []struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&allEnvs); err != nil {
		http.Error(w, "failed to decode envs", http.StatusInternalServerError)
		return
	}

	// Filter based on EditableEnvs
	var filtered []map[string]string
	for _, env := range allEnvs {
		if slices.Contains(res.EditableEnvs, env.Key) {
			filtered = append(filtered, map[string]string{
				"key":   env.Key,
				"value": env.Value,
			})
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"envs": filtered})
}

func handleEnvsPost(w http.ResponseWriter, r *http.Request, u user) {
	uuid := r.FormValue("uuid")
	key := r.FormValue("key")
	value := r.FormValue("value")

	var res *Resource
	for i := range cfg.Resources {
		if cfg.Resources[i].UUID == uuid {
			res = &cfg.Resources[i]
			break
		}
	}
	if res == nil || !userCanSee(u, *res) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	if !slices.Contains(res.EditableEnvs, key) {
		http.Error(w, "env var not editable", http.StatusForbidden)
		return
	}

	endpoint := fmt.Sprintf("%s/api/v1/%s/%s/envs", cfg.CoolifyURL, res.Kind, uuid)
	payload, _ := json.Marshal(map[string]string{
		"key":   key,
		"value": value,
	})

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPatch, endpoint, strings.NewReader(string(payload)))
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Authorization", "Bearer "+cfg.CoolifyToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, "coolify unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		http.Error(w, fmt.Sprintf("coolify error %d", resp.StatusCode), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}
