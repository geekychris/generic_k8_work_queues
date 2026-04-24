package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"time"
)

const (
	maxTimeout    = 30 * time.Second
	maxOutputSize = 1 << 20 // 1MB
)

type ProcessRequest struct {
	JobID   string          `json:"job_id"`
	Payload json.RawMessage `json:"payload"`
}

type ProcessResponse struct {
	Success bool            `json:"success"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   string          `json:"error,omitempty"`
}

type SandboxPayload struct {
	Action     string `json:"action"`
	Command    string `json:"command,omitempty"`
	Expression string `json:"expression,omitempty"`
	Language   string `json:"language,omitempty"`
	Script     string `json:"script,omitempty"`
	Timeout    int    `json:"timeout,omitempty"`
}

type ExecResult struct {
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	ExitCode   int    `json:"exit_code"`
	DurationMs int64  `json:"duration_ms"`
}

func main() {
	log.Println("sandbox-worker starting")

	http.HandleFunc("/process", handleProcess)
	http.HandleFunc("/health", handleHealth)

	log.Println("listening on :8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
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
		respondError(w, fmt.Sprintf("invalid request body: %v", err))
		return
	}

	var payload SandboxPayload
	if err := json.Unmarshal(req.Payload, &payload); err != nil {
		respondError(w, fmt.Sprintf("invalid payload: %v", err))
		return
	}

	log.Printf("job_id=%s action=%s", req.JobID, payload.Action)

	var result *ExecResult
	var err error

	switch payload.Action {
	case "exec":
		if payload.Command == "" {
			respondError(w, "exec action requires 'command' field")
			return
		}
		log.Printf("job_id=%s exec command=%q", req.JobID, payload.Command)
		result, err = runCommand(payload.Command, payload.Timeout)

	case "eval":
		if payload.Expression == "" {
			respondError(w, "eval action requires 'expression' field")
			return
		}
		lang := payload.Language
		if lang == "" {
			lang = "sh"
		}
		log.Printf("job_id=%s eval expression=%q language=%s", req.JobID, payload.Expression, lang)
		cmd := fmt.Sprintf("echo $(( %s ))", payload.Expression)
		if lang == "sh" {
			result, err = runCommand(cmd, payload.Timeout)
		} else {
			respondError(w, fmt.Sprintf("unsupported language: %s (only 'sh' supported)", lang))
			return
		}

	case "script":
		if payload.Script == "" {
			respondError(w, "script action requires 'script' field")
			return
		}
		log.Printf("job_id=%s script length=%d", req.JobID, len(payload.Script))
		result, err = runScript(payload.Script, payload.Timeout)

	default:
		respondError(w, fmt.Sprintf("unknown action: %s (supported: exec, eval, script)", payload.Action))
		return
	}

	if err != nil {
		respondError(w, fmt.Sprintf("execution error: %v", err))
		return
	}

	log.Printf("job_id=%s completed exit_code=%d duration_ms=%d", req.JobID, result.ExitCode, result.DurationMs)
	respondSuccess(w, result)
}

func runCommand(command string, timeoutSec int) (*ExecResult, error) {
	timeout := resolveTimeout(timeoutSec)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	return executeCmd(cmd, ctx)
}

func runScript(script string, timeoutSec int) (*ExecResult, error) {
	timeout := resolveTimeout(timeoutSec)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Write script to a temp file
	tmpFile, err := os.CreateTemp("", "sandbox-script-*.sh")
	if err != nil {
		return nil, fmt.Errorf("creating temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(script); err != nil {
		tmpFile.Close()
		return nil, fmt.Errorf("writing script: %w", err)
	}
	tmpFile.Close()

	if err := os.Chmod(tmpFile.Name(), 0700); err != nil {
		return nil, fmt.Errorf("chmod script: %w", err)
	}

	cmd := exec.CommandContext(ctx, "sh", tmpFile.Name())
	return executeCmd(cmd, ctx)
}

func executeCmd(cmd *exec.Cmd, ctx context.Context) (*ExecResult, error) {
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &limitedWriter{w: &stdout, limit: maxOutputSize}
	cmd.Stderr = &limitedWriter{w: &stderr, limit: maxOutputSize}

	start := time.Now()
	err := cmd.Run()
	duration := time.Since(start)

	result := &ExecResult{
		Stdout:     stdout.String(),
		Stderr:     stderr.String(),
		ExitCode:   0,
		DurationMs: duration.Milliseconds(),
	}

	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			result.ExitCode = 124 // standard timeout exit code
			result.Stderr = result.Stderr + "\n[TIMEOUT] command exceeded time limit"
		} else if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		} else {
			return nil, err
		}
	}

	return result, nil
}

func resolveTimeout(requested int) time.Duration {
	if requested <= 0 {
		return maxTimeout
	}
	t := time.Duration(requested) * time.Second
	if t > maxTimeout {
		return maxTimeout
	}
	return t
}

// limitedWriter wraps a writer and stops writing after a limit is reached.
type limitedWriter struct {
	w       *bytes.Buffer
	limit   int
	written int
}

func (lw *limitedWriter) Write(p []byte) (int, error) {
	remaining := lw.limit - lw.written
	if remaining <= 0 {
		return len(p), nil // discard but report success
	}
	if len(p) > remaining {
		p = p[:remaining]
	}
	n, err := lw.w.Write(p)
	lw.written += n
	return n, err
}

func hostname() string {
	h, _ := os.Hostname()
	return h
}

func respondSuccess(w http.ResponseWriter, result interface{}) {
	data, _ := json.Marshal(result)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ProcessResponse{
		Success: true,
		Result:  data,
	})
}

func respondError(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ProcessResponse{
		Success: false,
		Error:   msg,
	})
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok"}`))
}
