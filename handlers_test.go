package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testConfig(t *testing.T) *Config {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Colombo")
	if err != nil {
		t.Fatal(err)
	}
	return &Config{AdCountdown: 5, Location: loc}
}

func newTestApp(t *testing.T, cfg *Config) (http.Handler, *JSONStore) {
	t.Helper()
	store, _ := newTestJSONStore(t)
	h, err := NewHandlers(cfg, store)
	if err != nil {
		t.Fatalf("NewHandlers: %v", err)
	}
	return h.Routes(), store
}

func do(t *testing.T, app http.Handler, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, req)
	return rec
}

func decodeJSON[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return v
}

func TestShortenRedirectAndInfo(t *testing.T) {
	app, _ := newTestApp(t, testConfig(t))

	rec := do(t, app, "POST", "/shorten", `{"url":"https://go.dev/doc"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("shorten: status %d, body %s", rec.Code, rec.Body)
	}
	created := decodeJSON[shortenResponse](t, rec)
	if !isValidCode(created.ShortCode) {
		t.Fatalf("invalid short code %q", created.ShortCode)
	}
	if want := "http://example.com/" + created.ShortCode; created.ShortURL != want {
		t.Errorf("short_url = %q, want %q", created.ShortURL, want)
	}

	rec = do(t, app, "GET", "/"+created.ShortCode, "")
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/ad/"+created.ShortCode {
		t.Fatalf("redirect: status %d, Location %q", rec.Code, rec.Header().Get("Location"))
	}

	rec = do(t, app, "GET", "/ad/"+created.ShortCode, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "https://go.dev/doc") {
		t.Fatalf("ad page: status %d, body lacks destination", rec.Code)
	}

	rec = do(t, app, "GET", "/info/"+created.ShortCode, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("info: status %d", rec.Code)
	}
	info := decodeJSON[infoResponse](t, rec)
	if info.LongURL != "https://go.dev/doc" || info.ShortURL != created.ShortURL || info.Clicks != 1 {
		t.Errorf("info = %+v", info)
	}
	ts, err := time.Parse(time.RFC3339, info.CreatedAt)
	if err != nil {
		t.Fatalf("created_at %q is not RFC 3339: %v", info.CreatedAt, err)
	}
	if !strings.HasSuffix(info.CreatedAt, "+05:30") {
		t.Errorf("created_at %q is not in Asia/Colombo time", info.CreatedAt)
	}
	if time.Since(ts) > time.Minute || time.Since(ts) < -time.Minute {
		t.Errorf("created_at %v is not around now", ts)
	}
}

func TestShortenValidation(t *testing.T) {
	app, _ := newTestApp(t, testConfig(t))

	tests := []struct {
		name, body string
		status     int
	}{
		{"empty body", ``, http.StatusBadRequest},
		{"invalid JSON", `{"url":`, http.StatusBadRequest},
		{"missing url", `{}`, http.StatusBadRequest},
		{"blank url", `{"url":"   "}`, http.StatusBadRequest},
		{"no scheme", `{"url":"example.com"}`, http.StatusBadRequest},
		{"ftp scheme", `{"url":"ftp://example.com/file"}`, http.StatusBadRequest},
		{"javascript", `{"url":"javascript:alert(1)"}`, http.StatusBadRequest},
		{"scheme only", `{"url":"http://"}`, http.StatusBadRequest},
		{"port only", `{"url":"http://:80/x"}`, http.StatusBadRequest},
		{"bad host", `{"url":"http://exa mple.com"}`, http.StatusBadRequest},
		{"credentials", `{"url":"https://bank.example@evil.example/"}`, http.StatusBadRequest},
		{"self link", `{"url":"http://example.com/abcdef"}`, http.StatusBadRequest},
		{"too long", `{"url":"https://example.org/` + strings.Repeat("a", maxURLLength) + `"}`, http.StatusBadRequest},
		{"body too large", `{"url":"https://example.org/","pad":"` + strings.Repeat("a", maxBodyBytes) + `"}`, http.StatusRequestEntityTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := do(t, app, "POST", "/shorten", tt.body)
			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tt.status, rec.Body)
			}
			if got := decodeJSON[map[string]string](t, rec)["error"]; got == "" {
				t.Error("missing error message")
			}
		})
	}
}

func TestShortenNormalizesURL(t *testing.T) {
	app, _ := newTestApp(t, testConfig(t))
	rec := do(t, app, "POST", "/shorten", `{"url":"  HTTPS://Example.org/a b?q=1#frag  "}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body)
	}
	code := decodeJSON[shortenResponse](t, rec).ShortCode
	info := decodeJSON[infoResponse](t, do(t, app, "GET", "/info/"+code, ""))
	if want := "https://Example.org/a%20b?q=1#frag"; info.LongURL != want {
		t.Errorf("long_url = %q, want %q", info.LongURL, want)
	}
}

