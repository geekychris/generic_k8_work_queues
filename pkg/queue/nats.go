package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/chris/kqueue/pkg/api"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Manager handles all queue operations via NATS JetStream
type Manager struct {
	nc           *nats.Conn
	js           jetstream.JetStream
	streamPrefix string
	streams      map[string]jetstream.Stream
	consumers    map[string]jetstream.Consumer
	mu           sync.RWMutex
	logger       *slog.Logger
}

// NewManager creates a new queue manager
func NewManager(natsURL, streamPrefix string, logger *slog.Logger) (*Manager, error) {
	nc, err := nats.Connect(natsURL,
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("connecting to NATS: %w", err)
	}

	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("creating JetStream context: %w", err)
	}

	return &Manager{
		nc:           nc,
		js:           js,
		streamPrefix: streamPrefix,
		streams:      make(map[string]jetstream.Stream),
		consumers:    make(map[string]jetstream.Consumer),
		logger:       logger,
	}, nil
}

// SetupQueue creates the JetStream stream and consumers for a queue
func (m *Manager) SetupQueue(ctx context.Context, cfg api.QueueConfig) error {
	streamName := m.streamPrefix + "_" + cfg.Name
	dlqStreamName := streamName + "_dlq"

	// Create main stream
	stream, err := m.js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      streamName,
		Subjects:  []string{cfg.Subject},
		Retention: jetstream.WorkQueuePolicy,
		MaxAge:    24 * time.Hour,
		Storage:   jetstream.FileStorage,
		Replicas:  1,
	})
	if err != nil {
		return fmt.Errorf("creating stream %s: %w", streamName, err)
	}

	// Create DLQ stream
	_, err = m.js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      dlqStreamName,
		Subjects:  []string{cfg.Subject + ".dlq"},
		Retention: jetstream.LimitsPolicy,
		MaxAge:    7 * 24 * time.Hour,
		Storage:   jetstream.FileStorage,
		Replicas:  1,
	})
	if err != nil {
		return fmt.Errorf("creating DLQ stream %s: %w", dlqStreamName, err)
	}

	// Create consumer for workers
	consumer, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Name:          cfg.Name + "_workers",
		Durable:       cfg.Name + "_workers",
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       cfg.ProcessingTimeout,
		MaxDeliver:    cfg.MaxRetries + 1,
		FilterSubject: cfg.Subject,
	})
	if err != nil {
		return fmt.Errorf("creating consumer: %w", err)
	}

	m.mu.Lock()
	m.streams[cfg.Name] = stream
	m.consumers[cfg.Name] = consumer
	m.mu.Unlock()

	m.logger.Info("queue setup complete", "queue", cfg.Name, "stream", streamName)
	return nil
}

// Publish sends a job to the queue
func (m *Manager) Publish(ctx context.Context, queueName, subject string, job *api.Job) error {
	data, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("marshaling job: %w", err)
	}

	_, err = m.js.Publish(ctx, subject, data,
		jetstream.WithMsgID(job.ID),
	)
	if err != nil {
		return fmt.Errorf("publishing to %s: %w", subject, err)
	}

	m.logger.Info("job published", "job_id", job.ID, "queue", queueName)
	return nil
}

// PublishToDLQ sends a failed job to the dead letter queue
func (m *Manager) PublishToDLQ(ctx context.Context, subject string, job *api.Job) error {
	job.Status = api.JobStatusDeadLetter
	job.UpdatedAt = time.Now()
	data, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("marshaling job for DLQ: %w", err)
	}

	_, err = m.js.Publish(ctx, subject+".dlq", data)
	if err != nil {
		return fmt.Errorf("publishing to DLQ: %w", err)
	}

	m.logger.Warn("job sent to DLQ", "job_id", job.ID, "queue", subject)
	return nil
}

// GetStats returns queue statistics
func (m *Manager) GetStats(ctx context.Context, queueName string) (*api.QueueStats, error) {
	m.mu.RLock()
	stream, ok := m.streams[queueName]
	consumer, consumerOk := m.consumers[queueName]
	m.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("queue %s not found", queueName)
	}

	streamInfo, err := stream.Info(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting stream info: %w", err)
	}

	stats := &api.QueueStats{
		Name: queueName,
	}

	if consumerOk {
		consumerInfo, err := consumer.Info(ctx)
		if err == nil {
			stats.Pending = int64(consumerInfo.NumPending)
			stats.Processing = int64(consumerInfo.NumAckPending)
		}
	}

	_ = streamInfo // total messages can be derived if needed
	return stats, nil
}

// NewJobID generates a new unique job ID
func NewJobID() string {
	return uuid.New().String()
}

// Close shuts down the queue manager
func (m *Manager) Close() {
	if m.nc != nil {
		m.nc.Close()
	}
}

// GetConsumer returns the consumer for a queue (used by sidecar)
func (m *Manager) GetConsumer(queueName string) (jetstream.Consumer, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.consumers[queueName]
	return c, ok
}
