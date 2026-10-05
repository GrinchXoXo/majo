# Multi-stage build: compile the static binary, then ship a minimal runtime image.
FROM golang:1.22-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/server ./cmd/server

FROM gcr.io/distroless/static-debian12
WORKDIR /app
COPY --from=build /out/server ./server
COPY --from=build /src/migrations ./migrations
EXPOSE 8080
ENTRYPOINT ["/app/server"]
