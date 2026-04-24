// Package main implements a GitHub webhook receiver that queues code review jobs.
//
// It receives GitHub webhook events (pull_request, push) and submits them
// to the KQueue controller as code review jobs.
//
// Configuration via environment variables:
//
//	KQUEUE_URL      - Controller API URL (default: http://controller:8080)
//	WEBHOOK_PORT    - Port to listen on (default: 9000)
//	WEBHOOK_SECRET  - GitHub webhook secret for HMAC validation (optional)
//	QUEUE_NAME      - Queue to submit jobs to (default: codereview)
package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

// ReviewPayload matches what the codereview worker expects.
type ReviewPayload struct {
	Action       string       `json:"action"`
	RepoOwner    string       `json:"repo_owner"`
	RepoName     string       `json:"repo_name"`
	CloneURL     string       `json:"clone_url"`
	Ref          string       `json:"ref"`
	PRNumber     int          `json:"pr_number,omitempty"`
	PRTitle      string       `json:"pr_title,omitempty"`
	PRBody       string       `json:"pr_body,omitempty"`
	Sender       string       `json:"sender"`
	CommitSHA    string       `json:"commit_sha,omitempty"`
	DiffURL      string       `json:"diff_url,omitempty"`
	FilesChanged []FileChange `json:"files_changed,omitempty"`
}

type FileChange struct {
	Filename string `json:"filename"`
	Status   string `json:"status"`
	Patch    string `json:"patch,omitempty"`
}

// SubmitRequest matches the KQueue API.
type SubmitRequest struct {
	Queue   string      `json:"queue"`
	Payload interface{} `json:"payload"`
}

var (
	kqueueURL     string
	webhookSecret string
	queueName     string
	logger        *slog.Logger
)

func main() {
	logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	kqueueURL = envOr("KQUEUE_URL", "http://controller:8080")
	webhookSecret = os.Getenv("WEBHOOK_SECRET")
	queueName = envOr("QUEUE_NAME", "codereview")
	port := envOr("WEBHOOK_PORT", "9000")

	mux := http.NewServeMux()
	mux.HandleFunc("/webhook/github", handleGitHubWebhook)
	mux.HandleFunc("/webhook/test", handleTestWebhook)
	mux.HandleFunc("/health", handleHealth)

	logger.Info("webhook service starting",
		"port", port,
		"kqueue_url", kqueueURL,
		"queue", queueName,
		"secret_configured", webhookSecret != "",
	)

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: mux,
	}
	if err := srv.ListenAndServe(); err != nil {
		logger.Error("server error", "error", err)
		os.Exit(1)
	}
}

// handleGitHubWebhook processes real GitHub webhook events.
func handleGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	// Validate HMAC signature if secret is configured
	if webhookSecret != "" {
		sig := r.Header.Get("X-Hub-Signature-256")
		if !validateSignature(body, sig, webhookSecret) {
			logger.Warn("invalid webhook signature")
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
	}

	eventType := r.Header.Get("X-GitHub-Event")
	logger.Info("received github webhook", "event", eventType)

	var payload ReviewPayload

	switch eventType {
	case "pull_request":
		payload, err = parsePullRequestEvent(body)
	case "push":
		payload, err = parsePushEvent(body)
	case "ping":
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "pong"})
		return
	default:
		logger.Info("ignoring event type", "event", eventType)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ignored", "event": eventType})
		return
	}

	if err != nil {
		logger.Error("failed to parse event", "event", eventType, "error", err)
		http.Error(w, fmt.Sprintf("parse error: %v", err), http.StatusBadRequest)
		return
	}

	jobID, err := submitToKQueue(payload)
	if err != nil {
		logger.Error("failed to submit job", "error", err)
		http.Error(w, fmt.Sprintf("submit error: %v", err), http.StatusBadGateway)
		return
	}

	logger.Info("job submitted",
		"job_id", jobID,
		"action", payload.Action,
		"repo", payload.RepoOwner+"/"+payload.RepoName,
		"pr", payload.PRNumber,
	)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "queued",
		"job_id": jobID,
		"queue":  queueName,
	})
}

// handleTestWebhook allows manually triggering a review without a real GitHub webhook.
// POST /webhook/test with a JSON body:
//
//	{
//	  "repo_owner": "octocat",
//	  "repo_name": "hello-world",
//	  "pr_number": 42,
//	  "pr_title": "Add feature X",
//	  "pr_body": "This PR adds feature X to the project.",
//	  "sender": "developer",
//	  "files_changed": [
//	    {"filename": "main.go", "status": "modified", "patch": "...diff..."}
//	  ]
//	}
func handleTestWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var payload ReviewPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON: %v", err), http.StatusBadRequest)
		return
	}

	// Fill defaults
	if payload.Action == "" {
		payload.Action = "review_pr"
	}
	if payload.CloneURL == "" && payload.RepoOwner != "" && payload.RepoName != "" {
		payload.CloneURL = fmt.Sprintf("https://github.com/%s/%s.git", payload.RepoOwner, payload.RepoName)
	}

	logger.Info("received test webhook",
		"repo", payload.RepoOwner+"/"+payload.RepoName,
		"pr", payload.PRNumber,
	)

	jobID, err := submitToKQueue(payload)
	if err != nil {
		logger.Error("failed to submit job", "error", err)
		http.Error(w, fmt.Sprintf("submit error: %v", err), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "queued",
		"job_id": jobID,
		"queue":  queueName,
	})
}

