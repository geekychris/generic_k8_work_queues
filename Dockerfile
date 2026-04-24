FROM golang:1.26-alpine AS builder

RUN apk --no-cache add ca-certificates

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o controller ./cmd/controller

FROM alpine:3.20

RUN apk --no-cache add ca-certificates

COPY --from=builder /app/controller /usr/local/bin/controller

EXPOSE 8080 9090
ENTRYPOINT ["controller"]
