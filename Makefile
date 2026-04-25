.PHONY: build docker-build docker-up docker-down test test-integration test-e2e \
       submit-echo submit-nlp submit-sandbox submit-review submit-review-pr submit-batch \
       status logs clean help k8s-setup k8s-teardown k8s-status k8s-logs k8s-portforward

CONTROLLER_URL ?= http://localhost:8080

## build: Build all Go binaries locally
build:
	CGO_ENABLED=0 go build -ldflags="-s -w" -o bin/controller ./cmd/controller
	CGO_ENABLED=0 go build -ldflags="-s -w" -o bin/sidecar ./cmd/sidecar

## docker-build: Build all Docker images
docker-build:
	docker compose build

## docker-up: Start all services via docker compose
docker-up:
	docker compose up -d

## docker-down: Stop all services
docker-down:
	docker compose down

## test: Run unit tests
test:
	go test ./pkg/... ./internal/... -v -count=1

## test-integration: Run integration tests
test-integration:
	go test ./test/ -run Test -v -count=1

## test-e2e: Run e2e tests (requires docker-compose running)
test-e2e:
	go test ./test/ -run TestE2E -v -count=1 -tags=e2e

## submit-echo: Submit a sample echo job
submit-echo:
	@curl -s -X POST $(CONTROLLER_URL)/api/v1/jobs \
		-H 'Content-Type: application/json' \
		-d '{"queue":"echo","payload":{"message":"hello from make","timestamp":"$(shell date -u +%Y-%m-%dT%H:%M:%SZ)"}}' | python3 -m json.tool

## submit-nlp: Submit a sample NLP job
submit-nlp:
	@curl -s -X POST $(CONTROLLER_URL)/api/v1/jobs \
		-H 'Content-Type: application/json' \
		-d '{"queue":"nlp","payload":{"text":"The quick brown fox jumps over the lazy dog. Natural language processing is a subfield of linguistics and computer science."}}' | python3 -m json.tool

## submit-sandbox: Submit a sample sandbox job (runs a shell command)
submit-sandbox:
	@curl -s -X POST $(CONTROLLER_URL)/api/v1/jobs \
		-H 'Content-Type: application/json' \
		-d '{"queue":"sandbox","payload":{"action":"exec","command":"echo hello from sandbox && uname -a && date"}}' | python3 -m json.tool

## submit-review-pr: Submit a real PR for review (usage: make submit-review-pr OWNER=foo REPO=bar PR=123)
submit-review-pr:
	@if [ -z "$(OWNER)" ] || [ -z "$(REPO)" ] || [ -z "$(PR)" ]; then \
		echo "Usage: make submit-review-pr OWNER=<owner> REPO=<repo> PR=<number>"; \
		echo "Example: make submit-review-pr OWNER=octocat REPO=hello-world PR=42"; \
		exit 1; \
	fi
	@curl -s -X POST http://localhost:9000/webhook/test \
		-H 'Content-Type: application/json' \
		-d '{"action":"review_pr","repo_owner":"$(OWNER)","repo_name":"$(REPO)","pr_number":$(PR),"clone_url":"https://github.com/$(OWNER)/$(REPO).git","sender":"kqueue-reviewer"}' \
		| python3 -m json.tool

