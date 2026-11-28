package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	os.Exit(m.Run())
}

// withStubKQueue points kqueueURL at a stub upstream for the duration of fn and
// restores the previous global state afterward. The globals are the same ones
// main() sets from environment variables, so tests exercise the real handlers.
func withStubKQueue(t *testing.T, handler http.HandlerFunc, secret string, fn func(stub *httptest.Server, calls *int)) {
	t.Helper()

	calls := 0
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		handler(w, r)
	})
	stub := httptest.NewServer(wrapped)
	defer stub.Close()

	prevURL, prevSecret, prevQueue := kqueueURL, webhookSecret, queueName
	kqueueURL = stub.URL
	webhookSecret = secret
	queueName = "codereview"
	defer func() {
		kqueueURL, webhookSecret, queueName = prevURL, prevSecret, prevQueue
	}()

	fn(stub, &calls)
}

func acceptJobsHandler(jobID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"job_id": jobID})
	}
}

func TestHandleHealth(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()

	handleHealth(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("status field = %q, want %q", body["status"], "ok")
	}
}

func TestHandleTestWebhook(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		body       string
		upstream   http.HandlerFunc
		wantStatus int
		wantCalled bool
	}{
		{
			name:       "valid body queues job",
			method:     http.MethodPost,
			body:       `{"repo_owner":"octocat","repo_name":"hello-world","pr_number":42}`,
			upstream:   acceptJobsHandler("job-123"),
			wantStatus: http.StatusOK,
			wantCalled: true,
		},
		{
			name:       "malformed JSON",
			method:     http.MethodPost,
			body:       `{not valid json`,
			upstream:   acceptJobsHandler("job-123"),
			wantStatus: http.StatusBadRequest,
			wantCalled: false,
		},
		{
			name:       "wrong method",
			method:     http.MethodGet,
			body:       "",
			upstream:   acceptJobsHandler("job-123"),
			wantStatus: http.StatusMethodNotAllowed,
			wantCalled: false,
		},
		{
			name:   "upstream rejects",
			method: http.MethodPost,
			body:   `{"repo_owner":"octocat","repo_name":"hello-world"}`,
			upstream: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			wantStatus: http.StatusBadGateway,
			wantCalled: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			withStubKQueue(t, tc.upstream, "", func(stub *httptest.Server, calls *int) {
				req := httptest.NewRequest(tc.method, "/webhook/test", bytes.NewBufferString(tc.body))
				rec := httptest.NewRecorder()

				handleTestWebhook(rec, req)

				if rec.Code != tc.wantStatus {
					t.Fatalf("status = %d, want %d (body: %s)", rec.Code, tc.wantStatus, rec.Body.String())
				}
				if (*calls > 0) != tc.wantCalled {
					t.Fatalf("upstream called = %v, want %v", *calls > 0, tc.wantCalled)
				}
				if tc.wantStatus == http.StatusOK {
					var resp map[string]interface{}
					if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
						t.Fatalf("decode response: %v", err)
					}
					if resp["status"] != "queued" {
						t.Fatalf("status field = %v, want %q", resp["status"], "queued")
					}
					if resp["job_id"] != "job-123" {
						t.Fatalf("job_id = %v, want %q", resp["job_id"], "job-123")
					}
					if resp["queue"] != "codereview" {
						t.Fatalf("queue = %v, want %q", resp["queue"], "codereview")
					}
				}
			})
		})
	}
}

func TestHandleGitHubWebhook_Ping(t *testing.T) {
	withStubKQueue(t, acceptJobsHandler("job-123"), "", func(stub *httptest.Server, calls *int) {
		req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewBufferString(`{}`))
		req.Header.Set("X-GitHub-Event", "ping")
		rec := httptest.NewRecorder()

		handleGitHubWebhook(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		var body map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["status"] != "pong" {
			t.Fatalf("status field = %q, want %q", body["status"], "pong")
		}
		if *calls != 0 {
			t.Fatalf("upstream calls = %d, want 0", *calls)
		}
	})
}

