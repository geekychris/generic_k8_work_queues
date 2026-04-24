FROM golang:1.26-alpine AS builder

WORKDIR /app
RUN go mod init echo-worker
COPY main.go .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o echo-worker main.go

FROM alpine:3.20

RUN apk --no-cache add ca-certificates
COPY --from=builder /app/echo-worker /usr/local/bin/echo-worker

EXPOSE 8080
ENTRYPOINT ["echo-worker"]
