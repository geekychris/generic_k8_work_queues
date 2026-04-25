#!/bin/bash
set -euo pipefail

# KQueue Kubernetes Setup Script
# Builds images, creates secrets, and deploys to a local Kubernetes cluster.
#
# Prerequisites:
#   - kubectl configured and pointing to your cluster
#   - Docker running (images must be accessible to the cluster)
#   - Optional: GITHUB_TOKEN env var for code review PR posting
#   - Optional: ANTHROPIC_API_KEY env var for Claude-powered reviews
#
# Usage:
#   ./scripts/k8s-setup.sh                    # Deploy with local ollama
#   GITHUB_TOKEN=ghp_... ./scripts/k8s-setup.sh  # With GitHub integration
#   ANTHROPIC_API_KEY=sk-ant-... GITHUB_TOKEN=ghp_... ./scripts/k8s-setup.sh  # With Claude

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"

echo "=== KQueue Kubernetes Setup ==="
echo ""

# --- Step 1: Build images ---
echo "[1/5] Building Docker images..."
cd "$PROJECT_DIR"
docker compose build 2>&1 | grep -E "Built$|Error" || true

# Tag images with the names k8s expects
echo "[1/5] Tagging images..."
docker tag scalable_ks_queue_process-controller kqueue/controller:latest
docker tag scalable_ks_queue_process-echo-sidecar kqueue/sidecar:latest
docker tag scalable_ks_queue_process-echo-worker kqueue/echo-worker:latest
docker tag scalable_ks_queue_process-nlp-worker kqueue/nlp-worker:latest
docker tag scalable_ks_queue_process-sandbox-worker kqueue/sandbox-worker:latest
docker tag scalable_ks_queue_process-codereview-worker kqueue/codereview-worker:latest
docker tag scalable_ks_queue_process-webhook kqueue/webhook:latest
echo "  Done."

# --- Step 2: Deploy core ---
echo ""
echo "[2/5] Deploying to Kubernetes..."
kubectl apply -k deploy/base/ 2>&1 | sed 's/^/  /'

# --- Step 3: Create secrets ---
echo ""
echo "[3/5] Configuring secrets..."

if [ -n "${GITHUB_TOKEN:-}" ]; then
    kubectl -n kqueue create secret generic github-token \
        --from-literal=token="$GITHUB_TOKEN" \
        --dry-run=client -o yaml | kubectl apply -f - 2>&1 | sed 's/^/  /'
    echo "  GitHub token configured."
else
    echo "  GITHUB_TOKEN not set — code reviews will work but won't post to GitHub."
    echo "  To add later: kubectl -n kqueue create secret generic github-token --from-literal=token=ghp_..."
fi

if [ -n "${ANTHROPIC_API_KEY:-}" ]; then
    kubectl -n kqueue create secret generic anthropic-key \
        --from-literal=key="$ANTHROPIC_API_KEY" \
        --dry-run=client -o yaml | kubectl apply -f - 2>&1 | sed 's/^/  /'
    echo "  Anthropic API key configured."

    # Patch codereview worker to use Anthropic instead of ollama
    kubectl -n kqueue patch deployment kqueue-worker-codereview --type=json -p='[
      {
        "op": "replace",
        "path": "/spec/template/spec/containers/0/env",
        "value": [
          {"name": "OPENCLAUDE_PROVIDER", "value": "anthropic"},
          {"name": "OPENCLAUDE_MODEL", "value": "claude-sonnet-4-6"},
          {"name": "ANTHROPIC_API_KEY", "valueFrom": {"secretKeyRef": {"name": "anthropic-key", "key": "key"}}},
          {"name": "POST_REVIEWS", "value": "true"},
          {"name": "GITHUB_TOKEN", "valueFrom": {"secretKeyRef": {"name": "github-token", "key": "token"}}}
        ]
      }
    ]' 2>&1 | sed 's/^/  /'
    echo "  Codereview worker switched to Anthropic Claude."
else
    echo "  ANTHROPIC_API_KEY not set — code reviews will use local ollama."
    echo "  To add later: kubectl -n kqueue create secret generic anthropic-key --from-literal=key=sk-ant-..."

    # If we have a GitHub token, patch the codereview worker to use it
    if [ -n "${GITHUB_TOKEN:-}" ]; then
        kubectl -n kqueue patch deployment kqueue-worker-codereview --type=json -p='[
          {
            "op": "add",
            "path": "/spec/template/spec/containers/0/env/-",
            "value": {"name": "GITHUB_TOKEN", "valueFrom": {"secretKeyRef": {"name": "github-token", "key": "token"}}}
          }
        ]' 2>&1 | sed 's/^/  /' || true
    fi
fi

# --- Step 4: Wait for pods ---
echo ""
echo "[4/5] Waiting for pods to be ready..."
kubectl -n kqueue rollout status deployment/kqueue-controller --timeout=60s 2>&1 | sed 's/^/  /'
kubectl -n kqueue rollout status statefulset/nats --timeout=60s 2>&1 | sed 's/^/  /'

# Wait for worker pods (they're created by the controller, so they appear after a delay)
sleep 10
echo "  Pods:"
kubectl -n kqueue get pods -o custom-columns='NAME:.metadata.name,READY:.status.conditions[?(@.type=="Ready")].status,STATUS:.status.phase' 2>&1 | sed 's/^/  /'

# --- Step 5: Port-forward ---
echo ""
echo "[5/5] Setup complete!"
echo ""
echo "To access the services:"
echo "  kubectl -n kqueue port-forward svc/kqueue-controller 8080:8080 &"
echo "  kubectl -n kqueue port-forward svc/kqueue-webhook 9000:9000 &"
echo ""
echo "Then:"
echo "  Dashboard:     http://localhost:8080"
echo "  Submit echo:   make submit-echo"
echo "  Submit review: make submit-review"
echo "  Queue stats:   make status"
echo ""
echo "Secrets configured:"
echo "  GITHUB_TOKEN:     $(kubectl -n kqueue get secret github-token -o name 2>/dev/null && echo 'yes' || echo 'no')"
echo "  ANTHROPIC_API_KEY: $(kubectl -n kqueue get secret anthropic-key -o name 2>/dev/null && echo 'yes' || echo 'no')"
