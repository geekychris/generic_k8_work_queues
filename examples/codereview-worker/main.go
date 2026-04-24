package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// ProcessRequest is what the sidecar sends us.
type ProcessRequest struct {
	JobID   string          `json:"job_id"`
	Payload json.RawMessage `json:"payload"`
}

// ProcessResponse is what we return.
type ProcessResponse struct {
	Success bool        `json:"success"`
	Result  interface{} `json:"result,omitempty"`
	Error   string      `json:"error,omitempty"`
	Logs    []LogEntry  `json:"logs,omitempty"`
}

type LogEntry struct {
	Level   string `json:"level"`
	Message string `json:"message"`
}

// ReviewPayload is the expected payload from the webhook service.
type ReviewPayload struct {
	Action       string       `json:"action"` // "review_pr", "review_push"
	RepoOwner    string       `json:"repo_owner"`
	RepoName     string       `json:"repo_name"`
	CloneURL     string       `json:"clone_url"`
	Ref          string       `json:"ref"` // branch or sha
	PRNumber     int          `json:"pr_number,omitempty"`
	PRTitle      string       `json:"pr_title,omitempty"`
	PRBody       string       `json:"pr_body,omitempty"`
	Sender       string       `json:"sender"`
	CommitSHA    string       `json:"commit_sha,omitempty"`
	DiffURL      string       `json:"diff_url,omitempty"`
	FilesChanged []FileChange `json:"files_changed,omitempty"`
}

// FileChange represents a changed file in a PR/push.
type FileChange struct {
	Filename string `json:"filename"`
	Status   string `json:"status"` // added, modified, removed
	Patch    string `json:"patch,omitempty"`
}

// OllamaRequest is the request to the Ollama chat API.
type OllamaRequest struct {
	Model    string          `json:"model"`
	Messages []OllamaMessage `json:"messages"`
	Stream   bool            `json:"stream"`
}

// OllamaMessage is a chat message for Ollama.
type OllamaMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// OllamaResponse is the response from Ollama.
type OllamaResponse struct {
	Model         string        `json:"model"`
	Message       OllamaMessage `json:"message"`
	Done          bool          `json:"done"`
	TotalDuration int64         `json:"total_duration"`
}

var (
	ollamaURL   string
	ollamaModel string
)

func main() {
	ollamaURL = envOr("OLLAMA_URL", "http://host.docker.internal:11434")
	ollamaModel = envOr("OLLAMA_MODEL", "llama3.2")
	listenPort := envOr("PORT", "8080")

	log.Printf("codereview-worker starting, ollama=%s model=%s port=%s", ollamaURL, ollamaModel, listenPort)

	http.HandleFunc("/process", handleProcess)
	http.HandleFunc("/health", handleHealth)

	log.Printf("listening on :%s", listenPort)
	if err := http.ListenAndServe(":"+listenPort, nil); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func handleProcess(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req ProcessRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond(w, ProcessResponse{Error: fmt.Sprintf("invalid request: %v", err)})
		return
	}

	var payload ReviewPayload
	if err := json.Unmarshal(req.Payload, &payload); err != nil {
		respond(w, ProcessResponse{Error: fmt.Sprintf("invalid payload: %v", err)})
		return
	}

	var logs []LogEntry

	logf := func(level, format string, args ...interface{}) {
		msg := fmt.Sprintf(format, args...)
		log.Printf("job_id=%s [%s] %s", req.JobID, level, msg)
		logs = append(logs, LogEntry{Level: level, Message: msg})
	}

	logf("info", "Starting review: %s repo=%s/%s PR=#%d by %s",
		payload.Action, payload.RepoOwner, payload.RepoName, payload.PRNumber, payload.Sender)

	if len(payload.FilesChanged) > 0 {
		logf("info", "Files changed (%d):", len(payload.FilesChanged))
		for _, f := range payload.FilesChanged {
			logf("info", "  %s [%s] (%d bytes patch)", f.Filename, f.Status, len(f.Patch))
		}
	}

	// TODO: Clone the repo for full file access
	if payload.CloneURL != "" {
		logf("info", "Clone placeholder: would clone %s ref=%s", payload.CloneURL, payload.Ref)
	}
	_ = cloneRepoPlaceholder(payload)

	// Build a prompt from the PR/push info
	prompt := buildReviewPrompt(payload)
	logf("info", "Built review prompt (%d chars) for model %s", len(prompt), ollamaModel)
	logf("info", "Calling Ollama at %s ...", ollamaURL)

	// Call Ollama
	start := time.Now()
	review, err := callOllama(prompt)
	duration := time.Since(start)

	if err != nil {
		logf("error", "Ollama call failed after %v: %v", duration, err)
		respond(w, ProcessResponse{
			Error: fmt.Sprintf("ollama call failed: %v", err),
			Logs:  logs,
		})
		return
	}

	logf("info", "Ollama responded in %v (%d chars)", duration, len(review))
	logf("info", "--- LLM Review Output ---")
	// Split the review into lines so each line is a separate log entry for readability
	for _, line := range strings.Split(review, "\n") {
		if line != "" {
			logf("info", "%s", line)
		}
	}
	logf("info", "--- End Review Output ---")

	result := map[string]interface{}{
		"action":         payload.Action,
		"repo":           payload.RepoOwner + "/" + payload.RepoName,
		"pr_number":      payload.PRNumber,
		"pr_title":       payload.PRTitle,
		"review":         review,
		"model":          ollamaModel,
		"duration_ms":    duration.Milliseconds(),
		"files_reviewed": len(payload.FilesChanged),
		"worker":         hostname(),
	}

	respond(w, ProcessResponse{Success: true, Result: result, Logs: logs})
}

