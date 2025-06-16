#!/bin/bash

# This script runs a system test to verify the liveness probe and recovery mechanism.

set -ex

SIDECAR_POD_LABEL="app=smee-sidecar"
SIDECAR_DEPLOYMENT="deployment/smee-client"
SMEE_SERVER_DEPLOYMENT="deployment/smee-server"

echo "--- Phase 1: Verify Initial Health ---"

# Get the name of the running sidecar pod.
SIDECAR_POD_NAME=$(kubectl get pods -l ${SIDECAR_POD_LABEL} -o jsonpath='{.items[0].metadata.name}')

# Check that the pod is running and has 0 restarts.
INITIAL_RESTARTS=$(kubectl get pod ${SIDECAR_POD_NAME} -o jsonpath='{.status.containerStatuses[0].restartCount}')
if [ "${INITIAL_RESTARTS}" -ne "0" ]; then
  echo "Error: Sidecar started with ${INITIAL_RESTARTS} restarts, expected 0."
  exit 1
fi
echo "Sidecar is running with 0 restarts, as expected."

echo "--- Phase 2: Break Communication by Scaling Down Smee Server ---"

kubectl scale ${SMEE_SERVER_DEPLOYMENT} --replicas=0
echo "Smee server scaled down to 0 replicas."

echo "--- Phase 3: Verify Sidecar Restarts ---"
echo "Waiting for liveness probe to fail and for Kubernetes to restart the sidecar..."

# Loop and wait for the restart count to become at least 1.
# Timeout after 90 seconds to prevent the job from running forever.
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


echo "--- Phase 5: Verify Sidecar Recovery and Stability ---"
echo "Waiting for the restarted sidecar to become healthy..."
kubectl wait --for=condition=Available ${SIDECAR_DEPLOYMENT} --timeout=60s

# Get the latest pod name and restart count after recovery.
SIDECAR_POD_NAME=$(kubectl get pods -l ${SIDECAR_POD_LABEL} -o jsonpath='{.items[0].metadata.name}')
RESTARTS_AFTER_RECOVERY=$(kubectl get pod ${SIDECAR_POD_NAME} -o jsonpath='{.status.containerStatuses[0].restartCount}')

# Verify that the restart count is stable by checking it again after a delay.
echo "Verifying stability. Restart count is ${RESTARTS_AFTER_RECOVERY}. Waiting 15 seconds..."
sleep 15
STABLE_RESTARTS=$(kubectl get pod ${SIDECAR_POD_NAME} -o jsonpath='{.status.containerStatuses[0].restartCount}')

if [ "${STABLE_RESTARTS}" -ne "${RESTARTS_AFTER_RECOVERY}" ]; then
  echo "Error: Sidecar is unstable. Restart count increased from ${RESTARTS_AFTER_RECOVERY} to ${STABLE_RESTARTS}."
  exit 1
fi
echo "Sidecar has recovered and is stable with ${STABLE_RESTARTS} restart(s)."

echo "--- System Test PASSED ---"
