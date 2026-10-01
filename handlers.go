package main

import (
	"bytes"
	"crypto/rand"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

//go:embed templates/*.html
var templateFS embed.FS

const (
	// codeLength is the number of characters in a generated short code.
	codeLength = 6

	// maxURLLength is the longest destination URL accepted by /shorten.
	maxURLLength = 2048

	// maxBodyBytes caps the POST /shorten request body. It leaves room for a
	// maxURLLength URL even when every character is JSON-escaped.
	maxBodyBytes = 16 << 10

	// saveAttempts is how many random codes ShortenURL tries before giving up.
	saveAttempts = 5
)

const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// getBaseURL returns the scheme+host prefix used when building short URLs.
//
// Priority:
//  1. If BASE_URL was explicitly set via environment variable, that value is
//     returned unchanged — useful for custom domains or CDN fronting.
//  2. Otherwise the base URL is derived from the current request:
//     a. Scheme comes from the X-Forwarded-Proto header (set by reverse proxies
//     such as nginx/Caddy/Traefik when terminating TLS upstream). Only "http"
//     and "https" are honoured; for a comma-separated list the first entry
//     (the client-facing hop) is used.
//     b. If the header is absent or invalid, HTTPS is used when the connection
//     has TLS.
//     c. Otherwise HTTP is assumed.
//     d. The host is taken from r.Host (includes port when non-standard).
func getBaseURL(cfg *Config, r *http.Request) string {
	if cfg.BaseURLOverride {
		return cfg.BaseURL
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		proto, _, _ = strings.Cut(proto, ",")
		switch p := strings.ToLower(strings.TrimSpace(proto)); p {
		case "http", "https":
			scheme = p
		}
	}
	return scheme + "://" + r.Host
}

// generateShortCode creates a cryptographically random alphanumeric string of
// the given length drawn from [a-zA-Z0-9]. Random bytes at or above the
// largest multiple of len(charset) are discarded, so every character is
// equally likely (a plain modulo would favour the first 8 characters).
func generateShortCode(length int) string {
	const limit = 256 - 256%len(charset)
	code := make([]byte, 0, length)
	buf := make([]byte, length)
	for len(code) < length {
		rand.Read(buf) // never returns an error since Go 1.24
		for _, b := range buf {
			if int(b) < limit && len(code) < length {
				code = append(code, charset[int(b)%len(charset)])
			}
		}
	}
	return string(code)
}

// isValidCode reports whether s has the shape of a generated short code, so
// paths such as /favicon.ico or /robots.txt are rejected without a store
// lookup.
func isValidCode(s string) bool {
	if len(s) != codeLength {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9') {
			return false
		}
	}
	return true
}

// normalizeURL validates a destination URL and returns its canonical form.
// It accepts only absolute http(s) URLs with a host, rejects embedded
// credentials (a phishing trick: https://bank.example@evil.example), and
// refuses links to this service itself so short links cannot be chained to
// hide their real destination.
func normalizeURL(raw, baseURL string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("url field is required")
	}
	if len(raw) > maxURLLength {
		return "", fmt.Errorf("url must be at most %d characters", maxURLLength)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("url is not valid")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("url must start with http:// or https://")
	}
	if u.Hostname() == "" {
		return "", errors.New("url must include a host name")
	}
	if u.User != nil {
		return "", errors.New("url must not contain a username or password")
	}
	if base, err := url.Parse(baseURL); err == nil && strings.EqualFold(u.Host, base.Host) {
		return "", errors.New("links to this service cannot be shortened")
	}
	return u.String(), nil
}

// Handlers groups the shared dependencies injected into every HTTP handler.
// Create one via NewHandlers and serve it with Routes.
type Handlers struct {
	cfg       *Config
	store     Store
	templates *template.Template
}

// NewHandlers creates a Handlers instance and parses the HTML templates
// embedded in the binary. An error is returned if a template contains a
// syntax error.
func NewHandlers(cfg *Config, store Store) (*Handlers, error) {
	tmpl, err := template.ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	return &Handlers{cfg: cfg, store: store, templates: tmpl}, nil
}

// Routes returns the application's HTTP handler: every route registered on a
// ServeMux, wrapped with the security-header middleware. ServeMux picks the
// most specific pattern, so /ad/{code} and /info/{code} win over the
// single-segment /{code}, "/{$}" matches only the home page, and "/" catches
// everything else as a 404.
func (h *Handlers) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", h.IndexPage)
	mux.HandleFunc("POST /shorten", h.ShortenURL)
	mux.HandleFunc("GET /healthz", h.Health)
	mux.HandleFunc("GET /ad/{code}", h.AdPage)
	mux.HandleFunc("GET /info/{code}", h.GetInfo)
	mux.HandleFunc("GET /{code}", h.RedirectToAd)
	mux.HandleFunc("/", h.NotFound)
	return securityHeaders(mux)
}

// securityHeaders sets response headers that harden every page: no MIME
// sniffing, no framing by other sites (clickjacking), and no full URLs leaked
// in the Referer header to other origins.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := w.Header()
		hdr.Set("X-Content-Type-Options", "nosniff")
		hdr.Set("X-Frame-Options", "DENY")
		hdr.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		next.ServeHTTP(w, r)
	})
}

type indexData struct {
	BaseURL string
	Error   string
}

type adData struct {
	DestinationURL string
	Countdown      int
}

