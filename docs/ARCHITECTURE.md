# KQueue Architecture

## System Overview

KQueue is a scalable job queue framework for Kubernetes. Jobs are submitted via
a REST API, routed through NATS JetStream, and processed by worker containers
paired with a sidecar that handles queue protocol, retries, and status reporting.

```
                          +------------------+
                          |   Clients/API    |
                          | POST /api/v1/jobs|
                          +--------+---------+
                                   |
                                   v
                          +------------------+
                          |   Controller     |
                          |  - REST API      |
                          |  - Web UI        |
                          |  - Autoscaler    |
                          |  - Metrics       |
                          +--------+---------+
                                   |
                        publish    |   subscribe events
                     +-------------+-------------+
                     |                           |
                     v                           |
              +------+-------+                   |
              | NATS JetStream|<---- status -----+
              |              |       events
              | Streams:     |
              |  kqueue_echo |
              |  kqueue_nlp  |
              |  *_dlq       |
              +------+-------+
                     |
          +----------+----------+
          |                     |
          v                     v
   +------+-------+     +------+-------+
   | Pod (echo)   |     | Pod (nlp)    |
   |              |     |              |
   | +----------+ |     | +----------+ |
   | | Sidecar  | |     | | Sidecar  | |
   | | - fetch  | |     | | - fetch  | |
   | | - ack/nak| |     | | - ack/nak| |
   | +----+-----+ |     | +----+-----+ |
   |      | HTTP   |     |      | HTTP   |
   |      v        |     |      v        |
   | +----------+ |     | +----------+ |
   | | Worker   | |     | | Worker   | |
   | | echo-    | |     | | nlp-     | |
   | | worker   | |     | | worker   | |
   | +----------+ |     | +----------+ |
   +--------------+     +--------------+
```

## Components

### Controller

The controller (`cmd/controller/`) is the central service. It performs several
roles:

- **REST API server** -- accepts job submissions (`POST /api/v1/jobs`), exposes
  job status and queue statistics, and manages the dead letter queue.
- **Queue setup** -- on startup it connects to NATS and creates JetStream
  streams and consumers for every configured queue.
- **Autoscaler** -- periodically evaluates queue depth and adjusts worker
  replica counts through the Kubernetes API (or a no-op scaler in local mode).
- **Status listener** -- subscribes to NATS status events published by sidecars
  to track job lifecycle in an in-memory store.
- **Web dashboard** -- serves a single-page UI from embedded static files
  (`ui/static/index.html`).
- **Prometheus metrics** -- exports counters and histograms on `/metrics`.

Flag reference:
- `-config <path>` -- path to the YAML configuration file (default `config.yaml`)
- `--local` -- skip Kubernetes deployer, use a no-op scaler instead

### NATS JetStream

NATS JetStream provides durable, at-least-once message delivery. For each
configured queue the controller creates:

| Stream | Retention | Purpose |
|--------|-----------|---------|
| `kqueue_<name>` | WorkQueue | Main job stream. Messages are removed once acknowledged. |
| `kqueue_<name>_dlq` | Limits (7 days) | Dead letter queue for jobs that exhaust retries. |

A durable consumer named `<name>_workers` is created on the main stream with
explicit acknowledgement. Multiple sidecar instances share this consumer, so
messages are distributed across workers automatically.

### Worker Sidecar

The sidecar (`cmd/sidecar/`) runs alongside every worker container. It:

1. Connects to NATS and fetches messages from the durable consumer.
2. Sends each job payload to the worker via `POST http://localhost:8080/process`.
3. On success, acknowledges the message and publishes a `completed` status event.
4. On failure, either NAKs the message for redelivery or, after max retries,
   acknowledges it and publishes to the DLQ stream.
5. Publishes status events on `kqueue.events.<queue>` for the controller.

Environment variables:
- `NATS_URL` -- NATS server address
- `QUEUE_NAME` -- queue this sidecar processes
- `QUEUE_SUBJECT` -- NATS subject (defaults to `kqueue.<QUEUE_NAME>`)
- `WORKER_URL` -- HTTP endpoint of the worker (default `http://localhost:8080/process`)
- `MAX_RETRIES` -- maximum delivery attempts before dead-lettering
- `STREAM_PREFIX` -- prefix for stream names (default `kqueue`)

### Worker Container

