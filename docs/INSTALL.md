# KQueue Installation Guide

## Prerequisites

- **Go 1.21+** (for building from source)
- **Docker** and **Docker Compose** (v2)
- **kubectl** (for Kubernetes deployment)
- **kind** or **minikube** (for local Kubernetes cluster)
- **curl** (for interacting with the API)

## Quick Start with Docker Compose

The fastest way to run KQueue locally. No Kubernetes cluster required.

```bash
# 1. Clone the repository
git clone https://github.com/chris/kqueue.git
cd kqueue

# 2. Build all Docker images
make docker-build

# 3. Start all services
make docker-up

# 4. Verify everything is running
make status

# 5. Submit a test job
make submit-echo
```

The web dashboard is available at http://localhost:8080. NATS monitoring is at
http://localhost:8222.

To stop everything:

```bash
make docker-down
```

To stop and remove all data volumes:

```bash
make clean
```

## Kubernetes Deployment with kind

### 1. Create a kind cluster

```bash
kind create cluster --name kqueue
```

### 2. Build and load images

```bash
make docker-build

kind load docker-image kqueue-controller:latest --name kqueue
kind load docker-image kqueue-sidecar:latest --name kqueue
kind load docker-image kqueue/echo-worker:latest --name kqueue
kind load docker-image kqueue/nlp-worker:latest --name kqueue
```

### 3. Apply Kubernetes manifests

```bash
kubectl apply -k deploy/base/
```

This creates the `kqueue` namespace and deploys NATS and the controller with
appropriate RBAC.

### 4. Verify the deployment

```bash
kubectl -n kqueue get pods
kubectl -n kqueue logs deployment/kqueue-controller
```

### 5. Port-forward to access the API

```bash
kubectl -n kqueue port-forward svc/kqueue-controller 8080:8080
```

Then submit jobs as usual:

```bash
curl -s -X POST http://localhost:8080/api/v1/jobs \
  -H 'Content-Type: application/json' \
  -d '{"queue":"echo","payload":{"message":"hello k8s"}}'
```

## Building from Source

```bash
# Build Go binaries
make build

# Binaries are placed in bin/
ls bin/
# controller  sidecar

# Run the controller locally (requires a running NATS server)
./bin/controller -config deploy/examples/config.yaml --local
```

To run NATS separately for development:

```bash
docker run -d --name nats -p 4222:4222 -p 8222:8222 nats:2.10-alpine -js -m 8222
```

## Configuration

KQueue is configured via a single YAML file passed with the `-config` flag.
See `deploy/examples/config.yaml` for a full example and `docs/ARCHITECTURE.md`
for a complete reference of all options.

Key settings:

| Section | Key | Default | Description |
|---------|-----|---------|-------------|
| `server.port` | int | 8080 | HTTP listen port |
| `server.ui_enabled` | bool | true | Serve the web dashboard |
| `nats.url` | string | `nats://nats:4222` | NATS server URL |
| `nats.stream_prefix` | string | `kqueue` | Prefix for JetStream stream names |
| `metrics.enabled` | bool | true | Enable Prometheus metrics |
| `metrics.port` | int | 9090 | Dedicated metrics port |

Queue-level settings are documented in `docs/ARCHITECTURE.md`.

## Creating a Custom Worker

Workers are simple HTTP servers. They must implement two endpoints:

### 1. Health check

```
GET /health  ->  200 OK  {"status":"ok"}
```

### 2. Process endpoint

```
POST /process
Content-Type: application/json

{
  "job_id": "uuid-string",
  "payload": { ... arbitrary JSON ... }
}
```

Response:

```json
{
  "success": true,
  "result": { ... },
  "error": ""
}
```

Return `"success": false` with an `"error"` message to signal failure. The
sidecar will retry up to `max_retries` times.

### Example: minimal Go worker

