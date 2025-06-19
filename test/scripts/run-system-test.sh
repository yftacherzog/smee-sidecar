#!/bin/bash

# This script runs a system test to verify event relaying, liveness probe recovery,
# and the accuracy of the health_check metric.

set -ex

SIDECAR_POD_LABEL="app=smee-sidecar"
SIDECAR_DEPLOYMENT="deployment/smee-client"
SMEE_SERVER_DEPLOYMENT="deployment/smee-server"
DOWNSTREAM_POD_LABEL="app=dummy-downstream"

# This function scrapes the /metrics endpoint of the sidecar and returns the value
# of the 'health_check' metric.
# It handles port-forwarding and cleanup automatically.
get_health_check_metric() {
  local pod_name
  pod_name=$(kubectl get pods -l ${SIDECAR_POD_LABEL} -o jsonpath='{.items[0].metadata.name}')
  if [ -z "${pod_name}" ]; then
    echo "Error: Could not find sidecar pod." >&2
    return 1
  fi

  echo "Scraping metrics from pod: ${pod_name}" >&2

  # Port-forward to the management port (9100) in the background.
  # Redirect its noisy output to stderr.
  kubectl port-forward "pod/${pod_name}" 9100:9100 >&2 &
  local pf_pid=$!

  # Ensure the port-forward process is killed on exit or error.
  # The RETURN trap executes whenever the function exits.
  trap 'kill "$pf_pid" && wait "$pf_pid" 2>/dev/null' RETURN

  # Give port-forward a moment to establish.
  sleep 3

  # First, attempt to fetch all metrics. The `|| true` ensures the script doesn't exit on failure.
  local all_metrics
  all_metrics=$(curl -s http://localhost:9100/metrics || true)

  # Now, parse the result. If curl failed, all_metrics will be empty.
  local metric_value
  metric_value=$(echo "${all_metrics}" | grep '^health_check' | awk '{print $2}')

  if [ -z "${metric_value}" ]; then
    echo "Warning: health_check metric not found or scrape failed. Assuming 0." >&2
    # The final value is printed to stdout.
    echo "0"
  else
    # The final value is printed to stdout.
    echo "${metric_value}"
  fi
}


echo "--- Phase 1.1: Verify Initial Health ---"

# Check that the pod is running and has 0 restarts.
SIDECAR_POD_NAME=$(kubectl get pods -l ${SIDECAR_POD_LABEL} -o jsonpath='{.items[0].metadata.name}')
INITIAL_RESTARTS=$(kubectl get pod "${SIDECAR_POD_NAME}" -o jsonpath='{.status.containerStatuses[0].restartCount}')
if [ "${INITIAL_RESTARTS}" -ne "0" ]; then
  echo "Error: Sidecar started with ${INITIAL_RESTARTS} restarts, expected 0."
  exit 1
fi
echo "Sidecar is running with 0 restarts, as expected."

echo "--- Phase 1.2: Verify Initial Metric is Healthy ---"
echo "Wait for the initial delay of the liveness probe to pass."
sleep 10
METRIC=$(get_health_check_metric)
if [ "${METRIC}" != "1" ]; then
  echo "Error: Initial health_check metric was ${METRIC}, expected 1."
  exit 1
fi
echo "Success: Initial health_check metric is 1."

echo "--- Phase 1.3: Verify Event Relaying ---"
TEST_MESSAGE="webhook-test-$(date +%s)"
echo "Sending unique message to relay: ${TEST_MESSAGE}"

# Use kubectl port-forward to send the request from the runner.
echo "Starting port-forward to smee-server-service..."
kubectl port-forward service/smee-server-service 8081:80 &
PORT_FORWARD_PID=$!
# Ensure the port-forward process is killed when the script exits.
trap 'kill "$PORT_FORWARD_PID"' EXIT
sleep 3
echo "Sending curl request to localhost:8081..."
curl -X POST -H "Content-Type: application/json" -H "X-Test-Message: ${TEST_MESSAGE}" -d '{}' "http://localhost:8081/systemcheckchannel"
kill $PORT_FORWARD_PID
trap - EXIT

echo "Waiting for message to appear in downstream logs..."
DOWNSTREAM_POD_NAME=$(kubectl get pods -l "${DOWNSTREAM_POD_LABEL}" -o jsonpath='{.items[0].metadata.name}')
ATTEMPTS=0
MAX_ATTEMPTS=10 # 10 attempts * 3s sleep = 30s timeout
LOG_FOUND=false
while [ $ATTEMPTS -lt $MAX_ATTEMPTS ]; do
  if kubectl logs "${DOWNSTREAM_POD_NAME}" | grep -q "X-Test-Message: ${TEST_MESSAGE}"; then
    echo "Success: Message found in downstream service logs."
    LOG_FOUND=true
    break
  fi
  ATTEMPTS=$((ATTEMPTS + 1))
  sleep 3
done
if [ "$LOG_FOUND" = false ]; then
  echo "Error: Timed out waiting for message in downstream logs."
  kubectl logs "${DOWNSTREAM_POD_NAME}"
  exit 1
fi


echo "--- Phase 2: Break Communication by Scaling Down Smee Server ---"
kubectl scale ${SMEE_SERVER_DEPLOYMENT} --replicas=0
echo "Smee server scaled down to 0 replicas."


echo "--- Phase 3.1: Verify Metric Reflects Unhealthy State ---"
echo "Waiting for healthz probe to fail and update the metric to 0..."
ATTEMPTS=0
MAX_ATTEMPTS=10 # 10 attempts * 6s sleep = 60s timeout
while true; do
  METRIC=$(get_health_check_metric)
  if [ "${METRIC}" == "0" ]; then
    echo "Success: health_check metric is now 0, indicating an unhealthy state."
    break
  fi
  ATTEMPTS=$((ATTEMPTS + 1))
  if [ "${ATTEMPTS}" -gt "${MAX_ATTEMPTS}" ]; then
    echo "Error: Timed out waiting for health_check metric to become 0."
    exit 1
  fi
  echo "Current health_check metric is ${METRIC}. Waiting..."
  sleep 6
done


echo "--- Phase 3.2: Verify Sidecar Restarts due to Liveness Probe ---"
echo "Waiting for liveness probe to fail and for Kubernetes to restart the sidecar..."
ATTEMPTS=0
MAX_ATTEMPTS=30 # 30 attempts * 3s sleep = 90s timeout
while true; do
  # It's possible for the pod name to change if the pod gets recreated, so we fetch it again.
  SIDECAR_POD_NAME=$(kubectl get pods -l ${SIDECAR_POD_LABEL} -o jsonpath='{.items[0].metadata.name}')
  CURRENT_RESTARTS=$(kubectl get pod ${SIDECAR_POD_NAME} -o jsonpath='{.status.containerStatuses[0].restartCount}')
  if [ "${CURRENT_RESTARTS}" -ge "1" ]; then
    echo "Success: Sidecar has restarted. Current count: ${CURRENT_RESTARTS}."
    break
  fi
  ATTEMPTS=$((ATTEMPTS + 1))
  if [ "${ATTEMPTS}" -gt "${MAX_ATTEMPTS}" ]; then
    echo "Error: Timed out waiting for sidecar to restart."
    echo "Final restart count: ${CURRENT_RESTARTS}"
    exit 1
  fi
  echo "Current restart count is ${CURRENT_RESTARTS}. Waiting..."
  sleep 3
done


echo "--- Phase 4: Restore Communication ---"
kubectl scale ${SMEE_SERVER_DEPLOYMENT} --replicas=1
echo "Smee server scaled back up to 1 replica."
kubectl wait --for=condition=Available ${SMEE_SERVER_DEPLOYMENT} --timeout=60s
echo "Smee server is available again."


echo "--- Phase 5.1: Verify Sidecar Recovery via Metric ---"
echo "Waiting for the sidecar to recover and the health_check metric to become 1..."
ATTEMPTS=0
MAX_ATTEMPTS=10 # 10 attempts * 6s sleep = 60s timeout
while true; do
  METRIC=$(get_health_check_metric)
  if [ "${METRIC}" == "1" ]; then
    echo "Success: health_check metric is 1 again. Sidecar has recovered."
    break
  fi
  ATTEMPTS=$((ATTEMPTS + 1))
  if [ "${ATTEMPTS}" -gt "${MAX_ATTEMPTS}" ]; then
    echo "Error: Timed out waiting for sidecar to recover and metric to become 1."
    exit 1
  fi
  echo "Current health_check metric is ${METRIC}. Waiting for recovery..."
  sleep 6
done


echo "--- Phase 5.2: Verify Sidecar Stability Post-Recovery ---"
echo "Waiting for the restarted sidecar to become healthy and stable..."
kubectl wait --for=condition=Available ${SIDECAR_DEPLOYMENT} --timeout=60s

# Get the latest pod name and restart count after recovery.
SIDECAR_POD_NAME=$(kubectl get pods -l ${SIDECAR_POD_LABEL} -o jsonpath='{.items[0].metadata.name}')
RESTARTS_AFTER_RECOVERY=$(kubectl get pod ${SIDECAR_POD_NAME} -o jsonpath='{.status.containerStatuses[0].restartCount}')
echo "Verifying stability. Restart count is ${RESTARTS_AFTER_RECOVERY}. Waiting 15 seconds..."
sleep 15
STABLE_RESTARTS=$(kubectl get pod ${SIDECAR_POD_NAME} -o jsonpath='{.status.containerStatuses[0].restartCount}')
if [ "${STABLE_RESTARTS}" -ne "${RESTARTS_AFTER_RECOVERY}" ]; then
  echo "Error: Sidecar is unstable. Restart count increased from ${RESTARTS_AFTER_RECOVERY} to ${STABLE_RESTARTS}."
  exit 1
fi
echo "Sidecar has recovered and is stable with ${STABLE_RESTARTS} restart(s)."

echo "--- System Test PASSED ---"
