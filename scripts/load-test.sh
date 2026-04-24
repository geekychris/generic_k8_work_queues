#!/usr/bin/env bash
set -euo pipefail

CONTROLLER_URL="${CONTROLLER_URL:-http://localhost:8080}"

usage() {
    echo "Usage: $0 <queue> <count>"
    echo ""
    echo "Submit <count> jobs to <queue> and report timing statistics."
    echo ""
    echo "Arguments:"
    echo "  queue   Queue name (e.g., echo, nlp)"
    echo "  count   Number of jobs to submit"
    echo ""
    echo "Environment:"
    echo "  CONTROLLER_URL  Controller address (default: http://localhost:8080)"
    echo ""
    echo "Examples:"
    echo "  $0 echo 50"
    echo "  $0 nlp 20"
    echo "  CONTROLLER_URL=http://myhost:8080 $0 echo 100"
    exit 1
}

if [ $# -lt 2 ]; then
    usage
fi

QUEUE="$1"
COUNT="$2"

if ! [[ "$COUNT" =~ ^[0-9]+$ ]] || [ "$COUNT" -eq 0 ]; then
    echo "Error: count must be a positive integer"
    exit 1
fi

# Verify controller is reachable
if ! curl -sf "${CONTROLLER_URL}/health" >/dev/null 2>&1; then
    echo "Error: controller not reachable at ${CONTROLLER_URL}"
    echo "Start services first: make docker-up"
    exit 1
fi

echo "=== KQueue Load Test ==="
echo "Queue:      ${QUEUE}"
echo "Count:      ${COUNT}"
echo "Controller: ${CONTROLLER_URL}"
echo ""

# Build a payload based on queue type
make_payload() {
    local i=$1
    case "$QUEUE" in
        nlp)
            echo "{\"text\":\"Load test message number ${i}. This sentence tests NLP processing under load.\"}"
            ;;
        *)
            echo "{\"batch_id\":${i},\"message\":\"load test job ${i}\",\"timestamp\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"}"
            ;;
    esac
}

# ------------------------------------------------------------------
# Submit phase
# ------------------------------------------------------------------
echo "--- Submitting ${COUNT} jobs ---"

SUBMIT_START=$(date +%s%N 2>/dev/null || python3 -c "import time; print(int(time.time()*1e9))")
SUBMITTED=0
FAILED=0
JOB_IDS=()

for i in $(seq 1 "$COUNT"); do
    PAYLOAD=$(make_payload "$i")
    BODY="{\"queue\":\"${QUEUE}\",\"payload\":${PAYLOAD}}"

    RESP=$(curl -sf -X POST "${CONTROLLER_URL}/api/v1/jobs" \
        -H 'Content-Type: application/json' \
        -d "$BODY" 2>/dev/null) || true

    if [ -n "$RESP" ]; then
        JID=$(echo "$RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('job_id',''))" 2>/dev/null) || JID=""
        if [ -n "$JID" ]; then
            JOB_IDS+=("$JID")
            SUBMITTED=$((SUBMITTED + 1))
        else
            FAILED=$((FAILED + 1))
        fi
    else
        FAILED=$((FAILED + 1))
    fi

    # Progress every 10 jobs
    if [ $((i % 10)) -eq 0 ] || [ "$i" -eq "$COUNT" ]; then
        printf "  [%d/%d] submitted (%d failed)\n" "$i" "$COUNT" "$FAILED"
    fi
done

SUBMIT_END=$(date +%s%N 2>/dev/null || python3 -c "import time; print(int(time.time()*1e9))")
SUBMIT_DURATION_MS=$(( (SUBMIT_END - SUBMIT_START) / 1000000 ))

echo ""
echo "Submitted: ${SUBMITTED}/${COUNT} (${FAILED} failed)"
echo "Submit duration: ${SUBMIT_DURATION_MS}ms"
if [ "$SUBMITTED" -gt 0 ] && [ "$SUBMIT_DURATION_MS" -gt 0 ]; then
    RATE=$(python3 -c "print(f'{${SUBMITTED} / (${SUBMIT_DURATION_MS} / 1000):.1f}')")
    echo "Submit rate: ${RATE} jobs/sec"
fi

# ------------------------------------------------------------------
# Wait for completion
# ------------------------------------------------------------------
echo ""
echo "--- Waiting for processing to complete ---"

PROCESS_START=$(date +%s%N 2>/dev/null || python3 -c "import time; print(int(time.time()*1e9))")
MAX_WAIT=300
WAITED=0
LAST_REMAINING=-1

while true; do
    STATS=$(curl -sf "${CONTROLLER_URL}/api/v1/queues" 2>/dev/null) || true
    if [ -z "$STATS" ]; then
        sleep 2
        WAITED=$((WAITED + 2))
        continue
    fi

    REMAINING=$(echo "$STATS" | python3 -c "
import sys, json
queues = json.load(sys.stdin)
for q in queues:
    if q['name'] == '${QUEUE}':
        print(int(q.get('pending', 0)) + int(q.get('processing', 0)))
        break
else:
    print(0)
" 2>/dev/null) || REMAINING=0

    if [ "$REMAINING" -eq 0 ] 2>/dev/null; then
        break
    fi

    if [ "$REMAINING" -ne "$LAST_REMAINING" ]; then
        echo "  Remaining: ${REMAINING}"
        LAST_REMAINING=$REMAINING
    fi

    WAITED=$((WAITED + 2))
    if [ "$WAITED" -ge "$MAX_WAIT" ]; then
        echo "  Timed out after ${MAX_WAIT}s (${REMAINING} still remaining)"
        break
    fi
    sleep 2
done

PROCESS_END=$(date +%s%N 2>/dev/null || python3 -c "import time; print(int(time.time()*1e9))")
PROCESS_DURATION_MS=$(( (PROCESS_END - PROCESS_START) / 1000000 ))

echo ""

# ------------------------------------------------------------------
# Final stats
# ------------------------------------------------------------------
echo "=== Results ==="

FINAL_STATS=$(curl -sf "${CONTROLLER_URL}/api/v1/queues" 2>/dev/null) || FINAL_STATS="[]"
echo "$FINAL_STATS" | python3 -c "
import sys, json
queues = json.load(sys.stdin)
for q in queues:
    if q['name'] == '${QUEUE}':
        print(f'  Pending:     {q.get(\"pending\", 0)}')
        print(f'  Processing:  {q.get(\"processing\", 0)}')
        print(f'  Completed:   {q.get(\"completed\", 0)}')
        print(f'  Failed:      {q.get(\"failed\", 0)}')
        print(f'  Dead Letter: {q.get(\"dead_letter\", 0)}')
        print(f'  Workers:     {q.get(\"workers\", \"?\")}')
        break
" 2>/dev/null || echo "  (could not fetch final stats)"

echo ""
echo "  Jobs submitted:      ${SUBMITTED}"
echo "  Submit duration:     ${SUBMIT_DURATION_MS}ms"
echo "  Processing duration: ${PROCESS_DURATION_MS}ms"
TOTAL_MS=$((SUBMIT_DURATION_MS + PROCESS_DURATION_MS))
echo "  Total duration:      ${TOTAL_MS}ms"

if [ "$SUBMITTED" -gt 0 ] && [ "$TOTAL_MS" -gt 0 ]; then
    THROUGHPUT=$(python3 -c "print(f'{${SUBMITTED} / (${TOTAL_MS} / 1000):.2f}')")
    echo "  Throughput:          ${THROUGHPUT} jobs/sec (end to end)"
fi

echo ""
echo "Done."
