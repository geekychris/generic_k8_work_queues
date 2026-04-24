#!/usr/bin/env bash
set -euo pipefail

CONTROLLER_URL="${CONTROLLER_URL:-http://localhost:8080}"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

info()  { echo -e "${BLUE}[INFO]${NC}  $*"; }
ok()    { echo -e "${GREEN}[OK]${NC}    $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC}  $*"; }
error() { echo -e "${RED}[ERROR]${NC} $*"; }

header() {
    echo ""
    echo -e "${BLUE}========================================${NC}"
    echo -e "${BLUE}  $*${NC}"
    echo -e "${BLUE}========================================${NC}"
    echo ""
}

# ------------------------------------------------------------------
# Prerequisites
# ------------------------------------------------------------------
header "Checking Prerequisites"

if ! command -v docker &>/dev/null; then
    error "docker is not installed"
    exit 1
fi
ok "docker found"

if ! command -v curl &>/dev/null; then
    error "curl is not installed"
    exit 1
fi
ok "curl found"

# ------------------------------------------------------------------
# Start services
# ------------------------------------------------------------------
header "Starting Services"

info "Building and starting docker compose..."
docker compose build --quiet
docker compose up -d

info "Waiting for services to be healthy..."
MAX_WAIT=60
WAITED=0
while true; do
    if curl -sf "${CONTROLLER_URL}/health" >/dev/null 2>&1; then
        break
    fi
    WAITED=$((WAITED + 2))
    if [ "$WAITED" -ge "$MAX_WAIT" ]; then
        error "Controller did not become healthy within ${MAX_WAIT}s"
        echo "Logs:"
        docker compose logs --tail=20
        exit 1
    fi
    echo -n "."
    sleep 2
done
echo ""
ok "Controller is healthy"

# Give sidecars a moment to connect to NATS and discover streams
sleep 3

# ------------------------------------------------------------------
# Submit echo job
# ------------------------------------------------------------------
header "Submitting Echo Job"

ECHO_RESPONSE=$(curl -sf -X POST "${CONTROLLER_URL}/api/v1/jobs" \
    -H 'Content-Type: application/json' \
    -d '{"queue":"echo","payload":{"message":"Hello from the demo script!","demo":true}}')

ECHO_JOB_ID=$(echo "$ECHO_RESPONSE" | python3 -c "import sys,json; print(json.load(sys.stdin)['job_id'])")
ok "Echo job submitted: ${ECHO_JOB_ID}"
echo "  Response: ${ECHO_RESPONSE}"

# ------------------------------------------------------------------
# Submit NLP job
# ------------------------------------------------------------------
header "Submitting NLP Job"

NLP_RESPONSE=$(curl -sf -X POST "${CONTROLLER_URL}/api/v1/jobs" \
    -H 'Content-Type: application/json' \
    -d '{"queue":"nlp","payload":{"text":"KQueue is a scalable job queue framework. It uses NATS JetStream for durable message delivery. Workers are simple HTTP servers."}}')

NLP_JOB_ID=$(echo "$NLP_RESPONSE" | python3 -c "import sys,json; print(json.load(sys.stdin)['job_id'])")
ok "NLP job submitted: ${NLP_JOB_ID}"
echo "  Response: ${NLP_RESPONSE}"

# ------------------------------------------------------------------
# Show queue stats
# ------------------------------------------------------------------
header "Queue Statistics"

sleep 2
STATS=$(curl -sf "${CONTROLLER_URL}/api/v1/queues")
echo "$STATS" | python3 -m json.tool

# ------------------------------------------------------------------
# Batch submission
# ------------------------------------------------------------------
header "Batch Submission (20 Echo Jobs)"

JOB_IDS=()
for i in $(seq 1 20); do
    RESP=$(curl -sf -X POST "${CONTROLLER_URL}/api/v1/jobs" \
        -H 'Content-Type: application/json' \
        -d "{\"queue\":\"echo\",\"payload\":{\"batch_id\":${i},\"message\":\"batch job ${i}\"}}")
    JID=$(echo "$RESP" | python3 -c "import sys,json; print(json.load(sys.stdin)['job_id'])")
    JOB_IDS+=("$JID")
    printf "  [%2d/20] %s\n" "$i" "$JID"
done

ok "All 20 jobs submitted"

# ------------------------------------------------------------------
# Poll for completion
# ------------------------------------------------------------------
header "Waiting for Jobs to Complete"

MAX_POLL=60
POLLED=0
while true; do
    STATS=$(curl -sf "${CONTROLLER_URL}/api/v1/queues")
    ECHO_PENDING=$(echo "$STATS" | python3 -c "
import sys, json
queues = json.load(sys.stdin)
for q in queues:
    if q['name'] == 'echo':
        print(q.get('pending', 0) + q.get('processing', 0))
        break
else:
    print(0)
")
    if [ "$ECHO_PENDING" -eq 0 ] 2>/dev/null; then
        break
    fi
    POLLED=$((POLLED + 2))
    if [ "$POLLED" -ge "$MAX_POLL" ]; then
        warn "Timed out waiting for completion (${ECHO_PENDING} still in queue)"
        break
    fi
    info "Still processing... ${ECHO_PENDING} remaining"
    sleep 2
done

ok "Processing complete"

# ------------------------------------------------------------------
# Final stats
# ------------------------------------------------------------------
header "Final Queue Statistics"

curl -sf "${CONTROLLER_URL}/api/v1/queues" | python3 -m json.tool

# ------------------------------------------------------------------
# Check individual job result
# ------------------------------------------------------------------
header "Sample Job Result"

info "Checking echo job ${ECHO_JOB_ID}..."
curl -sf "${CONTROLLER_URL}/api/v1/jobs/${ECHO_JOB_ID}" | python3 -m json.tool

echo ""
info "Checking NLP job ${NLP_JOB_ID}..."
curl -sf "${CONTROLLER_URL}/api/v1/jobs/${NLP_JOB_ID}" | python3 -m json.tool

# ------------------------------------------------------------------
# Open UI
# ------------------------------------------------------------------
header "Demo Complete"

echo "Web dashboard: ${CONTROLLER_URL}"
echo "NATS monitor:  http://localhost:8222"
echo ""
echo "Useful commands:"
echo "  make status       - show queue stats"
echo "  make submit-echo  - submit another echo job"
echo "  make submit-nlp   - submit another NLP job"
echo "  make submit-batch - submit 20 more echo jobs"
echo "  make logs         - follow service logs"
echo "  make docker-down  - stop everything"
echo ""

# Try to open the UI in a browser
if command -v open &>/dev/null; then
    info "Opening dashboard in browser..."
    open "${CONTROLLER_URL}" 2>/dev/null || true
elif command -v xdg-open &>/dev/null; then
    info "Opening dashboard in browser..."
    xdg-open "${CONTROLLER_URL}" 2>/dev/null || true
fi
