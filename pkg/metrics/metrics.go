package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// JobsSubmitted tracks the total number of jobs submitted per queue
	JobsSubmitted = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kqueue_jobs_submitted_total",
		Help: "Total number of jobs submitted",
	}, []string{"queue"})

	// JobsCompleted tracks the total number of jobs completed per queue
	JobsCompleted = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kqueue_jobs_completed_total",
		Help: "Total number of jobs completed",
	}, []string{"queue"})

	// JobsFailed tracks the total number of jobs failed per queue
	JobsFailed = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kqueue_jobs_failed_total",
		Help: "Total number of jobs failed",
	}, []string{"queue"})

	// JobsDeadLettered tracks the total number of jobs sent to dead letter queue
	JobsDeadLettered = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kqueue_jobs_dead_lettered_total",
		Help: "Total number of jobs sent to dead letter queue",
	}, []string{"queue"})

	// QueueDepth tracks the current queue depth by status
	QueueDepth = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kqueue_queue_depth",
		Help: "Current queue depth",
	}, []string{"queue", "status"})

	// WorkerReplicas tracks the current number of worker replicas per queue
	WorkerReplicas = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kqueue_worker_replicas",
		Help: "Current number of worker replicas",
	}, []string{"queue"})

	// JobProcessingDuration tracks the time taken to process a job
	JobProcessingDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "kqueue_job_processing_duration_seconds",
		Help:    "Time taken to process a job",
		Buckets: prometheus.ExponentialBuckets(0.1, 2, 10),
	}, []string{"queue"})

	// ScaleEvents tracks the total number of scale events
	ScaleEvents = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kqueue_scale_events_total",
		Help: "Total number of scale events",
	}, []string{"queue", "direction"})
)
