package api

import (
	"encoding/json"
	"time"
)

// JobStatus represents the state of a job
type JobStatus string

const (
	JobStatusPending    JobStatus = "pending"
	JobStatusProcessing JobStatus = "processing"
	JobStatusCompleted  JobStatus = "completed"
	JobStatusFailed     JobStatus = "failed"
	JobStatusDeadLetter JobStatus = "dead_letter"
)

// Job represents a unit of work
type Job struct {
	ID          string            `json:"id"`
	Queue       string            `json:"queue"`
	Payload     json.RawMessage   `json:"payload"`
	Status      JobStatus         `json:"status"`
	Result      json.RawMessage   `json:"result,omitempty"`
	Error       string            `json:"error,omitempty"`
	Attempts    int               `json:"attempts"`
	MaxRetries  int               `json:"max_retries"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
	StartedAt   *time.Time        `json:"started_at,omitempty"`
	CompletedAt *time.Time        `json:"completed_at,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// QueueConfig defines a queue and its worker configuration
type QueueConfig struct {
	Name              string         `json:"name" yaml:"name"`
	Subject           string         `json:"subject" yaml:"subject"` // NATS subject
	WorkerImage       string         `json:"worker_image" yaml:"worker_image"`
	Replicas          ReplicaConfig  `json:"replicas" yaml:"replicas"`
	Resources         ResourceConfig `json:"resources" yaml:"resources"`
	MaxRetries        int            `json:"max_retries" yaml:"max_retries"`
	ProcessingTimeout time.Duration  `json:"processing_timeout" yaml:"processing_timeout"`
	ScaleStrategy     ScaleStrategy  `json:"scale_strategy" yaml:"scale_strategy"`
	PayloadSchema     interface{}    `json:"payload_schema,omitempty" yaml:"payload_schema,omitempty"` // JSON Schema for payload validation
	RuntimeClass      string         `json:"runtime_class,omitempty" yaml:"runtime_class,omitempty"`   // Kubernetes RuntimeClass (e.g. "gvisor")
}

// ReplicaConfig defines min/max replicas
type ReplicaConfig struct {
	Min int `json:"min" yaml:"min"`
	Max int `json:"max" yaml:"max"`
}

// ResourceConfig defines pod resource requests/limits
type ResourceConfig struct {
	CPURequest    string `json:"cpu_request" yaml:"cpu_request"`
	CPULimit      string `json:"cpu_limit" yaml:"cpu_limit"`
	MemoryRequest string `json:"memory_request" yaml:"memory_request"`
	MemoryLimit   string `json:"memory_limit" yaml:"memory_limit"`
	GPULimit      string `json:"gpu_limit,omitempty" yaml:"gpu_limit,omitempty"`
}

// ScaleStrategy configures autoscaling behavior
type ScaleStrategy struct {
	Type               string `json:"type" yaml:"type"` // "threshold", "rate", "target_per_worker"
	ScaleUpThreshold   int    `json:"scale_up_threshold" yaml:"scale_up_threshold"`
	ScaleDownThreshold int    `json:"scale_down_threshold" yaml:"scale_down_threshold"`
	TargetPerWorker    int    `json:"target_per_worker" yaml:"target_per_worker"`
	CooldownSeconds    int    `json:"cooldown_seconds" yaml:"cooldown_seconds"`
	ScaleUpStep        int    `json:"scale_up_step" yaml:"scale_up_step"`
	ScaleDownStep      int    `json:"scale_down_step" yaml:"scale_down_step"`

	// Cost-aware scaling fields
	StartupCostSeconds    int     `json:"startup_cost_seconds" yaml:"startup_cost_seconds"`         // Estimated time to start a worker (load model, warm cache, etc.)
	ScaleDownDelaySeconds int     `json:"scale_down_delay_seconds" yaml:"scale_down_delay_seconds"` // Extra delay before scaling down (on top of cooldown)
	MinIdleWorkers        int     `json:"min_idle_workers" yaml:"min_idle_workers"`                 // Keep this many idle workers warm
	ScaleDownRate         float64 `json:"scale_down_rate" yaml:"scale_down_rate"`                   // Max fraction of workers to remove per scale-down (0.0-1.0, default 0.5)
}

// SubmitRequest is the API request to submit a job
type SubmitRequest struct {
	Queue    string            `json:"queue"`
	Payload  json.RawMessage   `json:"payload"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// SubmitResponse is the API response after submitting a job
type SubmitResponse struct {
	JobID  string    `json:"job_id"`
	Queue  string    `json:"queue"`
	Status JobStatus `json:"status"`
}

// QueueStats holds statistics for a queue
type QueueStats struct {
	Name       string `json:"name"`
	Pending    int64  `json:"pending"`
	Processing int64  `json:"processing"`
	Completed  int64  `json:"completed"`
	Failed     int64  `json:"failed"`
	DeadLetter int64  `json:"dead_letter"`
	Workers    int    `json:"workers"`
	MinWorkers int    `json:"min_workers"`
	MaxWorkers int    `json:"max_workers"`
}

// WorkerProcessRequest is sent to the worker container
type WorkerProcessRequest struct {
	JobID   string          `json:"job_id"`
	Payload json.RawMessage `json:"payload"`
}

// WorkerProcessResponse is returned by the worker container
type WorkerProcessResponse struct {
	Success bool            `json:"success"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   string          `json:"error,omitempty"`
	Logs    []WorkerLog     `json:"logs,omitempty"` // Optional log entries forwarded to the controller
}

// WorkerLog is a log entry returned by a worker for per-job log tracking.
type WorkerLog struct {
	Level   string `json:"level"` // "info", "warn", "error"
	Message string `json:"message"`
}