func TestBaseURL(t *testing.T) {
	cfg := testConfig(t)
	tests := []struct {
		name  string
		proto string
		tls   bool
		want  string
	}{
		{"plain", "", false, "http://short.test"},
		{"tls", "", true, "https://short.test"},
		{"forwarded https", "https", false, "https://short.test"},
		{"forwarded list", "HTTPS, http", false, "https://short.test"},
		{"forwarded http over tls", "http", true, "http://short.test"},
		{"forwarded garbage ignored", "javascript", false, "http://short.test"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "http://short.test/", nil)
			if tt.proto != "" {
				r.Header.Set("X-Forwarded-Proto", tt.proto)
			}
			if tt.tls {
				r.TLS = &tls.ConnectionState{}
			}
			if got := getBaseURL(cfg, r); got != tt.want {
				t.Errorf("getBaseURL = %q, want %q", got, tt.want)
			}
		})
	}

	cfg.BaseURL, cfg.BaseURLOverride = "https://sho.rt", true
	app, _ := newTestApp(t, cfg)
	created := decodeJSON[shortenResponse](t, do(t, app, "POST", "/shorten", `{"url":"https://example.org"}`))
	if want := "https://sho.rt/" + created.ShortCode; created.ShortURL != want {
		t.Errorf("short_url with BASE_URL = %q, want %q", created.ShortURL, want)
	}
	rec := do(t, app, "POST", "/shorten", `{"url":"https://sho.rt/abcdef"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("link to BASE_URL host: status %d, want 400", rec.Code)
	}
}

func TestHeadDoesNotCountClick(t *testing.T) {
	app, store := newTestApp(t, testConfig(t))
	if err := store.Save(context.Background(), "head01", "https://example.org"); err != nil {
		t.Fatal(err)
	}
	if rec := do(t, app, "HEAD", "/head01", ""); rec.Code != http.StatusFound {
		t.Fatalf("HEAD: status %d, want 302", rec.Code)
	}
	if rec := do(t, app, "HEAD", "/zzzzzz", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("HEAD missing: status %d, want 404", rec.Code)
	}
	if rec, _ := store.GetByCode(context.Background(), "head01"); rec.Clicks != 0 {
		t.Errorf("clicks after HEAD = %d, want 0", rec.Clicks)
	}
}

func TestNotFound(t *testing.T) {
	app, _ := newTestApp(t, testConfig(t))
	for _, path := range []string{"/zzzzzz", "/favicon.ico", "/a/b", "/ad/zzzzzz", "/ad/bad", "/shorten"} {
		rec := do(t, app, "GET", path, "")
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: status %d, want 404", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "Short URL not found") {
			t.Errorf("GET %s: body lacks not-found message", path)
		}
	}
	for _, path := range []string{"/info/zzzzzz", "/info/x"} {
		rec := do(t, app, "GET", path, "")
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: status %d, want 404", path, rec.Code)
		}
		decodeJSON[map[string]string](t, rec)
	}
}

func TestIndexPage(t *testing.T) {
	app, _ := newTestApp(t, testConfig(t))
	rec := do(t, app, "GET", "/", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "http://example.com/aB3xYz") {
		t.Error("index page does not show the short link preview")
	}
	if strings.Contains(body, "Short URL not found") {
		t.Error("index page shows the not-found message")
	}
}

func TestAdPageEscapesDestination(t *testing.T) {
	cfg := testConfig(t)
	cfg.AdCountdown = 7
	app, store := newTestApp(t, cfg)
	evil := `https://example.org/?q="</script><script>alert(1)</script>`
	if err := store.Save(context.Background(), "xss001", evil); err != nil {
		t.Fatal(err)
	}
	rec := do(t, app, "GET", "/ad/xss001", "")
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if strings.Contains(body, "<script>alert(1)") {
		t.Fatal("destination URL is not escaped")
	}
	if !strings.Contains(body, "const total =  7 ;") {
		t.Error("countdown not rendered into the script")
	}
	if !strings.Contains(body, `http-equiv="refresh" content="7;url=`) {
		t.Error("no-JS refresh fallback missing")
	}
}

