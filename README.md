# URLShortner

A lightweight URL shortener with interstitial ad support, written in Go. Runs as a single self-contained binary (templates and time zone data are embedded) and stores links in a JSON file or MongoDB.

## Features

- Shorten any `http://` or `https://` URL to a 6-character alphanumeric code
- Interstitial ad page with configurable countdown timer before redirecting visitors (works without JavaScript too)
- Click tracking per short URL, plus a JSON metadata endpoint to inspect any short link
- Two storage backends:
  - **JSON file** (default) — no database required; atomic, crash-safe writes
  - **MongoDB** — for higher traffic or multiple instances
- Domain-aware: short URLs automatically reflect the site's own domain, or can be overridden via `BASE_URL`
- Timestamps in Sri Lanka time (`Asia/Colombo`) by default, configurable via `TIMEZONE`
- Production basics: request timeouts, body-size limits, security headers, `/healthz`, graceful shutdown
- Fully configurable via environment variables

## Quick Start

```bash
git clone https://github.com/gigumbrajaguru/URLShortner.git
cd URLShortner
go build -o urlshortner .
./urlshortner
```

Open `http://localhost:8182` in your browser, paste a long URL, and click **Shorten URL**.

## Configuration

All configuration is done through environment variables. Every variable has a sensible default so the app works out of the box.

| Variable | Default | Description |
|---|---|---|
| `PORT` | `8182` | Port the HTTP server listens on |
| `BASE_URL` | *(derived from request host)* | Override the domain used in generated short links (e.g. `https://sho.rt`). Must be an absolute `http(s)` URL; a trailing slash is ignored. When not set, the short URL uses the host the request came in on. |
| `STORE_PATH` | `urls.json` | Path to the JSON storage file (JSON backend only). Missing directories are created at startup. |
| `AD_COUNTDOWN` | `5` | Seconds to display the interstitial ad before redirecting. Invalid values fall back to `5` with a warning. |
| `TIMEZONE` | `Asia/Colombo` | IANA time zone for `created_at` in API responses and for log timestamps. Records are always stored in UTC. |
| `MONGODB_URI` | *(unset)* | MongoDB connection string. When set, MongoDB is used instead of the JSON file. |
| `MONGODB_DB` | `urlshortner` | MongoDB database name |
| `MONGODB_COLLECTION` | `urls` | MongoDB collection name (a unique index on `short_code` is created automatically) |

The app refuses to start on an invalid `BASE_URL` or `TIMEZONE`, on an unreadable or unwritable JSON store, or when MongoDB is unreachable within 10 seconds.

### Example: custom domain + MongoDB

```bash
BASE_URL=https://sho.rt \
MONGODB_URI="mongodb+srv://user:pass@cluster.example.mongodb.net" \
./urlshortner
```

### Example: Docker

```bash
docker build -t urlshortner .
docker run -p 8182:8182 \
  -e BASE_URL=https://sho.rt \
  -e AD_COUNTDOWN=8 \
  -v urlshortner-data:/data \
  urlshortner
```

The image runs as an unprivileged user and stores links in `/data/urls.json`. Use a named volume as above; if you bind-mount a host directory instead, make it writable by UID `65532`.

## API Reference

### `POST /shorten`

Create a new short URL.

**Request body (JSON):**
```json
{ "url": "https://example.com/some/very/long/path" }
```

**Response `200` (JSON):**
```json
{
  "short_url": "https://sho.rt/aB3xYz",
  "short_code": "aB3xYz"
}
```

The URL must be an absolute `http://` or `https://` URL with a host name, at most 2048 characters, without embedded credentials (`user:pass@`), and must not point back to this service. Surrounding whitespace is trimmed and the URL is stored in normalised form.

**Errors** (`{"error": "..."}`):
- `400` — body is not valid JSON, or the URL is missing or fails the rules above
- `413` — request body larger than 16 KB
- `500` — the record could not be saved

---

### `GET /{code}`

Counts a click and redirects (`302`) to the interstitial ad page `/ad/{code}`. `HEAD` requests check the link without counting a click. Unknown codes get a `404` page.

---

### `GET /ad/{code}`

Renders the interstitial ad page. After the countdown expires the visitor is automatically forwarded to the destination, and a **Continue to Site** button is enabled.

---

### `GET /info/{code}`

Returns JSON metadata about a short link.

**Response `200` (JSON):**
```json
{
  "short_code": "aB3xYz",
  "long_url": "https://example.com/some/very/long/path",
  "short_url": "https://sho.rt/aB3xYz",
  "created_at": "2026-03-30T17:30:00+05:30",
  "clicks": 42
}
```

`created_at` is RFC 3339 in the configured `TIMEZONE`.

**Errors:**
- `404` — short code not found

---

### `GET /healthz`

Health check for load balancers and orchestrators. Returns `200 {"status":"ok"}`, or `503` when MongoDB is unreachable.

## Project Structure

```
URLShortner/
├── main.go            # Entry point: config loading, store selection, server with graceful shutdown
├── handlers.go        # HTTP handlers, routes, URL validation, and short code generation
├── storage.go         # Store interface and the crash-safe JSON file implementation
├── mongo_store.go     # MongoDB implementation of Store
├── templates/
│   ├── index.html     # Home page with URL submission form
│   └── ad.html        # Interstitial ad page with countdown timer
├── *_test.go          # Unit and HTTP tests
├── Dockerfile         # Multi-stage build into a distroless image
└── .github/workflows/ci.yml  # gofmt, vet, race tests (incl. MongoDB), Docker build
```

## Development

**Prerequisites:** Go 1.25 or later.

```bash
# Build
go build -o urlshortner .

# Vet
go vet ./...

# Run tests (race detector)
go test -race ./...

# Also run the MongoDB store tests against a local server
MONGODB_TEST_URI=mongodb://localhost:27017 go test -race ./...
```

The store file (`urls.json` by default) is created automatically on first run. It is excluded from version control via `.gitignore`.

## Deployment Notes

- Place the application behind a reverse proxy (nginx, Caddy, Traefik) that terminates TLS.
- Have the proxy set the `X-Forwarded-Proto` header so the app generates `https://` short URLs when `BASE_URL` is not set (only `http`/`https` values are accepted), or simply set `BASE_URL`.
- On `SIGINT`/`SIGTERM` the server stops accepting connections, lets in-flight requests finish (up to 10 seconds), and flushes pending data before exiting.
- The JSON store keeps everything in memory, writes new links to disk immediately, and batches click-count updates once per second. It suits a single instance with low-to-moderate traffic. For high volume or multiple instances, set `MONGODB_URI`.
- To place real ads, replace the marked banner block in `templates/ad.html` with your ad network's code and rebuild.

## Contributing

Pull requests are welcome. Please open an issue first for significant changes.
