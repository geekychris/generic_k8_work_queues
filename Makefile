.PHONY: build docker-build docker-up docker-down test test-integration test-e2e \
       submit-echo submit-nlp submit-batch status logs clean help

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

## help: Show available targets
help:
	@echo "KQueue - Scalable Kubernetes Job Queue"
	@echo ""
	@echo "Usage: make <target>"
	@echo ""
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## /  /'
