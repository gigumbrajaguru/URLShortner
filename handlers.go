package main

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"strings"
)

// errDuplicate is returned by Store.Save when the short code already exists.
var errDuplicate = errors.New("duplicate short code")

const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// getBaseURL returns the scheme+host prefix used when building short URLs.
//
// Priority:
//  1. If BASE_URL was explicitly set via environment variable, that value is
//     returned unchanged — useful for custom domains or CDN fronting.
//  2. Otherwise the base URL is derived from the current request:
//     a. Scheme comes from the X-Forwarded-Proto header (set by reverse proxies
//        such as nginx/Caddy/Traefik when terminating TLS upstream).
//     b. If the header is absent, HTTPS is used when the connection has TLS.
//     c. Otherwise HTTP is assumed.
//     d. The host is taken from r.Host (includes port when non-standard).
func getBaseURL(cfg *Config, r *http.Request) string {
	if cfg.BaseURLOverride {
		return cfg.BaseURL
	}
	scheme := "http"
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	} else if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// generateShortCode creates a cryptographically random alphanumeric string of
// the given length drawn from [a-zA-Z0-9]. It uses crypto/rand as the source
// of entropy so the codes are unpredictable even at high volume.
func generateShortCode(length int) string {
	b := make([]byte, length)
	rand.Read(b)
	for i := range b {
		b[i] = charset[int(b[i])%len(charset)]
	}
	return string(b)
}

// Handlers groups the shared dependencies injected into every HTTP handler.
// Create one via NewHandlers and pass it to RegisterRoutes.
type Handlers struct {
	cfg       *Config
	store     Store
	templates *template.Template
}

// NewHandlers creates a Handlers instance and eagerly parses all HTML
// templates from the templates/ directory. An error is returned if any
// template file is missing or contains a syntax error.
func NewHandlers(cfg *Config, store Store) (*Handlers, error) {
	tmpl, err := template.ParseGlob("templates/*.html")
	if err != nil {
		return nil, err
	}
	return &Handlers{cfg: cfg, store: store, templates: tmpl}, nil
}

// RegisterRoutes wires all application routes onto mux. More-specific paths
// (e.g. /ad/{code}, /info/{code}) are registered before the catch-all
// /{code} so that Go's ServeMux pattern matching resolves them correctly.
func RegisterRoutes(mux *http.ServeMux, h *Handlers) {
	mux.HandleFunc("GET /", h.IndexPage)
	mux.HandleFunc("POST /shorten", h.ShortenURL)
	mux.HandleFunc("GET /ad/{code}", h.AdPage)
	mux.HandleFunc("GET /info/{code}", h.GetInfo)
	mux.HandleFunc("GET /{code}", h.RedirectToAd)
}

// IndexPage renders the home page with the URL submission form.
// It passes the resolved base URL to the template so the UI can display
// a preview of what the generated short link will look like.
func (h *Handlers) IndexPage(w http.ResponseWriter, r *http.Request) {
	h.renderHTML(w, "index.html", http.StatusOK, map[string]any{
		"baseURL": getBaseURL(h.cfg, r),
	})
}

type shortenRequest struct {
	URL string `json:"url"`
}

type shortenResponse struct {
	ShortURL  string `json:"short_url"`
	ShortCode string `json:"short_code"`
}

// ShortenURL handles POST /shorten. It expects a JSON body of the form
// {"url":"https://..."}, generates a unique 6-character short code, persists
// the mapping, and returns {"short_url":"...","short_code":"..."}.
//
// The handler retries code generation up to 5 times to resolve the rare case
// where a randomly generated code collides with an existing one.
func (h *Handlers) ShortenURL(w http.ResponseWriter, r *http.Request) {
	var req shortenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.URL == "" {
		jsonError(w, "url field is required", http.StatusBadRequest)
		return
	}

	lower := strings.ToLower(req.URL)
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		jsonError(w, "url must start with http:// or https://", http.StatusBadRequest)
		return
	}

	var code string
	for i := 0; i < 5; i++ {
		candidate := generateShortCode(6)
		err := h.store.Save(candidate, req.URL)
		if err == nil {
			code = candidate
			break
		}
		if !errors.Is(err, errDuplicate) {
			jsonError(w, "failed to save URL", http.StatusInternalServerError)
			return
		}
	}
	if code == "" {
		jsonError(w, "failed to generate unique short code", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(shortenResponse{
		ShortURL:  getBaseURL(h.cfg, r) + "/" + code,
		ShortCode: code,
	})
}

// RedirectToAd handles GET /{code}. It looks up the short code, increments
// the click counter asynchronously (so the redirect is not delayed), and
// issues a 302 redirect to the interstitial ad page at /ad/{code}.
func (h *Handlers) RedirectToAd(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")

	record, err := h.store.GetByCode(code)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	if record == nil {
		h.renderHTML(w, "index.html", http.StatusNotFound, map[string]any{
			"baseURL": getBaseURL(h.cfg, r),
			"error":   "Short URL not found",
		})
		return
	}

	// Increment clicks asynchronously so the redirect isn't delayed.
	go h.store.IncrementClicks(code)

	http.Redirect(w, r, "/ad/"+code, http.StatusFound)
}

// AdPage handles GET /ad/{code}. It renders the interstitial ad page that
// counts down before forwarding the visitor to the destination URL.
//
// The destination URL is looked up server-side and injected directly into the
// template — it is never passed as a query parameter. This prevents open
// redirect abuse and ensures the URL is properly escaped by html/template.
func (h *Handlers) AdPage(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")

	record, err := h.store.GetByCode(code)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	if record == nil {
		h.renderHTML(w, "index.html", http.StatusNotFound, map[string]any{
			"baseURL": getBaseURL(h.cfg, r),
			"error":   "Short URL not found",
		})
		return
	}

	h.renderHTML(w, "ad.html", http.StatusOK, map[string]any{
		"destinationURL": record.LongURL,
		"countdown":      h.cfg.AdCountdown,
		"shortCode":      code,
	})
}

type infoResponse struct {
	ShortCode string `json:"short_code"`
	LongURL   string `json:"long_url"`
	ShortURL  string `json:"short_url"`
	CreatedAt string `json:"created_at"`
	Clicks    int64  `json:"clicks"`
}

// GetInfo handles GET /info/{code}. It returns a JSON object with metadata
// about the short link: the original long URL, the constructed short URL,
// the creation timestamp (ISO 8601 / UTC), and the total click count.
func (h *Handlers) GetInfo(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")

	record, err := h.store.GetByCode(code)
	if err != nil {
		jsonError(w, "database error", http.StatusInternalServerError)
		return
	}
	if record == nil {
		jsonError(w, "short URL not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(infoResponse{
		ShortCode: record.ShortCode,
		LongURL:   record.LongURL,
		ShortURL:  getBaseURL(h.cfg, r) + "/" + record.ShortCode,
		CreatedAt: record.CreatedAt.Format("2006-01-02T15:04:05Z"),
		Clicks:    record.Clicks,
	})
}

// renderHTML executes a named template, writing status and HTML to w.
func (h *Handlers) renderHTML(w http.ResponseWriter, name string, status int, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := h.templates.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

// jsonError writes a JSON error response.
func jsonError(w http.ResponseWriter, msg string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
