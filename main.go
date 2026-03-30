package main

import (
	"log"
	"net/http"
	"os"
	"strconv"
)

// Config holds all runtime configuration loaded from environment variables.
// Each field maps to a corresponding environment variable; see loadConfig for defaults.
type Config struct {
	// Port is the TCP port the HTTP server listens on (env: PORT, default: "8080").
	Port string

	// BaseURL is the scheme+host prefix used when constructing short URLs
	// (env: BASE_URL). When BaseURLOverride is false this field is empty and
	// the base URL is derived from each incoming request instead.
	BaseURL string

	// BaseURLOverride is true when the BASE_URL environment variable was
	// explicitly set. Handlers use this to decide whether to use the static
	// BaseURL or to derive the domain from the request host.
	BaseURLOverride bool

	// StorePath is the file-system path for the JSON persistence file
	// (env: STORE_PATH, default: "urls.json").
	StorePath string

	// AdCountdown is the number of seconds the interstitial ad page counts
	// down before automatically forwarding the visitor (env: AD_COUNTDOWN,
	// default: 5).
	AdCountdown int
}

// loadConfig reads configuration from environment variables and returns a
// populated Config. Missing or invalid values fall back to sensible defaults:
//
//   - PORT          → "8080"
//   - BASE_URL      → derived per-request from the Host header (no static default)
//   - STORE_PATH    → "urls.json"
//   - AD_COUNTDOWN  → 5 seconds
func loadConfig() *Config {
	countdown := 5
	if v := os.Getenv("AD_COUNTDOWN"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			countdown = n
		}
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	baseURL := os.Getenv("BASE_URL")
	baseURLOverride := baseURL != ""

	storePath := os.Getenv("STORE_PATH")
	if storePath == "" {
		storePath = "urls.json"
	}

	return &Config{
		Port:            port,
		BaseURL:         baseURL,
		BaseURLOverride: baseURLOverride,
		StorePath:       storePath,
		AdCountdown:     countdown,
	}
}

// main loads configuration, initialises the persistent store and HTTP handlers,
// registers all routes, and starts the HTTP server. The process exits with a
// non-zero status if any initialisation step fails.
func main() {
	cfg := loadConfig()

	store, err := NewJSONStore(cfg.StorePath)
	if err != nil {
		log.Fatalf("failed to open store: %v", err)
	}

	handlers, err := NewHandlers(cfg, store)
	if err != nil {
		log.Fatalf("failed to load templates: %v", err)
	}

	mux := http.NewServeMux()
	RegisterRoutes(mux, handlers)

	baseDisplay := cfg.BaseURL
	if !cfg.BaseURLOverride {
		baseDisplay = "(derived from request host)"
	}
	log.Printf("URL Shortener running on :%s (base: %s)", cfg.Port, baseDisplay)
	log.Printf("Ad countdown: %d seconds | Store: %s", cfg.AdCountdown, cfg.StorePath)

	if err := http.ListenAndServe(":"+cfg.Port, mux); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