## submit-review: Submit a test code review via the webhook service
submit-review:
	@curl -s -X POST http://localhost:9000/webhook/test \
		-H 'Content-Type: application/json' \
		-d '{ \
			"repo_owner": "octocat", \
			"repo_name": "hello-world", \
			"pr_number": 42, \
			"pr_title": "Add user authentication", \
			"pr_body": "This PR adds JWT-based authentication to the API endpoints.", \
			"sender": "developer", \
			"ref": "feature/auth", \
			"files_changed": [ \
				{"filename": "auth/jwt.go", "status": "added", "patch": "+package auth\n+\n+import (\n+\t\"time\"\n+\t\"github.com/golang-jwt/jwt/v5\"\n+)\n+\n+func GenerateToken(userID string, secret []byte) (string, error) {\n+\ttoken := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{\n+\t\t\"user_id\": userID,\n+\t\t\"exp\": time.Now().Add(24 * time.Hour).Unix(),\n+\t})\n+\treturn token.SignedString(secret)\n+}"}, \
				{"filename": "middleware/auth.go", "status": "added", "patch": "+package middleware\n+\n+func AuthRequired(next http.Handler) http.Handler {\n+\treturn http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {\n+\t\ttoken := r.Header.Get(\"Authorization\")\n+\t\tif token == \"\" {\n+\t\t\thttp.Error(w, \"unauthorized\", 401)\n+\t\t\treturn\n+\t\t}\n+\t\tnext.ServeHTTP(w, r)\n+\t})\n+}"}, \
				{"filename": "main.go", "status": "modified", "patch": "@@ -15,6 +15,7 @@\n import (\n \t\"net/http\"\n+\t\"myapp/middleware\"\n )\n@@ -28,6 +29,7 @@\n \tr := chi.NewRouter()\n+\tr.Use(middleware.AuthRequired)\n \tr.Get(\"/api/users\", listUsers)"} \
			] \
		}' | python3 -m json.tool

## submit-batch: Submit 20 echo jobs rapidly to test scaling
submit-batch:
	@echo "Submitting 20 echo jobs..."
	@for i in $$(seq 1 20); do \
		curl -s -X POST $(CONTROLLER_URL)/api/v1/jobs \
			-H 'Content-Type: application/json' \
			-d "{\"queue\":\"echo\",\"payload\":{\"batch_id\":$$i,\"message\":\"batch job $$i\"}}" \
			| python3 -c "import sys,json; r=json.load(sys.stdin); print('  [$$i/20] job_id=' + r.get('job_id','?'))"; \
	done
	@echo "Done. Run 'make status' to check progress."

## status: Show queue statistics
status:
	@echo "=== Queue Stats ==="
	@curl -s $(CONTROLLER_URL)/api/v1/queues | python3 -m json.tool
	@echo ""
	@echo "=== Controller Health ==="
	@curl -s $(CONTROLLER_URL)/health | python3 -m json.tool

## logs: Follow docker compose logs
logs:
	docker compose logs -f

## clean: Remove containers, volumes, and build artifacts
clean:
	docker compose down -v --remove-orphans
	rm -rf bin/
	rm -f controller sidecar

## k8s-setup: Build images and deploy to Kubernetes (set GITHUB_TOKEN and/or ANTHROPIC_API_KEY env vars)
k8s-setup:
	./scripts/k8s-setup.sh

## k8s-teardown: Remove all KQueue resources from Kubernetes
k8s-teardown:
	./scripts/k8s-teardown.sh

## k8s-status: Show Kubernetes pod status and queue stats
k8s-status:
	@echo "=== Pods ==="
	@kubectl -n kqueue get pods
	@echo ""
	@echo "=== Secrets ==="
	@kubectl -n kqueue get secrets 2>/dev/null || true
	@echo ""
	@echo "=== Queue Stats (requires port-forward) ==="
	@curl -s $(CONTROLLER_URL)/api/v1/queues 2>/dev/null | python3 -m json.tool 2>/dev/null || echo "  Port-forward not active. Run: make k8s-portforward"

## k8s-logs: Follow codereview worker logs in Kubernetes
k8s-logs:
	kubectl -n kqueue logs -l kqueue/queue=codereview -c worker -f --tail=50

## k8s-portforward: Start port-forwarding for controller and webhook
k8s-portforward:
	@echo "Starting port-forwards (Ctrl+C to stop)..."
	@echo "  Dashboard: http://localhost:8080"
	@echo "  Webhook:   http://localhost:9000"
	@kubectl -n kqueue port-forward svc/kqueue-controller 8080:8080 &
	@kubectl -n kqueue port-forward svc/kqueue-webhook 9000:9000 &
	@echo "Port-forwards started in background. Use 'kill %1 %2' to stop."

## help: Show available targets
help:
	@echo "KQueue - Scalable Kubernetes Job Queue"
	@echo ""
	@echo "Usage: make <target>"
	@echo ""
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## /  /'