```go
package main

import (
    "encoding/json"
    "log"
    "net/http"
)

func main() {
    http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
        w.Write([]byte(`{"status":"ok"}`))
    })

    http.HandleFunc("/process", func(w http.ResponseWriter, r *http.Request) {
        var req struct {
            JobID   string          `json:"job_id"`
            Payload json.RawMessage `json:"payload"`
        }
        json.NewDecoder(r.Body).Decode(&req)

        // --- your processing logic here ---

        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(map[string]interface{}{
            "success": true,
            "result":  map[string]string{"echo": string(req.Payload)},
        })
    })

    log.Fatal(http.ListenAndServe(":8080", nil))
}
```

### Example: minimal Python worker

```python
from flask import Flask, request, jsonify

app = Flask(__name__)

@app.route("/health")
def health():
    return jsonify(status="ok")

@app.route("/process", methods=["POST"])
def process():
    data = request.get_json()
    job_id = data["job_id"]
    payload = data["payload"]

    # --- your processing logic here ---

    return jsonify(success=True, result={"received": payload})

if __name__ == "__main__":
    app.run(host="0.0.0.0", port=8080)
```

### 3. Create a Dockerfile

```dockerfile
FROM golang:1.23-alpine AS builder
WORKDIR /app
COPY . .
RUN CGO_ENABLED=0 go build -o worker .

FROM alpine:3.20
COPY --from=builder /app/worker /usr/local/bin/worker
EXPOSE 8080
ENTRYPOINT ["worker"]
```

### 4. Add to configuration

Add an entry to your config YAML:

```yaml
queues:
  - name: my-worker
    worker_image: my-worker:latest
    replicas:
      min: 1
      max: 10
    max_retries: 3
    processing_timeout: 60s
    scale_strategy:
      type: threshold
      scale_up_threshold: 10
      scale_down_threshold: 2
```

### 5. Add to docker-compose.yaml (for local dev)

```yaml
  my-worker:
    build: ./path/to/worker
    healthcheck:
      test: ["CMD", "wget", "-qO-", "http://localhost:8080/health"]
      interval: 5s
      timeout: 3s
      retries: 10
    depends_on:
      nats:
        condition: service_healthy

  my-sidecar:
    build:
      context: .
      dockerfile: Dockerfile.sidecar
    network_mode: "service:my-worker"
    environment:
      - NATS_URL=nats://nats:4222
      - QUEUE_NAME=my-worker
      - QUEUE_SUBJECT=kqueue.my-worker
      - WORKER_URL=http://localhost:8080/process
      - MAX_RETRIES=3
    depends_on:
      my-worker:
        condition: service_healthy
      controller:
        condition: service_started
```

## Troubleshooting

### Services fail to start

Check logs for connection errors:

```bash
docker compose logs controller
docker compose logs echo-sidecar
```

The most common issue is the sidecar starting before NATS streams are created.
The `depends_on` ordering in docker-compose.yaml handles this, but if you see
"failed to get stream" errors, restart the sidecars:

```bash
docker compose restart echo-sidecar nlp-sidecar
```

### Jobs stuck in pending

Verify the sidecar is running and connected:

```bash
docker compose logs echo-sidecar
```

Check that the NATS consumer exists:

```bash
curl -s http://localhost:8222/jsz?consumers=true | python3 -m json.tool
```

### NATS connection refused

Ensure NATS is running and healthy:

```bash
docker compose ps nats
curl http://localhost:8222/healthz
```

### Workers returning errors

Check worker logs:

```bash
docker compose logs echo-worker
docker compose logs nlp-worker
```

### Port conflicts

If port 8080, 4222, or 8222 is already in use, either stop the conflicting
service or override ports in docker-compose.yaml.

### Kubernetes: pods in CrashLoopBackOff

Check pod logs and events:

```bash
kubectl -n kqueue describe pod <pod-name>
kubectl -n kqueue logs <pod-name> -c sidecar
kubectl -n kqueue logs <pod-name> -c worker
```

Common causes:
- NATS not reachable from the pod network
- Worker image not available in the cluster (use `kind load` or push to a registry)
- Incorrect RBAC -- the controller needs permissions to manage deployments
