package scaler

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"testing"

	"github.com/chris/kqueue/pkg/api"
)

// mockWorkerScaler records all Scale calls and returns configurable replicas.
type mockWorkerScaler struct {
	mu              sync.Mutex
	replicasByQueue map[string]int
	scaleCalls      []scaleCall
}

type scaleCall struct {
	QueueName string
	Replicas  int
}

func newMockWorkerScaler() *mockWorkerScaler {
	return &mockWorkerScaler{
		replicasByQueue: make(map[string]int),
	}
}

func (m *mockWorkerScaler) Scale(_ context.Context, queueName string, replicas int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.replicasByQueue[queueName] = replicas
	m.scaleCalls = append(m.scaleCalls, scaleCall{QueueName: queueName, Replicas: replicas})
	return nil
}

func (m *mockWorkerScaler) GetCurrentReplicas(_ context.Context, queueName string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.replicasByQueue[queueName]
	if !ok {
		return 0, nil
	}
	return r, nil
}

func (m *mockWorkerScaler) getScaleCalls() []scaleCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]scaleCall, len(m.scaleCalls))
	copy(cp, m.scaleCalls)
	return cp
}

func newTestAutoscaler(t *testing.T) (*Autoscaler, *mockWorkerScaler) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	mock := newMockWorkerScaler()
	as := NewAutoscaler(nil, mock, logger)
	return as, mock
}

func baseCfg() api.QueueConfig {
	return api.QueueConfig{
		Name:    "test-queue",
		Subject: "kqueue.test",
		Replicas: api.ReplicaConfig{
			Min: 1,
			Max: 20,
		},
		ScaleStrategy: api.ScaleStrategy{
			Type:               "threshold",
			ScaleUpThreshold:   10,
			ScaleDownThreshold: 2,
			TargetPerWorker:    5,
			CooldownSeconds:    30,
			ScaleUpStep:        2,
			ScaleDownStep:      1,
		},
	}
}

// ---- Threshold Strategy Tests ----

func TestThresholdStrategy_ScaleUp(t *testing.T) {
	as, _ := newTestAutoscaler(t)
	cfg := baseCfg()
	cfg.ScaleStrategy.Type = "threshold"

	stats := &api.QueueStats{Pending: 15, Processing: 0}
	current := 3
	desired := as.GetDesiredReplicas(cfg, stats, current)

	// totalWork=15 > ScaleUpThreshold=10 => current + ScaleUpStep = 3+2 = 5
	if desired != 5 {
		t.Errorf("expected 5 replicas, got %d", desired)
	}
}

func TestThresholdStrategy_ScaleDown(t *testing.T) {
	as, _ := newTestAutoscaler(t)
	cfg := baseCfg()
	cfg.ScaleStrategy.Type = "threshold"

	stats := &api.QueueStats{Pending: 1, Processing: 0}
	current := 5
	desired := as.GetDesiredReplicas(cfg, stats, current)

	// totalWork=1 < ScaleDownThreshold=2, current > min => 5 - 1 = 4
	if desired != 4 {
		t.Errorf("expected 4 replicas, got %d", desired)
	}
}

func TestThresholdStrategy_StayWhenBetween(t *testing.T) {
	as, _ := newTestAutoscaler(t)
	cfg := baseCfg()
	cfg.ScaleStrategy.Type = "threshold"

	stats := &api.QueueStats{Pending: 5, Processing: 0}
	current := 3
	desired := as.GetDesiredReplicas(cfg, stats, current)

	// totalWork=5, between 2 and 10 => no change
	if desired != 3 {
		t.Errorf("expected 3 replicas (no change), got %d", desired)
	}
}

func TestThresholdStrategy_ExactlyAtUpThreshold(t *testing.T) {
	as, _ := newTestAutoscaler(t)
	cfg := baseCfg()
	cfg.ScaleStrategy.Type = "threshold"

	stats := &api.QueueStats{Pending: 10, Processing: 0}
	current := 3
	desired := as.GetDesiredReplicas(cfg, stats, current)

	// totalWork=10, not > 10 => no scale up; not < 2 => no scale down
	if desired != 3 {
		t.Errorf("expected 3 replicas (at threshold boundary), got %d", desired)
	}
}

