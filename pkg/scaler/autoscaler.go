package scaler

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/chris/kqueue/pkg/api"
	"github.com/chris/kqueue/pkg/queue"
)

// WorkerScaler manages the interface for scaling workers
type WorkerScaler interface {
	Scale(ctx context.Context, queueName string, replicas int) error
	GetCurrentReplicas(ctx context.Context, queueName string) (int, error)
}

// Autoscaler monitors queues and scales workers
type Autoscaler struct {
	queueMgr      *queue.Manager
	workerScaler  WorkerScaler
	configs       map[string]api.QueueConfig
	lastScaleTime map[string]time.Time
	lastActivity  map[string]time.Time // tracks when pending+processing > 0 was last seen
	mu            sync.Mutex
	logger        *slog.Logger
	stopCh        chan struct{}
}

// NewAutoscaler creates a new autoscaler
func NewAutoscaler(queueMgr *queue.Manager, workerScaler WorkerScaler, logger *slog.Logger) *Autoscaler {
	return &Autoscaler{
		queueMgr:      queueMgr,
		workerScaler:  workerScaler,
		configs:       make(map[string]api.QueueConfig),
		lastScaleTime: make(map[string]time.Time),
		lastActivity:  make(map[string]time.Time),
		logger:        logger,
		stopCh:        make(chan struct{}),
	}
}

// AddQueue registers a queue for autoscaling
func (a *Autoscaler) AddQueue(cfg api.QueueConfig) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.configs[cfg.Name] = cfg
}

// Start begins the autoscaling loop
func (a *Autoscaler) Start(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	a.logger.Info("autoscaler started", "interval", interval)

	for {
		select {
		case <-ctx.Done():
			return
		case <-a.stopCh:
			return
		case <-ticker.C:
			a.evaluate(ctx)
		}
	}
}

// Stop stops the autoscaler
func (a *Autoscaler) Stop() {
	close(a.stopCh)
}

func (a *Autoscaler) evaluate(ctx context.Context) {
	a.mu.Lock()
	configs := make(map[string]api.QueueConfig, len(a.configs))
	for k, v := range a.configs {
		configs[k] = v
	}
	a.mu.Unlock()

	for name, cfg := range configs {
		stats, err := a.queueMgr.GetStats(ctx, name)
		if err != nil {
			a.logger.Error("failed to get queue stats", "queue", name, "error", err)
			continue
		}

		currentReplicas, err := a.workerScaler.GetCurrentReplicas(ctx, name)
		if err != nil {
			a.logger.Error("failed to get current replicas", "queue", name, "error", err)
			continue
		}

		// Track activity for cost-aware scaling
		a.mu.Lock()
		if stats.Pending+stats.Processing > 0 {
			a.lastActivity[name] = time.Now()
		}
		a.mu.Unlock()

		desired := a.calculateDesired(cfg, stats, currentReplicas)

		if desired != currentReplicas {
			// Check cooldown
			a.mu.Lock()
			lastScale, ok := a.lastScaleTime[name]
			cooldown := time.Duration(cfg.ScaleStrategy.CooldownSeconds) * time.Second

			// Cost-aware: use longer effective cooldown for scale-down
			isScaleDown := desired < currentReplicas
			if isScaleDown && cfg.ScaleStrategy.StartupCostSeconds > 0 {
				scaleDownDelay := time.Duration(cfg.ScaleStrategy.ScaleDownDelaySeconds) * time.Second
				startupCost := time.Duration(cfg.ScaleStrategy.StartupCostSeconds) * time.Second
				cooldown = cooldown + scaleDownDelay + startupCost
			}

			// Check if recent activity makes scale-down too costly
			if isScaleDown && cfg.ScaleStrategy.StartupCostSeconds > 0 {
				lastAct, hasActivity := a.lastActivity[name]
				recentWindow := time.Duration(cfg.ScaleStrategy.StartupCostSeconds*2) * time.Second
				if hasActivity && time.Since(lastAct) < recentWindow {
					a.mu.Unlock()
					a.logger.Info("cost-aware: skipping scale-down due to recent activity",
						"queue", name,
						"last_activity_ago", time.Since(lastAct),
						"startup_cost_seconds", cfg.ScaleStrategy.StartupCostSeconds,
					)
					continue
				}
			}
			a.mu.Unlock()

			if ok && time.Since(lastScale) < cooldown {
				a.logger.Debug("cooldown active, skipping scale", "queue", name)
				continue
			}

			a.logger.Info("scaling workers",
				"queue", name,
				"from", currentReplicas,
				"to", desired,
				"pending", stats.Pending,
				"processing", stats.Processing,
				"strategy", cfg.ScaleStrategy.Type,
			)

			if err := a.workerScaler.Scale(ctx, name, desired); err != nil {
				a.logger.Error("failed to scale", "queue", name, "error", err)
				continue
			}

			a.mu.Lock()
			a.lastScaleTime[name] = time.Now()
			a.mu.Unlock()
		}
	}
}

