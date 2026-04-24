# KQueue Installation & Quick Start

## Prerequisites

| Tool | Version | Required For |
|------|---------|-------------|
| Go | 1.26+ | Building from source |
| Docker | 20+ | Building images, running locally |
| Docker Compose | v2 | Local development |
| kubectl | 1.28+ | Kubernetes deployment |
| kind or minikube | latest | Local Kubernetes cluster |
| curl | any | API interaction |

## Quick Start (docker-compose)

Five commands to a running system:

```bash
# 1. Clone the repository
git clone https://github.com/chris/kqueue.git
cd kqueue

# 2. Build all Docker images
make docker-build

# 3. Start all services
make docker-up

# 4. Check status
make status

# 5. Submit a test job
make submit-echo
```

This starts NATS, the controller, three workers (echo, nlp, sandbox), and their
sidecars.

**What is running:**

| URL | Service |
|-----|---------|
| http://localhost:8080 | Web dashboard + REST API |
| http://localhost:4222 | NATS client port |
| http://localhost:8222 | NATS monitoring dashboard |

## Verify It Works

**Submit a job and check its status:**

```bash
# Submit an echo job
curl -s -X POST http://localhost:8080/api/v1/jobs \
  -H 'Content-Type: application/json' \
  -d '{"queue":"echo","payload":{"action":"uppercase","text":"hello world"}}' \
  | python3 -m json.tool

# Note the job_id from the response, then check status:
curl -s http://localhost:8080/api/v1/jobs/<job_id> | python3 -m json.tool

# View per-job logs:
curl -s http://localhost:8080/api/v1/jobs/<job_id>/logs | python3 -m json.tool

# Check queue stats:
curl -s http://localhost:8080/api/v1/queues | python3 -m json.tool
```

**Open the dashboard**: visit http://localhost:8080 in a browser.

**Submit a batch to test scaling:**

```bash
make submit-batch    # Submits 20 echo jobs
make status          # Watch pending/completed counts
```

**Stop everything:**

```bash
make docker-down     # Stop containers
make clean           # Stop + remove volumes + build artifacts
```

## Kubernetes Deployment (with kind)

### Step 1: Create a cluster

```bash
kind create cluster --name kqueue
```

### Step 2: Build and load images

```bash
make docker-build

kind load docker-image kqueue-controller:latest --name kqueue
kind load docker-image kqueue-sidecar:latest --name kqueue
# Only if using the example queues:
kind load docker-image kqueue/echo-worker:latest --name kqueue
kind load docker-image kqueue/nlp-worker:latest --name kqueue
```

### Step 3: Deploy

**Option A -- Minimal (no example queues):**

```bash
kubectl apply -k deploy/minimal/
```

This deploys only NATS and the controller with an empty queue config. Then edit
the ConfigMap to add your own queues:

```bash
kubectl -n kqueue edit configmap kqueue-config
kubectl -n kqueue rollout restart deployment kqueue-controller
```

**Option B -- Full (with echo/nlp example queues):**

```bash
kubectl apply -k deploy/base/
```

Both options create:
- `kqueue` namespace
- NATS StatefulSet with JetStream and persistent storage
- KQueue controller Deployment with RBAC
- ConfigMap with queue definitions (empty for minimal, examples for full)

### Step 4: Wait for pods to be ready

```bash
kubectl -n kqueue get pods -w
```

Wait until all pods show `Running` and `Ready`.

### Step 5: Access the API

```bash
kubectl -n kqueue port-forward svc/kqueue-controller 8080:8080
```

In another terminal:

```bash
curl -s http://localhost:8080/api/v1/queues | python3 -m json.tool
curl -s -X POST http://localhost:8080/api/v1/jobs \
  -H 'Content-Type: application/json' \
  -d '{"queue":"echo","payload":{"action":"echo","text":"hello from k8s"}}'
```

### Step 6 (optional): gVisor for sandbox workers

If your nodes have gVisor installed:

```bash
kubectl apply -f deploy/examples/gvisor-runtimeclass.yaml
```

## Running Tests

