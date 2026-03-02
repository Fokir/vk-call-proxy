# Build stage
FROM golang:1.23-alpine AS builder

RUN apk add --no-cache git

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /server ./cmd/server

# Runtime stage (distroless)
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /server /server

EXPOSE 9000/udp

USER nonroot:nonroot

ENTRYPOINT ["/server"]
CMD ["--listen=0.0.0.0:9000"]
