package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
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

var failureRate float64

func main() {
	rateStr := os.Getenv("FAILURE_RATE")
	if rateStr == "" {
		rateStr = "0.1"
	}
	var err error
	failureRate, err = strconv.ParseFloat(rateStr, 64)
	if err != nil {
		log.Fatalf("invalid FAILURE_RATE %q: %v", rateStr, err)
	}
	log.Printf("echo-worker starting with failure_rate=%.2f", failureRate)

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

	log.Printf("processing job_id=%s payload_size=%d", req.JobID, len(req.Payload))

	start := time.Now()

	// Simulate work: sleep 1-3 seconds.
	time.Sleep(time.Duration(1000+rand.Intn(2000)) * time.Millisecond)

	processingTime := time.Since(start)

	// Simulate failures based on configured rate.
	if rand.Float64() < failureRate {
		log.Printf("job_id=%s simulated failure after %v", req.JobID, processingTime)
		respondError(w, "simulated processing failure")
		return
	}

	// Parse payload to check for an "action" field
	var payloadMap map[string]interface{}
	hasAction := false
	action := ""
	if err := json.Unmarshal(req.Payload, &payloadMap); err == nil {
		if a, ok := payloadMap["action"].(string); ok {
			hasAction = true
			action = a
		}
	}

	var result interface{}

	if hasAction {
		// Dispatch based on action
		switch action {
		case "echo":
			result = handleEcho(payloadMap, processingTime)
		case "uppercase":
			result = handleUppercase(payloadMap, processingTime)
		case "count":
			result = handleCount(payloadMap, processingTime)
		case "sort":
			result = handleSort(payloadMap, processingTime)
		case "reverse":
			result = handleReverse(payloadMap, processingTime)
		case "hash":
			result = handleHash(payloadMap, processingTime)
		case "delay":
			result = handleDelay(payloadMap, processingTime)
		default:
			// Unknown action, echo back with a note
			result = map[string]interface{}{
				"action":         action,
				"note":           "unknown action, echoing payload",
				"payload":        payloadMap,
				"processing_ms":  processingTime.Milliseconds(),
				"worker":         hostname(),
				"available_actions": []string{"echo", "uppercase", "count", "sort", "reverse", "hash", "delay"},
			}
		}
	} else {
		// No action field - default echo behavior (works with any JSON type)
		result = map[string]interface{}{
			"echo":           json.RawMessage(req.Payload),
			"processing_ms":  processingTime.Milliseconds(),
			"worker":         hostname(),
			"processed_at":   time.Now().UTC().Format(time.RFC3339),
		}
	}

	log.Printf("job_id=%s completed action=%q in %v", req.JobID, action, processingTime)
	respondSuccess(w, result)
}

// --- Action handlers ---

func handleEcho(payload map[string]interface{}, dur time.Duration) interface{} {
	return map[string]interface{}{
		"action":        "echo",
		"echo":          payload,
		"processing_ms": dur.Milliseconds(),
		"worker":        hostname(),
	}
}

func handleUppercase(payload map[string]interface{}, dur time.Duration) interface{} {
	text, _ := payload["text"].(string)
	return map[string]interface{}{
		"action":        "uppercase",
		"input":         text,
		"output":        strings.ToUpper(text),
		"processing_ms": dur.Milliseconds(),
		"worker":        hostname(),
	}
}

func handleCount(payload map[string]interface{}, dur time.Duration) interface{} {
	text, _ := payload["text"].(string)
	words := strings.Fields(text)
	return map[string]interface{}{
		"action":          "count",
		"input":           text,
		"words":           len(words),
		"characters":      len(text),
		"lines":           len(strings.Split(text, "\n")),
		"processing_ms":   dur.Milliseconds(),
		"worker":          hostname(),
	}
}

func handleSort(payload map[string]interface{}, dur time.Duration) interface{} {
	// Sort items if it's an array of strings, or sort words in text
	if items, ok := payload["items"].([]interface{}); ok {
		strs := make([]string, 0, len(items))
		for _, item := range items {
			strs = append(strs, fmt.Sprint(item))
		}
		sort.Strings(strs)
		return map[string]interface{}{
			"action":        "sort",
			"input":         items,
			"sorted":        strs,
			"count":         len(strs),
			"processing_ms": dur.Milliseconds(),
			"worker":        hostname(),
		}
	}
	text, _ := payload["text"].(string)
	words := strings.Fields(text)
	sort.Strings(words)
	return map[string]interface{}{
		"action":        "sort",
		"input":         text,
		"sorted_words":  words,
		"count":         len(words),
		"processing_ms": dur.Milliseconds(),
		"worker":        hostname(),
	}
}

func handleReverse(payload map[string]interface{}, dur time.Duration) interface{} {
	text, _ := payload["text"].(string)
	runes := []rune(text)
	for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
		runes[i], runes[j] = runes[j], runes[i]
	}
	return map[string]interface{}{
		"action":        "reverse",
		"input":         text,
		"output":        string(runes),
		"processing_ms": dur.Milliseconds(),
		"worker":        hostname(),
	}
}

func handleHash(payload map[string]interface{}, dur time.Duration) interface{} {
	text, _ := payload["text"].(string)
	// Simple hash - not crypto, just for demo
	var h uint64
	for _, c := range text {
		h = h*31 + uint64(c)
	}
	return map[string]interface{}{
		"action":        "hash",
		"input":         text,
		"hash":          fmt.Sprintf("%016x", h),
		"length":        len(text),
		"processing_ms": dur.Milliseconds(),
		"worker":        hostname(),
	}
}

func handleDelay(payload map[string]interface{}, dur time.Duration) interface{} {
	ms, _ := payload["ms"].(float64)
	if ms > 0 && ms < 30000 {
		time.Sleep(time.Duration(ms) * time.Millisecond)
	}
	return map[string]interface{}{
		"action":           "delay",
		"requested_ms":     ms,
		"total_processing": time.Since(time.Now().Add(-dur)).Milliseconds(),
		"worker":           hostname(),
	}
}

// --- Helpers ---

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
