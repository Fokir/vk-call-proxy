# Multi-target Dockerfile for integration tests
# Targets: server, client, target, runner

# === Builder stage ===
FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

# Build server
FROM builder AS build-server
RUN CGO_ENABLED=0 go build -o /bin/server ./cmd/server

# Build client
FROM builder AS build-client
RUN CGO_ENABLED=0 go build -o /bin/client ./cmd/client

# Build target
FROM builder AS build-target
RUN CGO_ENABLED=0 go build -o /bin/target ./test/integration/target

# Build runner
FROM builder AS build-runner
RUN CGO_ENABLED=0 go build -o /bin/runner ./test/integration/runner

# === Runtime stages ===

FROM alpine:3.19 AS server
COPY --from=build-server /bin/server /usr/local/bin/server
ENTRYPOINT ["server"]

FROM alpine:3.19 AS client
COPY --from=build-client /bin/client /usr/local/bin/client
ENTRYPOINT ["client"]

FROM alpine:3.19 AS target
COPY --from=build-target /bin/target /usr/local/bin/target
ENTRYPOINT ["target"]

FROM alpine:3.19 AS runner
COPY --from=build-runner /bin/runner /usr/local/bin/runner
ENTRYPOINT ["runner"]