func submitToKQueue(payload ReviewPayload) (string, error) {
	submitReq := SubmitRequest{
		Queue:   queueName,
		Payload: payload,
	}

	data, err := json.Marshal(submitReq)
	if err != nil {
		return "", fmt.Errorf("marshal: %w", err)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(kqueueURL+"/api/v1/jobs", "application/json", bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("post to kqueue: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("kqueue returned %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}

	return result.JobID, nil
}

// --- GitHub event parsers ---

func parsePullRequestEvent(body []byte) (ReviewPayload, error) {
	var event struct {
		Action string `json:"action"`
		Number int    `json:"number"`
		PR     struct {
			Title   string `json:"title"`
			Body    string `json:"body"`
			DiffURL string `json:"diff_url"`
			Head    struct {
				SHA string `json:"sha"`
				Ref string `json:"ref"`
			} `json:"head"`
		} `json:"pull_request"`
		Repository struct {
			Owner struct {
				Login string `json:"login"`
			} `json:"owner"`
			Name     string `json:"name"`
			CloneURL string `json:"clone_url"`
		} `json:"repository"`
		Sender struct {
			Login string `json:"login"`
		} `json:"sender"`
	}

	if err := json.Unmarshal(body, &event); err != nil {
		return ReviewPayload{}, fmt.Errorf("unmarshal PR event: %w", err)
	}

	// Only review on opened/synchronize (new commits pushed)
	if event.Action != "opened" && event.Action != "synchronize" {
		return ReviewPayload{
			Action:    "review_pr",
			RepoOwner: event.Repository.Owner.Login,
			RepoName:  event.Repository.Name,
		}, nil
	}

	return ReviewPayload{
		Action:    "review_pr",
		RepoOwner: event.Repository.Owner.Login,
		RepoName:  event.Repository.Name,
		CloneURL:  event.Repository.CloneURL,
		Ref:       event.PR.Head.Ref,
		PRNumber:  event.Number,
		PRTitle:   event.PR.Title,
		PRBody:    event.PR.Body,
		Sender:    event.Sender.Login,
		CommitSHA: event.PR.Head.SHA,
		DiffURL:   event.PR.DiffURL,
	}, nil
}

func parsePushEvent(body []byte) (ReviewPayload, error) {
	var event struct {
		Ref     string `json:"ref"`
		After   string `json:"after"`
		Commits []struct {
			ID       string   `json:"id"`
			Message  string   `json:"message"`
			Added    []string `json:"added"`
			Modified []string `json:"modified"`
			Removed  []string `json:"removed"`
		} `json:"commits"`
		Repository struct {
			Owner struct {
				Login string `json:"login"`
				Name  string `json:"name"`
			} `json:"owner"`
			Name     string `json:"name"`
			CloneURL string `json:"clone_url"`
		} `json:"repository"`
		Sender struct {
			Login string `json:"login"`
		} `json:"sender"`
	}

	if err := json.Unmarshal(body, &event); err != nil {
		return ReviewPayload{}, fmt.Errorf("unmarshal push event: %w", err)
	}

	// Build file list from commits
	var files []FileChange
	seen := make(map[string]bool)
	for _, c := range event.Commits {
		for _, f := range c.Added {
			if !seen[f] {
				files = append(files, FileChange{Filename: f, Status: "added"})
				seen[f] = true
			}
		}
		for _, f := range c.Modified {
			if !seen[f] {
				files = append(files, FileChange{Filename: f, Status: "modified"})
				seen[f] = true
			}
		}
		for _, f := range c.Removed {
			if !seen[f] {
				files = append(files, FileChange{Filename: f, Status: "removed"})
				seen[f] = true
			}
		}
	}

	// Extract branch from ref (refs/heads/main -> main)
	ref := event.Ref
	if strings.HasPrefix(ref, "refs/heads/") {
		ref = strings.TrimPrefix(ref, "refs/heads/")
	}

	owner := event.Repository.Owner.Login
	if owner == "" {
		owner = event.Repository.Owner.Name
	}

	return ReviewPayload{
		Action:       "review_push",
		RepoOwner:    owner,
		RepoName:     event.Repository.Name,
		CloneURL:     event.Repository.CloneURL,
		Ref:          ref,
		Sender:       event.Sender.Login,
		CommitSHA:    event.After,
		FilesChanged: files,
	}, nil
}

// --- Helpers ---

func validateSignature(body []byte, signature, secret string) bool {
	if !strings.HasPrefix(signature, "sha256=") {
		return false
	}
	sig, err := hex.DecodeString(strings.TrimPrefix(signature, "sha256="))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := mac.Sum(nil)
	return hmac.Equal(sig, expected)
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