func TestThresholdStrategy_PendingPlusProcessing(t *testing.T) {
	as, _ := newTestAutoscaler(t)
	cfg := baseCfg()
	cfg.ScaleStrategy.Type = "threshold"

	stats := &api.QueueStats{Pending: 6, Processing: 6}
	current := 3
	desired := as.GetDesiredReplicas(cfg, stats, current)

	// totalWork = 6+6 = 12 > 10 => scale up to 5
	if desired != 5 {
		t.Errorf("expected 5 replicas, got %d", desired)
	}
}

// ---- Rate Strategy Tests ----

func TestRateStrategy_ScaleUp(t *testing.T) {
	as, _ := newTestAutoscaler(t)
	cfg := baseCfg()
	cfg.ScaleStrategy.Type = "rate"
	cfg.ScaleStrategy.ScaleUpThreshold = 5 // workPerWorker threshold

	stats := &api.QueueStats{Pending: 30, Processing: 0}
	current := 3
	desired := as.GetDesiredReplicas(cfg, stats, current)

	// workPerWorker = 30/3 = 10 > 5 => 3 + 2 = 5
	if desired != 5 {
		t.Errorf("expected 5 replicas, got %d", desired)
	}
}

func TestRateStrategy_ScaleDown(t *testing.T) {
	as, _ := newTestAutoscaler(t)
	cfg := baseCfg()
	cfg.ScaleStrategy.Type = "rate"
	cfg.ScaleStrategy.ScaleDownThreshold = 2

	stats := &api.QueueStats{Pending: 3, Processing: 0}
	current := 5
	desired := as.GetDesiredReplicas(cfg, stats, current)

	// workPerWorker = 3/5 = 0.6 < 2 => 5 - 1 = 4
	if desired != 4 {
		t.Errorf("expected 4 replicas, got %d", desired)
	}
}

func TestRateStrategy_ZeroWorkers_WithWork(t *testing.T) {
	as, _ := newTestAutoscaler(t)
	cfg := baseCfg()
	cfg.ScaleStrategy.Type = "rate"

	stats := &api.QueueStats{Pending: 10, Processing: 0}
	current := 0
	desired := as.GetDesiredReplicas(cfg, stats, current)

	// 0 workers with work => returns Min
	if desired != cfg.Replicas.Min {
		t.Errorf("expected %d replicas (min), got %d", cfg.Replicas.Min, desired)
	}
}

func TestRateStrategy_ZeroWorkers_NoWork(t *testing.T) {
	as, _ := newTestAutoscaler(t)
	cfg := baseCfg()
	cfg.ScaleStrategy.Type = "rate"
	cfg.Replicas.Min = 0 // allow scaling to zero

	stats := &api.QueueStats{Pending: 0, Processing: 0}
	current := 0
	desired := as.GetDesiredReplicas(cfg, stats, current)

	// 0 workers, 0 work => 0, clamped to min=0
	if desired != 0 {
		t.Errorf("expected 0 replicas, got %d", desired)
	}
}

// ---- Target Per Worker Strategy Tests ----

func TestTargetPerWorker_BasicCalculation(t *testing.T) {
	as, _ := newTestAutoscaler(t)
	cfg := baseCfg()
	cfg.ScaleStrategy.Type = "target_per_worker"
	cfg.ScaleStrategy.TargetPerWorker = 5

	stats := &api.QueueStats{Pending: 23, Processing: 0}
	current := 2
	desired := as.GetDesiredReplicas(cfg, stats, current)

	// ceil(23/5) = 5
	if desired != 5 {
		t.Errorf("expected 5 replicas, got %d", desired)
	}
}

func TestTargetPerWorker_ExactDivision(t *testing.T) {
	as, _ := newTestAutoscaler(t)
	cfg := baseCfg()
	cfg.ScaleStrategy.Type = "target_per_worker"
	cfg.ScaleStrategy.TargetPerWorker = 10

	stats := &api.QueueStats{Pending: 20, Processing: 10}
	current := 2
	desired := as.GetDesiredReplicas(cfg, stats, current)

	// ceil(30/10) = 3
	if desired != 3 {
		t.Errorf("expected 3 replicas, got %d", desired)
	}
}

