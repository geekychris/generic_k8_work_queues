FROM golang:1.26-alpine AS builder

WORKDIR /app
RUN go mod init sandbox-worker
COPY main.go .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o sandbox-worker main.go

FROM alpine:3.20

RUN apk --no-cache add ca-certificates bash coreutils curl jq python3
COPY --from=builder /app/sandbox-worker /usr/local/bin/sandbox-worker

EXPOSE 8080
ENTRYPOINT ["sandbox-worker"]