Workers are user-provided HTTP servers that implement a simple contract:

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/process` | POST | Receive and process a job. |
| `/health` | GET | Return 200 when ready. |

**Request** (`/process`):
```json
{
  "job_id": "uuid",
  "payload": { ... }
}
```

**Response** (`/process`):
```json
{
  "success": true,
  "result": { ... },
  "error": ""
}
```

The worker does not need any knowledge of NATS, Kubernetes, or the queue
protocol. It simply processes HTTP requests and returns results.

### Autoscaler

The autoscaler (`pkg/scaler/autoscaler.go`) runs on a 10-second tick inside the
controller. On each tick it:

1. Fetches queue depth (pending + in-flight) from NATS consumer info.
2. Gets the current replica count from Kubernetes.
3. Computes the desired replica count using the configured strategy.
4. Clamps the result to `[min, max]` replicas.
5. Applies a cooldown window before issuing another scale event.

## Data Flow

```
1. Client  -->  POST /api/v1/jobs {queue:"echo", payload:{...}}
2. Controller publishes job JSON to NATS subject "kqueue.echo"
3. Controller stores job in memory with status "pending"
4. Sidecar fetches message from consumer "echo_workers"
5. Sidecar POSTs {job_id, payload} to worker at localhost:8080/process
6. Worker processes and returns {success:true, result:{...}}
7. Sidecar ACKs NATS message
8. Sidecar publishes status event on "kqueue.events.echo"
9. Controller receives event, updates job status to "completed"
```

On failure the sidecar NAKs the message. NATS redelivers it (up to
`MaxDeliver` times). If all attempts are exhausted the sidecar publishes the
job to the DLQ stream and sends a `dead_letter` status event.

## Autoscaling Strategies

### threshold

Scale up or down based on absolute queue depth.

| Parameter | Description |
|-----------|-------------|
| `scale_up_threshold` | If total work > threshold, add `scale_up_step` replicas |
| `scale_down_threshold` | If total work < threshold, remove `scale_down_step` replicas |

Example with `scale_up_threshold=10, scale_up_step=2`:
- Queue depth 12, current replicas 2 -> desired 4
- Queue depth 5, current replicas 4 -> no change
- Queue depth 1, `scale_down_threshold=2` -> desired 3

### rate

Scale based on work-per-worker ratio.

| Parameter | Description |
|-----------|-------------|
| `scale_up_threshold` | If work/worker > threshold, add replicas |
| `scale_down_threshold` | If work/worker < threshold, remove replicas |

Example with `scale_up_threshold=10`:
- 30 pending, 3 workers -> 10 per worker -> at threshold
- 40 pending, 3 workers -> 13.3 per worker -> scale up

### target_per_worker

Compute desired replicas directly as `ceil(total_work / target_per_worker)`.

| Parameter | Description |
|-----------|-------------|
| `target_per_worker` | Desired number of jobs per worker |

Example with `target_per_worker=5`:
- 23 pending -> ceil(23/5) = 5 workers
- 3 pending -> ceil(3/5) = 1 worker
- 0 pending -> falls back to `min` replicas

All strategies respect the `cooldown_seconds` setting to prevent flapping.

## Dead Letter Queue

When a job fails processing more than `max_retries` times:

1. The sidecar ACKs the original message (preventing further redelivery).
2. The job is published to the DLQ stream (`kqueue.<name>.dlq`).
3. A `dead_letter` status event is sent to the controller.
4. The controller stores the job in its DLQ store.

DLQ jobs can be inspected via `GET /api/v1/queues/<name>/dlq` and retried via
`POST /api/v1/queues/<name>/dlq/<jobId>/retry`, which resets the attempt
counter and re-publishes the job to the main stream.

## Metrics

The controller exports Prometheus metrics on `/metrics` (port 8080) and
optionally on a dedicated metrics port (default 9090).

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `kqueue_jobs_submitted_total` | Counter | queue | Jobs submitted |
| `kqueue_jobs_completed_total` | Counter | queue | Jobs completed |
| `kqueue_jobs_failed_total` | Counter | queue | Job failures (retriable) |
| `kqueue_jobs_dead_lettered_total` | Counter | queue | Jobs sent to DLQ |
| `kqueue_queue_depth` | Gauge | queue, status | Current queue depth |
| `kqueue_worker_replicas` | Gauge | queue | Current replica count |
| `kqueue_job_processing_duration_seconds` | Histogram | queue | Processing time |
| `kqueue_scale_events_total` | Counter | queue, direction | Scale up/down events |

To scrape with Prometheus, add a target for `controller:8080` or
`controller:9090`.

## Configuration Reference

Configuration is provided as a YAML file. See `deploy/examples/config.yaml` for
a complete example.

```yaml
server:
  port: 8080            # HTTP listen port
  ui_enabled: true      # Serve web dashboard

nats:
  url: nats://nats:4222 # NATS server URL
  stream_prefix: kqueue # Prefix for JetStream stream names

queues:
  - name: echo                    # Queue identifier
    subject: kqueue.echo          # NATS subject (auto-derived if omitted)
    worker_image: img:tag         # Docker image for the worker container
    replicas:
      min: 1                      # Minimum worker replicas
      max: 10                     # Maximum worker replicas
    resources:
      cpu_request: "100m"
      cpu_limit: "500m"
      memory_request: "64Mi"
      memory_limit: "256Mi"
      gpu_limit: ""               # Optional GPU request
    max_retries: 3                # Attempts before dead-lettering
    processing_timeout: 60s       # Ack wait / HTTP timeout
    scale_strategy:
      type: threshold             # threshold | rate | target_per_worker
      scale_up_threshold: 10
      scale_down_threshold: 2
      target_per_worker: 5        # Used by target_per_worker strategy
      cooldown_seconds: 30
      scale_up_step: 2
      scale_down_step: 1

metrics:
  enabled: true
  port: 9090                      # Dedicated metrics port
```
