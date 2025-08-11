# syntax=docker/dockerfile:1.7
############################
# Builder
############################
FROM golang:1.24-bookworm AS builder

WORKDIR /src

# Enable build cache for modules and compiled objects
ENV CGO_ENABLED=0

# Pre-cache deps
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Copy the rest and build
COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    go build -trimpath -ldflags="-s -w" -o /out/app .

############################
# Runner
############################
# Distroless base (has libc, NSS, and CA certs). Non-root by default.
FROM gcr.io/distroless/base-debian12:nonroot

WORKDIR /app
COPY --from=builder /out/app /app/app

# Environment (override at runtime)
# * TELEGRAM_BOT_TOKEN and GEMINI_API_KEY must be provided
ENV DEFAULT_CURRENCY=THB \
    DATABASE_URL="file:spends.db?_fk=1"

# No ports exposed (the bot uses long polling)
USER nonroot:nonroot
ENTRYPOINT ["/app/app"]