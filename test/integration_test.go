package test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/chris/kqueue/pkg/api"
	"github.com/go-chi/chi/v5"
)

// --- In-memory job store (mirrors cmd/controller/main.go) ---

type jobStore struct {
	mu   sync.RWMutex
	jobs map[string]*api.Job
}

func newJobStore() *jobStore {
	return &jobStore{jobs: make(map[string]*api.Job)}
}

func (s *jobStore) SaveJob(job *api.Job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs[job.ID] = job
}

func (s *jobStore) GetJob(id string) (*api.Job, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	j, ok := s.jobs[id]
	return j, ok
}

// --- Helpers to build the router without requiring NATS ---

func buildTestRouter(t *testing.T, store *jobStore, queueConfigs map[string]api.QueueConfig) http.Handler {
	t.Helper()

	r := chi.NewRouter()

	// Health endpoint
	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	r.Route("/api/v1", func(r chi.Router) {
		// Submit a job (no NATS publish -- just stores in memory)
		r.Post("/jobs", func(w http.ResponseWriter, r *http.Request) {
			var req api.SubmitRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
				return
			}
			if req.Queue == "" {
				http.Error(w, `{"error":"queue is required"}`, http.StatusBadRequest)
				return
			}
			qCfg, ok := queueConfigs[req.Queue]
			if !ok {
				http.Error(w, `{"error":"unknown queue"}`, http.StatusBadRequest)
				return
			}

			job := &api.Job{
				ID:         "test-job-" + time.Now().Format("150405.000"),
				Queue:      req.Queue,
				Payload:    req.Payload,
				Status:     api.JobStatusPending,
				MaxRetries: qCfg.MaxRetries,
				CreatedAt:  time.Now(),
				UpdatedAt:  time.Now(),
				Metadata:   req.Metadata,
			}

			store.SaveJob(job)

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(api.SubmitResponse{
				JobID:  job.ID,
				Queue:  job.Queue,
				Status: job.Status,
			})
		})

		// Get job status
		r.Get("/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
			id := chi.URLParam(r, "id")
			job, ok := store.GetJob(id)
			if !ok {
				http.Error(w, `{"error":"job not found"}`, http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(job)
		})

		// List all queue configs (returns config info since we have no NATS stats)
		r.Get("/queues", func(w http.ResponseWriter, r *http.Request) {
			var allStats []*api.QueueStats
			for name, qCfg := range queueConfigs {
				allStats = append(allStats, &api.QueueStats{
					Name:       name,
					MinWorkers: qCfg.Replicas.Min,
					MaxWorkers: qCfg.Replicas.Max,
				})
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(allStats)
		})
	})

	return r
}

func testQueueConfigs() map[string]api.QueueConfig {
	return map[string]api.QueueConfig{
		"echo": {
			Name:       "echo",
			Subject:    "kqueue.echo",
			MaxRetries: 3,
			Replicas:   api.ReplicaConfig{Min: 1, Max: 10},
		},
		"resize": {
			Name:       "resize",
			Subject:    "kqueue.resize",
			MaxRetries: 2,
			Replicas:   api.ReplicaConfig{Min: 1, Max: 5},
		},
	}
}

// --- Tests ---

func TestHealthEndpoint(t *testing.T) {
	store := newJobStore()
	handler := buildTestRouter(t, store, testQueueConfigs())

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp["status"] != "ok" {
		t.Errorf("expected status ok, got %q", resp["status"])
	}
}

func TestSubmitJob_Success(t *testing.T) {
	store := newJobStore()
	handler := buildTestRouter(t, store, testQueueConfigs())

	body := `{"queue":"echo","payload":{"message":"hello"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var resp api.SubmitResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Queue != "echo" {
		t.Errorf("expected queue echo, got %q", resp.Queue)
	}
	if resp.Status != api.JobStatusPending {
		t.Errorf("expected status pending, got %q", resp.Status)
	}
	if resp.JobID == "" {
		t.Error("expected non-empty job ID")
	}
}

func TestSubmitJob_UnknownQueue(t *testing.T) {
	store := newJobStore()
	handler := buildTestRouter(t, store, testQueueConfigs())

	body := `{"queue":"nonexistent","payload":{}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestSubmitJob_MissingQueue(t *testing.T) {
	store := newJobStore()
	handler := buildTestRouter(t, store, testQueueConfigs())

	body := `{"payload":{"data":1}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestSubmitJob_InvalidBody(t *testing.T) {
	store := newJobStore()
	handler := buildTestRouter(t, store, testQueueConfigs())

	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", bytes.NewBufferString("not json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetJob_SubmitAndRetrieve(t *testing.T) {
	store := newJobStore()
	handler := buildTestRouter(t, store, testQueueConfigs())

	// Submit
	body := `{"queue":"echo","payload":{"value":42}}`
	submitReq := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", bytes.NewBufferString(body))
	submitReq.Header.Set("Content-Type", "application/json")
	submitW := httptest.NewRecorder()
	handler.ServeHTTP(submitW, submitReq)

	if submitW.Code != http.StatusCreated {
		t.Fatalf("submit failed: %d", submitW.Code)
	}

	var submitResp api.SubmitResponse
	json.Unmarshal(submitW.Body.Bytes(), &submitResp)

	// Retrieve
	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/"+submitResp.JobID, nil)
	getW := httptest.NewRecorder()
	handler.ServeHTTP(getW, getReq)

	if getW.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", getW.Code)
	}

	var job api.Job
	if err := json.Unmarshal(getW.Body.Bytes(), &job); err != nil {
		t.Fatalf("failed to decode job: %v", err)
	}
	if job.ID != submitResp.JobID {
		t.Errorf("job ID mismatch: got %q, want %q", job.ID, submitResp.JobID)
	}
	if job.Queue != "echo" {
		t.Errorf("queue mismatch: got %q", job.Queue)
	}
	if job.Status != api.JobStatusPending {
		t.Errorf("status mismatch: got %q", job.Status)
	}
}

func TestGetJob_NotFound(t *testing.T) {
	store := newJobStore()
	handler := buildTestRouter(t, store, testQueueConfigs())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/does-not-exist", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestListQueues(t *testing.T) {
	store := newJobStore()
	configs := testQueueConfigs()
	handler := buildTestRouter(t, store, configs)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/queues", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var stats []*api.QueueStats
	if err := json.Unmarshal(w.Body.Bytes(), &stats); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}

	if len(stats) != len(configs) {
		t.Fatalf("expected %d queues, got %d", len(configs), len(stats))
	}

	// Build a set of returned queue names
	names := make(map[string]bool)
	for _, s := range stats {
		names[s.Name] = true
	}
	for name := range configs {
		if !names[name] {
			t.Errorf("queue %q not found in response", name)
		}
	}
}

func TestSubmitJob_WithMetadata(t *testing.T) {
	store := newJobStore()
	handler := buildTestRouter(t, store, testQueueConfigs())

	body := `{"queue":"echo","payload":{"msg":"test"},"metadata":{"priority":"high","user":"alice"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", w.Code)
	}

	var resp api.SubmitResponse
	json.Unmarshal(w.Body.Bytes(), &resp)

	// Retrieve and check metadata
	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/"+resp.JobID, nil)
	getW := httptest.NewRecorder()
	handler.ServeHTTP(getW, getReq)

	var job api.Job
	json.Unmarshal(getW.Body.Bytes(), &job)

	if job.Metadata["priority"] != "high" {
		t.Errorf("expected metadata priority=high, got %q", job.Metadata["priority"])
	}
	if job.Metadata["user"] != "alice" {
		t.Errorf("expected metadata user=alice, got %q", job.Metadata["user"])
	}
}
