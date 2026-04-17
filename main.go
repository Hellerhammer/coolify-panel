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

//go:embed index.html
var tmplFS embed.FS

type ResourceKind string

const (
	KindApplication ResourceKind = "applications"
	KindService     ResourceKind = "services"
	KindDatabase    ResourceKind = "databases"
)

type Resource struct {
	Name         string       `yaml:"name"`
	UUID         string       `yaml:"uuid"`
	Kind         ResourceKind `yaml:"kind"`            // "applications", "services", "databases"
	AllowedUsers []string     `yaml:"allowed_users"`   // usernames from Remote-User
	AllowedGroups []string    `yaml:"allowed_groups"`  // groups from Remote-Groups
	Actions      []string     `yaml:"actions"`         // subset of: restart, start, stop
}

type Config struct {
	CoolifyURL   string     `yaml:"coolify_url"`
	CoolifyToken string     `yaml:"coolify_token"`
	Resources    []Resource `yaml:"resources"`
	// If true, dev mode lets you pass ?user=alice&groups=admins instead of auth headers.
	DevMode bool `yaml:"dev_mode"`
}

var (
	cfg        Config
	indexTmpl  *template.Template
	rateLimit  = make(map[string]time.Time)
	rateMu     sync.Mutex
	rateWindow = 10 * time.Second
)

func main() {
	configPath := os.Getenv("CONFIG_PATH")
	if configPath == "" {
		configPath = "/config/config.yaml"
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		log.Fatalf("read config %s: %v", configPath, err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		log.Fatalf("parse config: %v", err)
	}
	// Env overrides so you can keep the token out of the yaml file.
	if t := os.Getenv("COOLIFY_TOKEN"); t != "" {
		cfg.CoolifyToken = t
	}
	if u := os.Getenv("COOLIFY_URL"); u != "" {
		cfg.CoolifyURL = u
	}
	cfg.CoolifyURL = strings.TrimRight(cfg.CoolifyURL, "/")
	if cfg.CoolifyToken == "" || cfg.CoolifyURL == "" {
		log.Fatal("coolify_url and coolify_token are required")
	}

	indexTmpl = template.Must(template.ParseFS(tmplFS, "index.html"))

	http.HandleFunc("/", handleIndex)
	http.HandleFunc("/action", handleAction)
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })

	addr := ":8080"
	log.Printf("listening on %s (dev_mode=%v, %d resources)", addr, cfg.DevMode, len(cfg.Resources))
	log.Fatal(http.ListenAndServe(addr, nil))
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
	}{u, visibleResources(u)}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := indexTmpl.Execute(w, data); err != nil {
		log.Printf("template error: %v", err)
	}
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
	log.Printf("coolify returned %d: %s", resp.StatusCode, string(body))
	w.WriteHeader(http.StatusBadGateway)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":      false,
		"message": fmt.Sprintf("Coolify error (%d)", resp.StatusCode),
	})
}
