package main

import (
	"log"
	"net/http"
	"os"
	"strconv"
)

// Config holds all runtime configuration loaded from environment variables.
type Config struct {
	Port            string
	BaseURL         string
	BaseURLOverride bool // true when BASE_URL env var was explicitly set
	StorePath       string
	AdCountdown     int
}

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