func TestSecurityHeaders(t *testing.T) {
	app, _ := newTestApp(t, testConfig(t))
	rec := do(t, app, "GET", "/", "")
	for k, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "strict-origin-when-cross-origin",
	} {
		if got := rec.Header().Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

func TestHealth(t *testing.T) {
	app, _ := newTestApp(t, testConfig(t))
	rec := do(t, app, "GET", "/healthz", "")
	if rec.Code != http.StatusOK || decodeJSON[map[string]string](t, rec)["status"] != "ok" {
		t.Fatalf("healthz: status %d, body %s", rec.Code, rec.Body)
	}

	h, _ := NewHandlers(testConfig(t), &stubStore{err: errors.New("down")})
	rec = do(t, h.Routes(), "GET", "/healthz", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("healthz with failing store: status %d, want 503", rec.Code)
	}
}

// stubStore is a Store whose behaviour is scripted by its fields.
type stubStore struct {
	err        error // returned by every method when set
	duplicates int   // number of Save calls that report errDuplicate
	saves      int
}

func (s *stubStore) Save(context.Context, string, string) error {
	s.saves++
	if s.err != nil {
		return s.err
	}
	if s.saves <= s.duplicates {
		return errDuplicate
	}
	return nil
}
func (s *stubStore) GetByCode(context.Context, string) (*URLRecord, error) { return nil, s.err }
func (s *stubStore) IncrementClicks(context.Context, string) (bool, error) { return false, s.err }
func (s *stubStore) Ping(context.Context) error                            { return s.err }
func (s *stubStore) Close(context.Context) error                           { return nil }

func TestStoreErrors(t *testing.T) {
	h, _ := NewHandlers(testConfig(t), &stubStore{err: errors.New("boom")})
	app := h.Routes()
	for _, tc := range []struct{ method, path, body string }{
		{"POST", "/shorten", `{"url":"https://example.org"}`},
		{"GET", "/abcdef", ""},
		{"GET", "/ad/abcdef", ""},
		{"GET", "/info/abcdef", ""},
	} {
		rec := do(t, app, tc.method, tc.path, tc.body)
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("%s %s: status %d, want 500", tc.method, tc.path, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "boom") {
			t.Errorf("%s %s: internal error leaked to client", tc.method, tc.path)
		}
	}
}

func TestShortenRetriesOnCollision(t *testing.T) {
	store := &stubStore{duplicates: saveAttempts - 1}
	h, _ := NewHandlers(testConfig(t), store)
	if rec := do(t, h.Routes(), "POST", "/shorten", `{"url":"https://example.org"}`); rec.Code != http.StatusOK {
		t.Fatalf("status %d after %d collisions, want 200", rec.Code, saveAttempts-1)
	}

	store = &stubStore{duplicates: saveAttempts}
	h, _ = NewHandlers(testConfig(t), store)
	if rec := do(t, h.Routes(), "POST", "/shorten", `{"url":"https://example.org"}`); rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d after %d collisions, want 500", rec.Code, saveAttempts)
	}
	if store.saves != saveAttempts {
		t.Errorf("Save called %d times, want %d", store.saves, saveAttempts)
	}
}

func TestGenerateShortCode(t *testing.T) {
	seen := make(map[string]bool)
	counts := make(map[byte]int)
	for i := 0; i < 2000; i++ {
		code := generateShortCode(codeLength)
		if !isValidCode(code) {
			t.Fatalf("invalid code %q", code)
		}
		if seen[code] {
			t.Fatalf("duplicate code %q", code)
		}
		seen[code] = true
		for j := 0; j < len(code); j++ {
			counts[code[j]]++
		}
	}
	if len(counts) != len(charset) {
		t.Errorf("only %d of %d characters used", len(counts), len(charset))
	}
}

func TestIsValidCode(t *testing.T) {
	for code, want := range map[string]bool{
		"abc123": true, "ABCxyz": true, "000000": true,
		"": false, "abc12": false, "abc1234": false, "abc-12": false, "abc.12": false, "ábc12": false,
	} {
		if got := isValidCode(code); got != want {
			t.Errorf("isValidCode(%q) = %v, want %v", code, got, want)
		}
	}
}