func TestTargetPerWorker_ZeroWork(t *testing.T) {
	as, _ := newTestAutoscaler(t)
	cfg := baseCfg()
	cfg.ScaleStrategy.Type = "target_per_worker"

	stats := &api.QueueStats{Pending: 0, Processing: 0}
	current := 5
	desired := as.GetDesiredReplicas(cfg, stats, current)

	// zero work => Min
	if desired != cfg.Replicas.Min {
		t.Errorf("expected %d replicas (min), got %d", cfg.Replicas.Min, desired)
	}
}

func TestTargetPerWorker_SingleJob(t *testing.T) {
	as, _ := newTestAutoscaler(t)
	cfg := baseCfg()
	cfg.ScaleStrategy.Type = "target_per_worker"
	cfg.ScaleStrategy.TargetPerWorker = 5

	stats := &api.QueueStats{Pending: 1, Processing: 0}
	current := 0
	desired := as.GetDesiredReplicas(cfg, stats, current)

	// ceil(1/5) = 1, clamped to min=1
	if desired != 1 {
		t.Errorf("expected 1 replica, got %d", desired)
	}
}

// ---- Min/Max Clamping Tests ----

func TestClamping_NeverBelowMin(t *testing.T) {
	as, _ := newTestAutoscaler(t)
	cfg := baseCfg()
	cfg.Replicas.Min = 3
	cfg.Replicas.Max = 20
	cfg.ScaleStrategy.Type = "threshold"

	// Scale down scenario that would go below min
	stats := &api.QueueStats{Pending: 0, Processing: 0}
	current := 3 // at min, scale down threshold met
	desired := as.GetDesiredReplicas(cfg, stats, current)

	// threshold: 0 < 2 but current == min => thresholdStrategy returns current
	// Even so, clamped to min=3
	if desired < cfg.Replicas.Min {
		t.Errorf("desired %d is below min %d", desired, cfg.Replicas.Min)
	}
}

func TestClamping_NeverAboveMax(t *testing.T) {
	as, _ := newTestAutoscaler(t)
	cfg := baseCfg()
	cfg.Replicas.Min = 1
	cfg.Replicas.Max = 5
	cfg.ScaleStrategy.Type = "threshold"

	stats := &api.QueueStats{Pending: 100, Processing: 0}
	current := 4
	desired := as.GetDesiredReplicas(cfg, stats, current)

	// threshold: 100 > 10 => 4+2 = 6, clamped to max=5
	if desired != 5 {
		t.Errorf("expected 5 (max), got %d", desired)
	}
}

func TestClamping_TargetPerWorker_MaxClamp(t *testing.T) {
	as, _ := newTestAutoscaler(t)
	cfg := baseCfg()
	cfg.Replicas.Min = 1
	cfg.Replicas.Max = 10
	cfg.ScaleStrategy.Type = "target_per_worker"
	cfg.ScaleStrategy.TargetPerWorker = 2

	stats := &api.QueueStats{Pending: 100, Processing: 0}
	current := 5
	desired := as.GetDesiredReplicas(cfg, stats, current)

	// ceil(100/2) = 50, clamped to max=10
	if desired != 10 {
		t.Errorf("expected 10 (max), got %d", desired)
	}
}

func TestClamping_TargetPerWorker_MinClamp(t *testing.T) {
	as, _ := newTestAutoscaler(t)
	cfg := baseCfg()
	cfg.Replicas.Min = 3
	cfg.Replicas.Max = 10
	cfg.ScaleStrategy.Type = "target_per_worker"
	cfg.ScaleStrategy.TargetPerWorker = 100

	stats := &api.QueueStats{Pending: 1, Processing: 0}
	current := 5
	desired := as.GetDesiredReplicas(cfg, stats, current)

	// ceil(1/100) = 1, clamped to min=3
	if desired != 3 {
		t.Errorf("expected 3 (min), got %d", desired)
	}
}

// ---- Edge Case Tests ----

func TestEdge_VeryLargeQueue(t *testing.T) {
	as, _ := newTestAutoscaler(t)
	cfg := baseCfg()
	cfg.Replicas.Min = 1
	cfg.Replicas.Max = 100
	cfg.ScaleStrategy.Type = "target_per_worker"
	cfg.ScaleStrategy.TargetPerWorker = 10

	stats := &api.QueueStats{Pending: 999999, Processing: 1}
	current := 1
	desired := as.GetDesiredReplicas(cfg, stats, current)

	// ceil(1000000/10) = 100000, clamped to max=100
	if desired != 100 {
		t.Errorf("expected 100 (max), got %d", desired)
	}
}

