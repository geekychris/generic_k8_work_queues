FROM golang:1.26-alpine AS builder

WORKDIR /app
RUN go mod init codereview-worker
COPY main.go .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o codereview-worker main.go

FROM alpine:3.20

RUN apk --no-cache add ca-certificates git
COPY --from=builder /app/codereview-worker /usr/local/bin/codereview-worker

EXPOSE 8080
ENTRYPOINT ["codereview-worker"]
