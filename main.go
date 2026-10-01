package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // embedded zone database, so TIMEZONE works in minimal containers
)

// shutdownTimeout bounds how long in-flight requests and the final store
// flush may take after SIGINT/SIGTERM.
const shutdownTimeout = 10 * time.Second

// Config holds all runtime configuration loaded from environment variables.
// Each field maps to a corresponding environment variable; see loadConfig for defaults.
type Config struct {
	// Port is the TCP port the HTTP server listens on (env: PORT, default: "8182").
	Port string

	// BaseURL is the scheme+host prefix used when constructing short URLs
	// (env: BASE_URL), without a trailing slash. When BaseURLOverride is
	// false this field is empty and the base URL is derived from each
	// incoming request instead.
	BaseURL string

	// BaseURLOverride is true when the BASE_URL environment variable was
	// explicitly set. Handlers use this to decide whether to use the static
	// BaseURL or to derive the domain from the request host.
	BaseURLOverride bool

	// StorePath is the file-system path for the JSON persistence file
	// (env: STORE_PATH, default: "urls.json"). Used only when MongoURI is empty.
	StorePath string

	// AdCountdown is the number of seconds the interstitial ad page counts
	// down before automatically forwarding the visitor (env: AD_COUNTDOWN,
	// default: 5).
	AdCountdown int

	// MongoURI is the MongoDB connection string (env: MONGODB_URI).
	// When set, MongoDB is used as the store instead of the JSON file.
	MongoURI string

	// MongoDB is the MongoDB database name (env: MONGODB_DB, default: "urlshortner").
	MongoDB string

	// MongoCollection is the MongoDB collection name (env: MONGODB_COLLECTION, default: "urls").
	MongoCollection string

	// Location is the time zone for timestamps in API responses and logs
	// (env: TIMEZONE, an IANA name, default: "Asia/Colombo"). Records are
	// always stored in UTC.
	Location *time.Location
}

// loadConfig reads configuration from environment variables and returns a
// populated Config. Unset variables fall back to sensible defaults:
//
//   - PORT                → "8182"
//   - BASE_URL            → derived per-request from the Host header (no static default)
//   - STORE_PATH          → "urls.json"
//   - AD_COUNTDOWN        → 5 seconds (also used, with a warning, for invalid values)
//   - TIMEZONE            → "Asia/Colombo"
//   - MONGODB_URI         → unset (use the JSON file store)
//   - MONGODB_DB          → "urlshortner"
//   - MONGODB_COLLECTION  → "urls"
//
// An invalid BASE_URL or TIMEZONE is an error, since either would produce
// wrong links or timestamps on every request.
func loadConfig() (*Config, error) {
	cfg := &Config{
		Port:            envOr("PORT", "8182"),
		StorePath:       envOr("STORE_PATH", "urls.json"),
		AdCountdown:     5,
		MongoURI:        os.Getenv("MONGODB_URI"),
		MongoDB:         envOr("MONGODB_DB", "urlshortner"),
		MongoCollection: envOr("MONGODB_COLLECTION", "urls"),
	}

	if v := os.Getenv("AD_COUNTDOWN"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.AdCountdown = n
		} else {
			log.Printf("warning: AD_COUNTDOWN=%q is not a positive integer; using %d", v, cfg.AdCountdown)
		}
	}

	if v := os.Getenv("BASE_URL"); v != "" {
		u, err := url.Parse(v)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("BASE_URL must be an absolute http(s) URL such as https://sho.rt, got %q", v)
		}
		cfg.BaseURL = strings.TrimRight(v, "/")
		cfg.BaseURLOverride = true
	}

	loc, err := time.LoadLocation(envOr("TIMEZONE", "Asia/Colombo"))
	if err != nil {
		return nil, fmt.Errorf("TIMEZONE: %w", err)
	}
	cfg.Location = loc

	return cfg, nil
}

// envOr returns the value of the environment variable key, or def when it is
// unset or empty.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// openStore returns the MongoDB store when MONGODB_URI is set and the JSON
// file store otherwise, along with a label describing it for the startup log.
func openStore(cfg *Config) (Store, string, error) {
	if cfg.MongoURI != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		s, err := NewMongoStore(ctx, cfg.MongoURI, cfg.MongoDB, cfg.MongoCollection)
		if err != nil {
			return nil, "", fmt.Errorf("connect to MongoDB: %w", err)
		}
		return s, "mongodb/" + cfg.MongoDB + "/" + cfg.MongoCollection, nil
	}
	s, err := NewJSONStore(cfg.StorePath)
	if err != nil {
		return nil, "", fmt.Errorf("open store: %w", err)
	}
	return s, cfg.StorePath, nil
}

// run loads configuration, opens the store, and serves HTTP until the server
// fails or the process receives SIGINT/SIGTERM. On shutdown it lets in-flight
// requests finish and flushes/closes the store.
func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	// Log timestamps use the configured zone too. Safe: no other goroutines yet.
	time.Local = cfg.Location

	store, storeLabel, err := openStore(cfg)
	if err != nil {
		return err
	}

	handlers, err := NewHandlers(cfg, store)
	if err != nil {
		store.Close(context.Background())
		return fmt.Errorf("load templates: %w", err)
	}

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           handlers.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	baseDisplay := cfg.BaseURL
	if !cfg.BaseURLOverride {
		baseDisplay = "(derived from request host)"
	}
	log.Printf("URL Shortener running on :%s (base: %s)", cfg.Port, baseDisplay)
	log.Printf("Ad countdown: %d seconds | Store: %s | Time zone: %s", cfg.AdCountdown, storeLabel, cfg.Location)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	select {
	case err = <-errCh:
		err = fmt.Errorf("server error: %w", err)
	case <-ctx.Done():
		stop() // restore default handling so a second Ctrl+C exits immediately
		log.Print("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if serr := srv.Shutdown(shutdownCtx); serr != nil && !errors.Is(serr, http.ErrServerClosed) {
		err = errors.Join(err, fmt.Errorf("shutdown: %w", serr))
	}
	if cerr := store.Close(shutdownCtx); cerr != nil {
		err = errors.Join(err, fmt.Errorf("close store: %w", cerr))
	}
	return err
}

// main runs the application and exits with a non-zero status on failure.
func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
