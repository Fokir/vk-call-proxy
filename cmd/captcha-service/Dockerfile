FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /captcha-service ./cmd/captcha-service/

FROM chromedp/headless-shell:latest
COPY --from=builder /captcha-service /usr/local/bin/captcha-service
EXPOSE 8090
ENTRYPOINT ["captcha-service"]
