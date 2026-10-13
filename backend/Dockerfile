# Multi-stage build: compile the static binary, then ship a minimal runtime image.
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/server ./cmd/server && mkdir -p /out/data/media

# Runs as the image's non-root user (uid 65532). The media directory is
# created owned by that user so a fresh named volume inherits it. An existing
# volume created by an older root image needs a one-time
# `chown -R 65532:65532` of its contents.
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/server ./server
COPY --from=build /src/migrations ./migrations
COPY --from=build --chown=65532:65532 /out/data ./data
USER nonroot:nonroot
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --retries=3 CMD ["/app/server", "healthcheck"]
ENTRYPOINT ["/app/server"]