func (a *Autoscaler) calculateDesired(cfg api.QueueConfig, stats *api.QueueStats, current int) int {
	var desired int
	pending := int(stats.Pending)
	processing := int(stats.Processing)
	total := pending + processing

	switch cfg.ScaleStrategy.Type {
	case "threshold":
		desired = a.thresholdStrategy(cfg, total, current)
	case "rate":
		desired = a.rateStrategy(cfg, total, current)
	case "target_per_worker":
		desired = a.targetPerWorkerStrategy(cfg, total, current)
	default:
		desired = a.thresholdStrategy(cfg, total, current)
	}

	// Clamp to min/max
	if desired < cfg.Replicas.Min {
		desired = cfg.Replicas.Min
	}
	if desired > cfg.Replicas.Max {
		desired = cfg.Replicas.Max
	}

	// Cost-aware: apply min idle workers floor
	if cfg.ScaleStrategy.MinIdleWorkers > 0 {
		idleFloor := cfg.Replicas.Min + cfg.ScaleStrategy.MinIdleWorkers
		if idleFloor > cfg.Replicas.Max {
			idleFloor = cfg.Replicas.Max
		}
		if desired < idleFloor {
			desired = idleFloor
		}
	}

	// Cost-aware: limit scale-down rate
	if desired < current && cfg.ScaleStrategy.ScaleDownRate > 0 {
		maxRemove := int(math.Ceil(float64(current) * cfg.ScaleStrategy.ScaleDownRate))
		if maxRemove < 1 {
			maxRemove = 1
		}
		minAllowed := current - maxRemove
		if desired < minAllowed {
			desired = minAllowed
		}
		// Re-clamp to min after rate limiting
		if desired < cfg.Replicas.Min {
			desired = cfg.Replicas.Min
		}
	}

	return desired
}

func (a *Autoscaler) thresholdStrategy(cfg api.QueueConfig, totalWork, current int) int {
	if totalWork > cfg.ScaleStrategy.ScaleUpThreshold {
		return current + cfg.ScaleStrategy.ScaleUpStep
	}
	if totalWork < cfg.ScaleStrategy.ScaleDownThreshold && current > cfg.Replicas.Min {
		return current - cfg.ScaleStrategy.ScaleDownStep
	}
	return current
}

func (a *Autoscaler) rateStrategy(cfg api.QueueConfig, totalWork, current int) int {
	// Scale proportionally to work volume
	if current == 0 {
		if totalWork > 0 {
			return cfg.Replicas.Min
		}
		return 0
	}

	workPerWorker := float64(totalWork) / float64(current)
	if workPerWorker > float64(cfg.ScaleStrategy.ScaleUpThreshold) {
		return current + cfg.ScaleStrategy.ScaleUpStep
	}
	if workPerWorker < float64(cfg.ScaleStrategy.ScaleDownThreshold) && current > cfg.Replicas.Min {
		return current - cfg.ScaleStrategy.ScaleDownStep
	}
	return current
}

func (a *Autoscaler) targetPerWorkerStrategy(cfg api.QueueConfig, totalWork, current int) int {
	if totalWork == 0 {
		return cfg.Replicas.Min
	}
	target := cfg.ScaleStrategy.TargetPerWorker
	desired := int(math.Ceil(float64(totalWork) / float64(target)))
	return desired
}

// GetDesiredReplicas exposes the calculation for testing
func (a *Autoscaler) GetDesiredReplicas(cfg api.QueueConfig, stats *api.QueueStats, current int) int {
	return a.calculateDesired(cfg, stats, current)
}
