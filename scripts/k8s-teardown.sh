#!/bin/bash
set -euo pipefail

# KQueue Kubernetes Teardown
# Removes all KQueue resources from the cluster.

echo "=== KQueue Kubernetes Teardown ==="

echo "Deleting worker deployments..."
kubectl -n kqueue delete deployment -l managed-by=kqueue 2>/dev/null || true

echo "Deleting core resources..."
kubectl delete -k deploy/base/ 2>/dev/null || true

echo "Deleting secrets..."
kubectl -n kqueue delete secret github-token anthropic-key 2>/dev/null || true

echo "Deleting namespace..."
kubectl delete namespace kqueue 2>/dev/null || true

echo "Done. All KQueue resources removed."