// IndexPage renders the home page with the URL submission form.
// It passes the resolved base URL to the template so the UI can display
// a preview of what the generated short link will look like.
func (h *Handlers) IndexPage(w http.ResponseWriter, r *http.Request) {
	h.renderHTML(w, "index.html", http.StatusOK, indexData{BaseURL: getBaseURL(h.cfg, r)})
}

// NotFound renders the home page with a "not found" message and a 404 status.
func (h *Handlers) NotFound(w http.ResponseWriter, r *http.Request) {
	h.renderHTML(w, "index.html", http.StatusNotFound, indexData{
		BaseURL: getBaseURL(h.cfg, r),
		Error:   "Short URL not found",
	})
}

// Health handles GET /healthz for load balancers and orchestrators. It returns
// 200 {"status":"ok"} when the store is reachable and 503 otherwise.
func (h *Handlers) Health(w http.ResponseWriter, r *http.Request) {
	if err := h.store.Ping(r.Context()); err != nil {
		log.Printf("health check: %v", err)
		jsonError(w, "store unavailable", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type shortenRequest struct {
	URL string `json:"url"`
}

type shortenResponse struct {
	ShortURL  string `json:"short_url"`
	ShortCode string `json:"short_code"`
}

// ShortenURL handles POST /shorten. It expects a JSON body of the form
// {"url":"https://..."}, validates and normalises the URL, generates a unique
// 6-character short code, persists the mapping, and returns
// {"short_url":"...","short_code":"..."}.
//
// The handler retries code generation up to saveAttempts times to resolve the
// rare case where a randomly generated code collides with an existing one.
func (h *Handlers) ShortenURL(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var req shortenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			jsonError(w, "request body is too large", http.StatusRequestEntityTooLarge)
			return
		}
		jsonError(w, `request body must be JSON such as {"url":"https://example.com"}`, http.StatusBadRequest)
		return
	}

	baseURL := getBaseURL(h.cfg, r)
	longURL, err := normalizeURL(req.URL, baseURL)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	var code string
	for range saveAttempts {
		candidate := generateShortCode(codeLength)
		err := h.store.Save(r.Context(), candidate, longURL)
		if err == nil {
			code = candidate
			break
		}
		if !errors.Is(err, errDuplicate) {
			log.Printf("shorten: save %s: %v", candidate, err)
			jsonError(w, "failed to save URL", http.StatusInternalServerError)
			return
		}
	}
	if code == "" {
		log.Printf("shorten: %d short code collisions in a row", saveAttempts)
		jsonError(w, "failed to generate unique short code", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, shortenResponse{
		ShortURL:  baseURL + "/" + code,
		ShortCode: code,
	})
}

// RedirectToAd handles GET /{code}. It counts the click and issues a 302
// redirect to the interstitial ad page at /ad/{code}. Counting and the
// existence check are a single store operation.
//
// HEAD requests (link previews, uptime checkers) only check that the code
// exists, so they are side-effect free and do not inflate the click count.
func (h *Handlers) RedirectToAd(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	if !isValidCode(code) {
		h.NotFound(w, r)
		return
	}

	var found bool
	var err error
	if r.Method == http.MethodHead {
		var record *URLRecord
		record, err = h.store.GetByCode(r.Context(), code)
		found = record != nil
	} else {
		found, err = h.store.IncrementClicks(r.Context(), code)
	}
	if err != nil {
		log.Printf("redirect %s: %v", code, err)
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	if !found {
		h.NotFound(w, r)
		return
	}

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
	if !isValidCode(code) {
		h.NotFound(w, r)
		return
	}

	record, err := h.store.GetByCode(r.Context(), code)
	if err != nil {
		log.Printf("ad page %s: %v", code, err)
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	if record == nil {
		h.NotFound(w, r)
		return
	}

	h.renderHTML(w, "ad.html", http.StatusOK, adData{
		DestinationURL: record.LongURL,
		Countdown:      h.cfg.AdCountdown,
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
// the creation timestamp (RFC 3339 in the configured TIMEZONE), and the total
// click count.
func (h *Handlers) GetInfo(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	if !isValidCode(code) {
		jsonError(w, "short URL not found", http.StatusNotFound)
		return
	}

	record, err := h.store.GetByCode(r.Context(), code)
	if err != nil {
		log.Printf("info %s: %v", code, err)
		jsonError(w, "database error", http.StatusInternalServerError)
		return
	}
	if record == nil {
		jsonError(w, "short URL not found", http.StatusNotFound)
		return
	}

	writeJSON(w, http.StatusOK, infoResponse{
		ShortCode: record.ShortCode,
		LongURL:   record.LongURL,
		ShortURL:  getBaseURL(h.cfg, r) + "/" + record.ShortCode,
		CreatedAt: record.CreatedAt.In(h.cfg.Location).Format(time.RFC3339),
		Clicks:    record.Clicks,
	})
}

// bufPool recycles the buffers templates are rendered into.
var bufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

// renderHTML executes a named template and writes it with the given status.
// The page is rendered into a buffer first so a template error produces a
// clean 500 instead of a half-written page.
func (h *Handlers) renderHTML(w http.ResponseWriter, name string, status int, data any) {
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufPool.Put(buf)

	if err := h.templates.ExecuteTemplate(buf, name, data); err != nil {
		log.Printf("render %s: %v", name, err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	buf.WriteTo(w)
}

// writeJSON writes v as a JSON response with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// jsonError writes a JSON error response of the form {"error":"..."}.
func jsonError(w http.ResponseWriter, msg string, status int) {
	writeJSON(w, status, map[string]string{"error": msg})
}