func TestHandleGitHubWebhook_IgnoredEvent(t *testing.T) {
	withStubKQueue(t, acceptJobsHandler("job-123"), "", func(stub *httptest.Server, calls *int) {
		req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewBufferString(`{}`))
		req.Header.Set("X-GitHub-Event", "issues")
		rec := httptest.NewRecorder()

		handleGitHubWebhook(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		var body map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["status"] != "ignored" || body["event"] != "issues" {
			t.Fatalf("body = %v, want status=ignored event=issues", body)
		}
		if *calls != 0 {
			t.Fatalf("upstream calls = %d, want 0", *calls)
		}
	})
}

func TestHandleGitHubWebhook_PullRequest(t *testing.T) {
	prEventJSON := func(action string) string {
		return `{
			"action": "` + action + `",
			"number": 42,
			"pull_request": {
				"title": "Add feature X",
				"body": "Adds feature X",
				"diff_url": "https://github.com/octocat/hello-world/pull/42.diff",
				"head": {"sha": "abc123", "ref": "feature-x"}
			},
			"repository": {
				"owner": {"login": "octocat"},
				"name": "hello-world",
				"clone_url": "https://github.com/octocat/hello-world.git"
			},
			"sender": {"login": "developer"}
		}`
	}

	tests := []struct {
		name         string
		action       string
		wantRef      string
		wantPRNumber int
	}{
		{name: "opened", action: "opened", wantRef: "feature-x", wantPRNumber: 42},
		{name: "synchronize", action: "synchronize", wantRef: "feature-x", wantPRNumber: 42},
		{name: "closed", action: "closed", wantRef: "", wantPRNumber: 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var captured struct {
				Payload SubmitRequest
			}
			upstream := func(w http.ResponseWriter, r *http.Request) {
				body, _ := jsonDecodeSubmitRequest(r)
				captured.Payload = body
				w.WriteHeader(http.StatusCreated)
				json.NewEncoder(w).Encode(map[string]string{"job_id": "job-123"})
			}

			withStubKQueue(t, upstream, "", func(stub *httptest.Server, calls *int) {
				req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewBufferString(prEventJSON(tc.action)))
				req.Header.Set("X-GitHub-Event", "pull_request")
				rec := httptest.NewRecorder()

				handleGitHubWebhook(rec, req)

				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
				}
				if *calls != 1 {
					t.Fatalf("upstream calls = %d, want 1", *calls)
				}

				payloadBytes, err := json.Marshal(captured.Payload.Payload)
				if err != nil {
					t.Fatalf("marshal captured payload: %v", err)
				}
				var payload ReviewPayload
				if err := json.Unmarshal(payloadBytes, &payload); err != nil {
					t.Fatalf("unmarshal captured payload: %v", err)
				}

				if payload.RepoOwner != "octocat" || payload.RepoName != "hello-world" {
					t.Fatalf("repo = %s/%s, want octocat/hello-world", payload.RepoOwner, payload.RepoName)
				}
				if payload.Ref != tc.wantRef {
					t.Fatalf("ref = %q, want %q", payload.Ref, tc.wantRef)
				}
				if payload.PRNumber != tc.wantPRNumber {
					t.Fatalf("pr_number = %d, want %d", payload.PRNumber, tc.wantPRNumber)
				}
			})
		})
	}
}