```bash
# Unit tests -- autoscaler strategies, cost-aware scaling, config loading
# No external dependencies required
make test

# Integration tests -- HTTP API handlers (submit, get, list, validation)
# No NATS required
make test-integration

# End-to-end tests -- full request lifecycle against running services
# Requires: make docker-up
make test-e2e
```

For details on what each suite covers, running individual tests, and debugging
failures, see [Architecture - Running Tests](ARCHITECTURE.md#53-running-tests).

## Building from Source

```bash
# Build Go binaries
make build
# -> bin/controller, bin/sidecar

# Run the controller locally (needs a running NATS server)
docker run -d --name nats -p 4222:4222 -p 8222:8222 nats:2.10-alpine -js -m 8222
./bin/controller -config docker-compose.config.yaml --local
```

## Creating Your First Custom Worker

### Minimal Go Worker

Create `my-worker/main.go`:

```go
package main

import (
    "encoding/json"
    "log"
    "net/http"
)

type Request struct {
    JobID   string          `json:"job_id"`
    Payload json.RawMessage `json:"payload"`
}

type Response struct {
    Success bool        `json:"success"`
    Result  interface{} `json:"result,omitempty"`
    Error   string      `json:"error,omitempty"`
}

func main() {
    http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
        w.Write([]byte(`{"status":"ok"}`))
    })

    http.HandleFunc("/process", func(w http.ResponseWriter, r *http.Request) {
        var req Request
        json.NewDecoder(r.Body).Decode(&req)

        // Your processing logic here.
        log.Printf("processing job %s", req.JobID)

        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(Response{
            Success: true,
            Result:  map[string]string{"status": "done", "payload": string(req.Payload)},
        })
    })

    log.Fatal(http.ListenAndServe(":8080", nil))
}
```

Create `my-worker/Dockerfile`:

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

### Minimal Python Worker

Create `my-worker/main.py`:

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

    # Your processing logic here.
    app.logger.info("processing job %s", job_id)

    return jsonify(success=True, result={"status": "done", "input": payload})

if __name__ == "__main__":
    app.run(host="0.0.0.0", port=8080)
```

Create `my-worker/Dockerfile`:

```dockerfile
FROM python:3.12-slim
WORKDIR /app
RUN pip install flask
COPY . .
EXPOSE 8080
CMD ["python", "main.py"]
```

### Register the Worker

Add to `docker-compose.config.yaml` (or your config YAML):

```yaml
queues:
  - name: my-queue
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

Add to `docker-compose.yaml`:

```yaml
  my-worker:
    build: ./my-worker
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
    environment:
      - NATS_URL=nats://nats:4222
      - QUEUE_NAME=my-queue
      - QUEUE_SUBJECT=kqueue.my-queue
      - WORKER_URL=http://my-worker:8080/process
      - MAX_RETRIES=3
      - STREAM_PREFIX=kqueue
    depends_on:
      my-worker:
        condition: service_healthy
      controller:
        condition: service_started
```

Rebuild and restart:

```bash
make docker-build && make docker-up
```

Submit a job to your new queue:

```bash
curl -s -X POST http://localhost:8080/api/v1/jobs \
  -H 'Content-Type: application/json' \
  -d '{"queue":"my-queue","payload":{"hello":"world"}}'
```

## Troubleshooting

| Problem | Solution |
|---------|----------|
| Sidecar shows "failed to get stream" | Controller has not created streams yet. Restart sidecar: `docker compose restart my-sidecar` |
| Jobs stuck in pending | Check sidecar logs: `docker compose logs my-sidecar` |
| NATS connection refused | Check NATS is running: `docker compose ps nats` |
| Worker errors | Check worker logs: `docker compose logs my-worker` |
| Port 8080 conflict | Stop conflicting service or change `server.port` in config |
| K8s pods in CrashLoopBackOff | Check `kubectl -n kqueue describe pod <name>` and `kubectl -n kqueue logs <name> -c sidecar` |

For detailed debugging information, see `docs/ARCHITECTURE.md` section 6.
