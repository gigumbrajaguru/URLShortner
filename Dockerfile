# syntax=docker/dockerfile:1

# ---- build ----
FROM golang:1.25-alpine AS build
WORKDIR /src

# Official Go images set GOTOOLCHAIN=local, which fails the build whenever
# go.mod (e.g. after a Dependabot bump) asks for a newer Go than the image
# ships. "auto" downloads the toolchain go.mod requires instead.
ENV GOTOOLCHAIN=auto

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/urlshortner . \
 && mkdir -p /out/data

# ---- runtime ----
# distroless/static ships CA certificates (for MongoDB Atlas TLS) and runs as
# an unprivileged user. Templates and time zone data are embedded in the binary.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/urlshortner /urlshortner
COPY --from=build --chown=65532:65532 /out/data /data

ENV PORT=8182 \
    STORE_PATH=/data/urls.json

EXPOSE 8182
VOLUME ["/data"]
ENTRYPOINT ["/urlshortner"]
