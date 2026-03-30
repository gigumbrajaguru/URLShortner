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

// generateShortCode creates a random alphanumeric string of the given length.
func generateShortCode(length int) string {
	b := make([]byte, length)
	rand.Read(b)
	for i := range b {
		b[i] = charset[int(b[i])%len(charset)]
	}
	return string(b)
}

// Handlers holds dependencies for all HTTP handlers.
type Handlers struct {
	cfg       *Config
	store     Store
	templates *template.Template
}

// NewHandlers creates a Handlers instance and parses HTML templates.
func NewHandlers(cfg *Config, store Store) (*Handlers, error) {
	tmpl, err := template.ParseGlob("templates/*.html")
	if err != nil {
		return nil, err
	}
	return &Handlers{cfg: cfg, store: store, templates: tmpl}, nil
}

// RegisterRoutes wires all routes onto mux.
// Specific routes must be registered before the catch-all /{code}.
func RegisterRoutes(mux *http.ServeMux, h *Handlers) {
	mux.HandleFunc("GET /", h.IndexPage)
	mux.HandleFunc("POST /shorten", h.ShortenURL)
	mux.HandleFunc("GET /ad/{code}", h.AdPage)
	mux.HandleFunc("GET /info/{code}", h.GetInfo)
	mux.HandleFunc("GET /{code}", h.RedirectToAd)
}

// IndexPage renders the URL submission UI.
func (h *Handlers) IndexPage(w http.ResponseWriter, r *http.Request) {
	h.renderHTML(w, "index.html", http.StatusOK, map[string]any{
		"baseURL": h.cfg.BaseURL,
	})
}

type shortenRequest struct {
	URL string `json:"url"`
}

type shortenResponse struct {
	ShortURL  string `json:"short_url"`
	ShortCode string `json:"short_code"`
}

// ShortenURL accepts {"url":"..."} and returns a short URL.
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
		ShortURL:  h.cfg.BaseURL + "/" + code,
		ShortCode: code,
	})
}

// RedirectToAd looks up the short code and redirects to the interstitial ad page.
func (h *Handlers) RedirectToAd(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")

	record, err := h.store.GetByCode(code)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	if record == nil {
		h.renderHTML(w, "index.html", http.StatusNotFound, map[string]any{
			"baseURL": h.cfg.BaseURL,
			"error":   "Short URL not found",
		})
		return
	}

	// Increment clicks asynchronously so the redirect isn't delayed.
	go h.store.IncrementClicks(code)

	http.Redirect(w, r, "/ad/"+code, http.StatusFound)
}

// AdPage renders the interstitial ad page with countdown timer.
// The destination URL is injected server-side — never in a query parameter.
func (h *Handlers) AdPage(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")

	record, err := h.store.GetByCode(code)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	if record == nil {
		h.renderHTML(w, "index.html", http.StatusNotFound, map[string]any{
			"baseURL": h.cfg.BaseURL,
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

// GetInfo returns JSON metadata about a short URL.
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
		ShortURL:  h.cfg.BaseURL + "/" + record.ShortCode,
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