func TestHandleGitHubWebhook_Push(t *testing.T) {
	pushEventJSON := `{
		"ref": "refs/heads/main",
		"after": "deadbeef",
		"commits": [
			{"id": "c1", "message": "first", "added": ["a.go"], "modified": ["b.go"], "removed": []},
			{"id": "c2", "message": "second", "added": ["a.go"], "modified": [], "removed": ["c.go"]}
		],
		"repository": {
			"owner": {"login": "octocat"},
			"name": "hello-world",
			"clone_url": "https://github.com/octocat/hello-world.git"
		},
		"sender": {"login": "developer"}
	}`

	var captured SubmitRequest
	upstream := func(w http.ResponseWriter, r *http.Request) {
		body, _ := jsonDecodeSubmitRequest(r)
		captured = body
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"job_id": "job-123"})
	}

	withStubKQueue(t, upstream, "", func(stub *httptest.Server, calls *int) {
		req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewBufferString(pushEventJSON))
		req.Header.Set("X-GitHub-Event", "push")
		rec := httptest.NewRecorder()

		handleGitHubWebhook(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
		}
		if *calls != 1 {
			t.Fatalf("upstream calls = %d, want 1", *calls)
		}

		payloadBytes, err := json.Marshal(captured.Payload)
		if err != nil {
			t.Fatalf("marshal captured payload: %v", err)
		}
		var payload ReviewPayload
		if err := json.Unmarshal(payloadBytes, &payload); err != nil {
			t.Fatalf("unmarshal captured payload: %v", err)
		}

		if payload.Ref != "main" {
			t.Fatalf("ref = %q, want %q (refs/heads/ prefix should be stripped)", payload.Ref, "main")
		}
		if len(payload.FilesChanged) != 3 {
			t.Fatalf("files_changed = %v (len %d), want len 3 (deduplicated a.go, b.go, c.go)", payload.FilesChanged, len(payload.FilesChanged))
		}
		statuses := map[string]string{}
		for _, f := range payload.FilesChanged {
			statuses[f.Filename] = f.Status
		}
		if statuses["a.go"] != "added" || statuses["b.go"] != "modified" || statuses["c.go"] != "removed" {
			t.Fatalf("statuses = %v, want a.go=added b.go=modified c.go=removed", statuses)
		}
	})
}

func TestHandleGitHubWebhook_SignatureValidation(t *testing.T) {
	secret := "test-secret"
	body := []byte(`{
		"action": "closed",
		"number": 1,
		"pull_request": {"head": {"sha": "abc", "ref": "main"}},
		"repository": {"owner": {"login": "octocat"}, "name": "hello-world"},
		"sender": {"login": "developer"}
	}`)

	sign := func(s string) string {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(s))
		return "sha256=" + hex.EncodeToString(mac.Sum(nil))
	}

	tests := []struct {
		name        string
		secret      string
		sigHeader   string
		wantStatus  int
		wantUpcalls bool
	}{
		{
			name:        "correct signature",
			secret:      secret,
			sigHeader:   sign(string(body)),
			wantStatus:  http.StatusOK,
			wantUpcalls: true,
		},
		{
			name:        "wrong signature",
			secret:      secret,
			sigHeader:   "sha256=0000000000000000000000000000000000000000000000000000000000000000",
			wantStatus:  http.StatusUnauthorized,
			wantUpcalls: false,
		},
		{
			name:        "missing signature",
			secret:      secret,
			sigHeader:   "",
			wantStatus:  http.StatusUnauthorized,
			wantUpcalls: false,
		},
		{
			name:        "secret empty, header ignored",
			secret:      "",
			sigHeader:   "sha256=bogus",
			wantStatus:  http.StatusOK,
			wantUpcalls: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			withStubKQueue(t, acceptJobsHandler("job-123"), tc.secret, func(stub *httptest.Server, calls *int) {
				req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(body))
				req.Header.Set("X-GitHub-Event", "pull_request")
				if tc.sigHeader != "" {
					req.Header.Set("X-Hub-Signature-256", tc.sigHeader)
				}
				rec := httptest.NewRecorder()

				handleGitHubWebhook(rec, req)

				if rec.Code != tc.wantStatus {
					t.Fatalf("status = %d, want %d (body: %s)", rec.Code, tc.wantStatus, rec.Body.String())
				}
				if (*calls > 0) != tc.wantUpcalls {
					t.Fatalf("upstream called = %v, want %v", *calls > 0, tc.wantUpcalls)
				}
			})
		})
	}
}

// jsonDecodeSubmitRequest decodes the SubmitRequest body sent by submitToKQueue,
// draining and restoring the request body so it can still be used by callers.
func jsonDecodeSubmitRequest(r *http.Request) (SubmitRequest, error) {
	var req SubmitRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	return req, err
}
