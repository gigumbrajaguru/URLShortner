# URLShortner

A lightweight URL shortener with interstitial ad support, written in Go. No external dependencies — pure standard library.

## Features

- Shorten any `http://` or `https://` URL to a 6-character alphanumeric code
- Interstitial ad page with configurable countdown timer before redirecting visitors
- Click tracking per short URL
- JSON metadata endpoint to inspect any short link
- In-memory store with JSON file persistence — no external database required
- Domain-aware: short URLs automatically reflect the site's own domain, or can be overridden via `BASE_URL`
- Fully configurable via environment variables

## Quick Start

```bash
git clone https://github.com/gigumbrajaguru/URLShortner.git
cd URLShortner
go build -o urlshortner .
./urlshortner
```

Open `http://localhost:8080` in your browser, paste a long URL, and click **Shorten URL**.

## Configuration

All configuration is done through environment variables. Every variable has a sensible default so the app works out of the box.

| Variable | Default | Description |
|---|---|---|
| `PORT` | `8080` | Port the HTTP server listens on |
| `BASE_URL` | *(derived from request host)* | Override the domain used in generated short links (e.g. `https://sho.rt`). When not set, the short URL automatically uses the same host the request came in on. |
| `STORE_PATH` | `urls.json` | Path to the JSON file used for persistent storage |
| `AD_COUNTDOWN` | `5` | Number of seconds to display the interstitial ad before redirecting the visitor |

### Example: custom domain + HTTPS

```bash
BASE_URL=https://sho.rt PORT=443 ./urlshortner
```

### Example: Docker

```bash
docker build -t urlshortner .
docker run -p 8080:8080 \
  -e BASE_URL=https://sho.rt \
  -e AD_COUNTDOWN=8 \
  -e STORE_PATH=/data/urls.json \
  -v $(pwd)/data:/data \
  urlshortner
```

## API Reference

### `POST /shorten`

Create a new short URL.

**Request body (JSON):**
```json
{ "url": "https://example.com/some/very/long/path" }
```

**Response (JSON):**
```json
{
  "short_url": "https://sho.rt/aB3xYz",
  "short_code": "aB3xYz"
}
```

**Errors:**
- `400` — missing or malformed URL, or URL does not start with `http://` / `https://`
- `500` — failed to persist the record

---

### `GET /{code}`

Redirects the visitor through the interstitial ad page to the destination URL. Increments the click counter.

---

### `GET /ad/{code}`

Renders the interstitial ad page. After the countdown expires the visitor is automatically forwarded to the destination. A **Continue to Site** button appears once the countdown finishes.

---

### `GET /info/{code}`

Returns JSON metadata about a short link.

**Response (JSON):**
```json
{
  "short_code": "aB3xYz",
  "long_url": "https://example.com/some/very/long/path",
  "short_url": "https://sho.rt/aB3xYz",
  "created_at": "2026-03-30T12:00:00Z",
  "clicks": 42
}
```

**Errors:**
- `404` — short code not found

---

## Project Structure

```
URLShortner/
├── main.go          # Entry point: config loading and server startup
├── handlers.go      # HTTP handlers, route registration, and URL construction
├── storage.go       # Store interface and thread-safe JSON file implementation
├── templates/
│   ├── index.html   # Home page with URL submission form
│   └── ad.html      # Interstitial ad page with countdown timer
└── go.mod           # Go module definition (no external dependencies)
```

## Development

**Prerequisites:** Go 1.24 or later.

```bash
# Build
go build -o urlshortner .

# Run with live reload (requires Air: https://github.com/air-verse/air)
air

# Vet
go vet ./...

# Run tests (if present)
go test ./...
```

The store file (`urls.json` by default) is created automatically on first run. It is excluded from version control via `.gitignore`.

## Deployment Notes

- Place the application behind a reverse proxy (nginx, Caddy, Traefik) that terminates TLS.
- Set the `X-Forwarded-Proto` header on the proxy so that the app generates `https://` short URLs correctly when `BASE_URL` is not set.
- The JSON store is suitable for low-to-moderate traffic. For high-volume deployments, replace the `Store` interface implementation with a database-backed one (Postgres, Redis, etc.) without changing any handler code.

## Contributing

Pull requests are welcome. Please open an issue first for significant changes.