func TestEdge_ZeroWorkAllStrategies(t *testing.T) {
	as, _ := newTestAutoscaler(t)

	stats := &api.QueueStats{Pending: 0, Processing: 0}
	current := 5

	strategies := []string{"threshold", "rate", "target_per_worker"}
	for _, strat := range strategies {
		t.Run(strat, func(t *testing.T) {
			cfg := baseCfg()
			cfg.ScaleStrategy.Type = strat
			desired := as.GetDesiredReplicas(cfg, stats, current)

			if desired < cfg.Replicas.Min {
				t.Errorf("strategy %s: desired %d below min %d", strat, desired, cfg.Replicas.Min)
			}
			if desired > cfg.Replicas.Max {
				t.Errorf("strategy %s: desired %d above max %d", strat, desired, cfg.Replicas.Max)
			}
		})
	}
}

func TestEdge_DefaultStrategyFallsBackToThreshold(t *testing.T) {
	as, _ := newTestAutoscaler(t)
	cfg := baseCfg()
	cfg.ScaleStrategy.Type = "unknown_strategy"

	stats := &api.QueueStats{Pending: 15, Processing: 0}
	current := 3
	desired := as.GetDesiredReplicas(cfg, stats, current)

	// Should fall through to thresholdStrategy: 15 > 10 => 3+2=5
	if desired != 5 {
		t.Errorf("expected 5 replicas (threshold fallback), got %d", desired)
	}
}

func TestEdge_MinEqualsMax(t *testing.T) {
	as, _ := newTestAutoscaler(t)
	cfg := baseCfg()
	cfg.Replicas.Min = 5
	cfg.Replicas.Max = 5
	cfg.ScaleStrategy.Type = "threshold"

	stats := &api.QueueStats{Pending: 100, Processing: 0}
	current := 5
	desired := as.GetDesiredReplicas(cfg, stats, current)

	// threshold wants 7, clamped to max=5; min also 5
	if desired != 5 {
		t.Errorf("expected 5 (min==max), got %d", desired)
	}
}

// ---- Table-driven tests ----

func TestCalculateDesired_TableDriven(t *testing.T) {
	as, _ := newTestAutoscaler(t)

	tests := []struct {
		name     string
		strategy string
		pending  int64
		proc     int64
		current  int
		min      int
		max      int
		tpw      int
		upThr    int
		downThr  int
		upStep   int
		downStep int
		want     int
	}{
		{
			name: "threshold_scale_up",
			strategy: "threshold", pending: 20, proc: 0, current: 2,
			min: 1, max: 10, upThr: 10, downThr: 2, upStep: 2, downStep: 1,
			want: 4,
		},
		{
			name: "threshold_scale_down",
			strategy: "threshold", pending: 1, proc: 0, current: 5,
			min: 1, max: 10, upThr: 10, downThr: 2, upStep: 2, downStep: 1,
			want: 4,
		},
		{
			name: "threshold_hold_steady",
			strategy: "threshold", pending: 5, proc: 0, current: 3,
			min: 1, max: 10, upThr: 10, downThr: 2, upStep: 2, downStep: 1,
			want: 3,
		},
		{
			name: "target_per_worker_ceil",
			strategy: "target_per_worker", pending: 11, proc: 0, current: 1,
			min: 1, max: 20, tpw: 5,
			want: 3, // ceil(11/5) = 3
		},
		{
			name: "target_per_worker_clamped_to_max",
			strategy: "target_per_worker", pending: 100, proc: 0, current: 1,
			min: 1, max: 5, tpw: 2,
			want: 5,
		},
		{
			name: "rate_scale_up",
			strategy: "rate", pending: 50, proc: 0, current: 5,
			min: 1, max: 20, upThr: 5, downThr: 1, upStep: 3, downStep: 1,
			want: 8, // 50/5=10 > 5 => 5+3=8
		},
		{
			name: "rate_scale_down",
			strategy: "rate", pending: 2, proc: 0, current: 10,
			min: 1, max: 20, upThr: 5, downThr: 1, upStep: 3, downStep: 2,
			want: 8, // 2/10=0.2 < 1 => 10-2=8
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := api.QueueConfig{
				Name: "test",
				Replicas: api.ReplicaConfig{
					Min: tt.min,
					Max: tt.max,
				},
				ScaleStrategy: api.ScaleStrategy{
					Type:               tt.strategy,
					ScaleUpThreshold:   tt.upThr,
					ScaleDownThreshold: tt.downThr,
					TargetPerWorker:    tt.tpw,
					ScaleUpStep:        tt.upStep,
					ScaleDownStep:      tt.downStep,
				},
			}
			stats := &api.QueueStats{
				Pending:    tt.pending,
				Processing: tt.proc,
			}
			got := as.GetDesiredReplicas(cfg, stats, tt.current)
			if got != tt.want {
				t.Errorf("got %d, want %d", got, tt.want)
			}
		})
	}
}

