//go:build e2e

package test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/chris/kqueue/pkg/api"
)

var baseURL string

func TestMain(m *testing.M) {
	baseURL = os.Getenv("KQUEUE_BASE_URL")
	if baseURL == "" {
		baseURL = "http://localhost:8080"
	}

	// Check if the service is running
	resp, err := http.Get(baseURL + "/health")
	if err != nil {
		fmt.Fprintf(os.Stderr, "SKIP: KQueue service not reachable at %s: %v\n", baseURL, err)
		os.Exit(0)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "SKIP: KQueue service health check failed: status %d\n", resp.StatusCode)
		os.Exit(0)
	}

	os.Exit(m.Run())
}

func submitJob(t *testing.T, queue string, payload interface{}) string {
	t.Helper()

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("failed to marshal payload: %v", err)
	}

	reqBody := api.SubmitRequest{
		Queue:   queue,
		Payload: payloadBytes,
	}
	body, _ := json.Marshal(reqBody)

	resp, err := http.Post(baseURL+"/api/v1/jobs", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("failed to submit job: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("submit returned %d: %s", resp.StatusCode, string(respBody))
	}

	var submitResp api.SubmitResponse
	if err := json.NewDecoder(resp.Body).Decode(&submitResp); err != nil {
		t.Fatalf("failed to decode submit response: %v", err)
	}

	return submitResp.JobID
}

func getJob(t *testing.T, jobID string) *api.Job {
	t.Helper()

	resp, err := http.Get(baseURL + "/api/v1/jobs/" + jobID)
	if err != nil {
		t.Fatalf("failed to get job %s: %v", jobID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("get job returned %d: %s", resp.StatusCode, string(respBody))
	}

	var job api.Job
	if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
		t.Fatalf("failed to decode job: %v", err)
	}
	return &job
}

func waitForCompletion(t *testing.T, jobID string, timeout time.Duration) *api.Job {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		job := getJob(t, jobID)
		switch job.Status {
		case api.JobStatusCompleted, api.JobStatusFailed, api.JobStatusDeadLetter:
			return job
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("job %s did not complete within %v", jobID, timeout)
	return nil
}

func TestE2E_HealthCheck(t *testing.T) {
	resp, err := http.Get(baseURL + "/health")
	if err != nil {
		t.Fatalf("health check failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var result map[string]string
	json.NewDecoder(resp.Body).Decode(&result)
	if result["status"] != "ok" {
		t.Errorf("expected status ok, got %q", result["status"])
	}
}

func TestE2E_SubmitAndComplete(t *testing.T) {
	jobID := submitJob(t, "echo", map[string]string{"message": "hello e2e"})

	job := waitForCompletion(t, jobID, 30*time.Second)

	if job.Status != api.JobStatusCompleted {
		t.Errorf("expected completed, got %s (error: %s)", job.Status, job.Error)
	}
}

func TestE2E_MultipleJobs(t *testing.T) {
	const numJobs = 5
	jobIDs := make([]string, numJobs)

	for i := 0; i < numJobs; i++ {
		jobIDs[i] = submitJob(t, "echo", map[string]interface{}{
			"message": fmt.Sprintf("job-%d", i),
			"index":   i,
		})
	}

	// Poll all jobs for completion
	for i, id := range jobIDs {
		job := waitForCompletion(t, id, 60*time.Second)
		if job.Status != api.JobStatusCompleted {
			t.Errorf("job %d (%s): expected completed, got %s (error: %s)", i, id, job.Status, job.Error)
		}
	}
}

func TestE2E_ListQueues(t *testing.T) {
	resp, err := http.Get(baseURL + "/api/v1/queues")
	if err != nil {
		t.Fatalf("failed to list queues: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var stats []*api.QueueStats
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}

	if len(stats) == 0 {
		t.Error("expected at least one queue, got none")
	}

	for _, s := range stats {
		t.Logf("queue %s: pending=%d processing=%d completed=%d workers=%d",
			s.Name, s.Pending, s.Processing, s.Completed, s.Workers)
	}
}

func TestE2E_JobNotFound(t *testing.T) {
	resp, err := http.Get(baseURL + "/api/v1/jobs/nonexistent-id-12345")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}