// cloneRepoPlaceholder is where repo cloning will be implemented.
// For now it just logs intent.
func cloneRepoPlaceholder(payload ReviewPayload) error {
	log.Printf("TODO: clone %s ref=%s", payload.CloneURL, payload.Ref)
	// Future implementation:
	//
	// func cloneRepo(cloneURL, ref string) (string, error) {
	//     workDir, err := os.MkdirTemp("", "codereview-*")
	//     if err != nil {
	//         return "", err
	//     }
	//     cmd := exec.Command("git", "clone", "--depth=1", "--branch", ref, cloneURL, workDir)
	//     cmd.Stdout = os.Stdout
	//     cmd.Stderr = os.Stderr
	//     if err := cmd.Run(); err != nil {
	//         os.RemoveAll(workDir)
	//         return "", fmt.Errorf("git clone failed: %w", err)
	//     }
	//     return workDir, nil
	// }
	return nil
}

func buildReviewPrompt(p ReviewPayload) string {
	var sb strings.Builder

	sb.WriteString("You are a code reviewer. Provide a brief, constructive review of the following code changes.\n\n")

	if p.PRTitle != "" {
		sb.WriteString(fmt.Sprintf("## Pull Request #%d: %s\n\n", p.PRNumber, p.PRTitle))
	}
	if p.PRBody != "" {
		sb.WriteString(fmt.Sprintf("### Description\n%s\n\n", p.PRBody))
	}

	sb.WriteString(fmt.Sprintf("Repository: %s/%s\n", p.RepoOwner, p.RepoName))
	sb.WriteString(fmt.Sprintf("Author: %s\n", p.Sender))
	if p.Ref != "" {
		sb.WriteString(fmt.Sprintf("Branch/Ref: %s\n", p.Ref))
	}

	if len(p.FilesChanged) > 0 {
		sb.WriteString(fmt.Sprintf("\n### Files Changed (%d)\n\n", len(p.FilesChanged)))
		for _, f := range p.FilesChanged {
			sb.WriteString(fmt.Sprintf("- **%s** (%s)\n", f.Filename, f.Status))
			if f.Patch != "" {
				// Limit patch size to avoid overwhelming the model
				patch := f.Patch
				if len(patch) > 3000 {
					patch = patch[:3000] + "\n... (truncated)"
				}
				sb.WriteString(fmt.Sprintf("```diff\n%s\n```\n\n", patch))
			}
		}
	} else {
		sb.WriteString("\n(No diff data available -- would need to clone the repo for full review)\n")
	}

	sb.WriteString("\nPlease provide:\n")
	sb.WriteString("1. A brief summary of what the changes do\n")
	sb.WriteString("2. Any potential issues or bugs\n")
	sb.WriteString("3. Suggestions for improvement\n")
	sb.WriteString("4. An overall assessment (approve, request changes, or comment)\n")

	return sb.String()
}

func callOllama(prompt string) (string, error) {
	reqBody := OllamaRequest{
		Model: ollamaModel,
		Messages: []OllamaMessage{
			{Role: "user", Content: prompt},
		},
		Stream: false,
	}

	data, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Post(ollamaURL+"/api/chat", "application/json", bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("ollama request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ollama returned %d: %s", resp.StatusCode, string(body))
	}

	var ollamaResp OllamaResponse
	if err := json.Unmarshal(body, &ollamaResp); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}

	return ollamaResp.Message.Content, nil
}

func respond(w http.ResponseWriter, resp ProcessResponse) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok"}`))
}

func hostname() string {
	h, _ := os.Hostname()
	return h
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