// ---- Cost-Aware Scaling Tests ----

func TestCostAware_ScaleDownRateLimit(t *testing.T) {
	as, _ := newTestAutoscaler(t)
	cfg := baseCfg()
	cfg.ScaleStrategy.Type = "target_per_worker"
	cfg.ScaleStrategy.TargetPerWorker = 5
	cfg.ScaleStrategy.ScaleDownRate = 0.5
	cfg.Replicas.Min = 1
	cfg.Replicas.Max = 20

	// With 0 work and 10 workers, target_per_worker wants min (1).
	// But scale_down_rate=0.5 limits removal to ceil(10*0.5)=5, so min allowed is 10-5=5.
	stats := &api.QueueStats{Pending: 0, Processing: 0}
	current := 10
	desired := as.GetDesiredReplicas(cfg, stats, current)

	if desired != 5 {
		t.Errorf("expected 5 replicas (rate-limited from 10), got %d", desired)
	}
}

func TestCostAware_MinIdleWorkers(t *testing.T) {
	as, _ := newTestAutoscaler(t)
	cfg := baseCfg()
	cfg.ScaleStrategy.Type = "target_per_worker"
	cfg.ScaleStrategy.TargetPerWorker = 5
	cfg.ScaleStrategy.MinIdleWorkers = 2
	cfg.Replicas.Min = 1
	cfg.Replicas.Max = 20

	// With 0 work, target_per_worker wants min (1).
	// But min_idle_workers=2 means floor is min+idle = 1+2 = 3.
	stats := &api.QueueStats{Pending: 0, Processing: 0}
	current := 5
	desired := as.GetDesiredReplicas(cfg, stats, current)

	if desired != 3 {
		t.Errorf("expected 3 replicas (min %d + idle %d), got %d", cfg.Replicas.Min, cfg.ScaleStrategy.MinIdleWorkers, desired)
	}
}

func TestCostAware_MinIdleWorkersCapped(t *testing.T) {
	as, _ := newTestAutoscaler(t)
	cfg := baseCfg()
	cfg.ScaleStrategy.Type = "target_per_worker"
	cfg.ScaleStrategy.TargetPerWorker = 5
	cfg.ScaleStrategy.MinIdleWorkers = 100 // absurdly high
	cfg.Replicas.Min = 1
	cfg.Replicas.Max = 5

	// idle floor = min(1+100, 5) = 5 (capped to max)
	stats := &api.QueueStats{Pending: 0, Processing: 0}
	current := 3
	desired := as.GetDesiredReplicas(cfg, stats, current)

	if desired != 5 {
		t.Errorf("expected 5 replicas (idle floor capped to max), got %d", desired)
	}
}

func TestCostAware_DefaultScaleDownRate(t *testing.T) {
	as, _ := newTestAutoscaler(t)
	cfg := baseCfg()
	cfg.ScaleStrategy.Type = "threshold"
	cfg.ScaleStrategy.ScaleDownRate = 0 // explicitly zero => no rate limiting
	cfg.ScaleStrategy.ScaleDownStep = 1
	cfg.Replicas.Min = 1
	cfg.Replicas.Max = 20

	// With work=0, threshold wants current - step = 5 - 1 = 4
	// ScaleDownRate=0 means no rate limiting, so the step-based behavior applies as before.
	stats := &api.QueueStats{Pending: 0, Processing: 0}
	current := 5
	desired := as.GetDesiredReplicas(cfg, stats, current)

	if desired != 4 {
		t.Errorf("expected 4 replicas (backwards compatible, no rate limit), got %d", desired)
	}
}
