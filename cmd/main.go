package main

import (
	"bytes"
	"context"
	"crypto/tls"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

//go:embed scripts/check-smee-health.sh
var smeeHealthScript []byte

//go:embed scripts/check-sidecar-health.sh
var sidecarHealthScript []byte

//go:embed scripts/check-file-age.sh
var fileAgeScript []byte

// HealthStatus represents the current health status
type HealthStatus struct {
	Status  string // "success" or "failure"
	Message string
}

var (
	forwardAttempts = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "smee_events_relayed_total",
			Help: "Total number of regular events relayed by the sidecar.",
		},
	)
	// Gauge metric to track the health check status.
	health_check = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "health_check",
			Help: "Indicates the outcome of the last completed health check (1 for OK, 0 for failure).",
		},
	)
	// The mutex protects a map where the KEY is the test ID
	// and the VALUE is a channel that the handler will wait on.
	healthChecks = make(map[string]chan bool)
	mutex        = &sync.Mutex{}
	// Global downstream service URL for per-request proxy creation
	downstreamServiceURL string
)

type HealthCheckPayload struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// createProxy creates a fresh reverse proxy for each request to prevent memory accumulation
func createProxy(downstreamURL string) *httputil.ReverseProxy {
	parsedURL, err := url.Parse(downstreamURL)
	if err != nil {
		log.Printf("ERROR: Could not parse downstream URL %s: %v", downstreamURL, err)
		return nil
	}
	return httputil.NewSingleHostReverseProxy(parsedURL)
}

// forwardHandler needs to find the correct channel to signal success.
func forwardHandler(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "cannot read request body", http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()

	var healthCheck HealthCheckPayload
	if r.Header.Get("Content-Type") == "application/json" {
		if json.Unmarshal(body, &healthCheck) == nil && healthCheck.Type == "health-check" {
			mutex.Lock()
			// Find the waiting channel for this specific ID and signal it.
			if resultChan, ok := healthChecks[healthCheck.ID]; ok {
				log.Printf("Health Check: Intercepted health check event with ID: %s", healthCheck.ID)
				resultChan <- true
				delete(healthChecks, healthCheck.ID) // Clean up the map
			}
			mutex.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}
	}
	forwardAttempts.Inc()
	r.Body = io.NopCloser(bytes.NewReader(body))
	proxy := createProxy(downstreamServiceURL)
	if proxy == nil {
		http.Error(w, "internal server error: failed to create proxy", http.StatusInternalServerError)
		return
	}
	proxy.ServeHTTP(w, r)
}

// writeScriptsToVolume writes the embedded probe scripts to the shared volume
func writeScriptsToVolume(sharedPath string) error {
	scripts := map[string][]byte{
		"check-smee-health.sh":    smeeHealthScript,
		"check-sidecar-health.sh": sidecarHealthScript,
		"check-file-age.sh":       fileAgeScript,
	}

	for filename, content := range scripts {
		scriptPath := filepath.Join(sharedPath, filename)

		// Check if file exists and make it writable before overwriting
		// This handles container restarts where the volume persists with read-only files
		if _, err := os.Stat(scriptPath); err == nil {
			if err := os.Chmod(scriptPath, 0755); err != nil {
				return fmt.Errorf("failed to make %s writable: %v", filename, err)
			}
		}

		if err := os.WriteFile(scriptPath, content, 0755); err != nil {
			return fmt.Errorf("failed to write %s: %v", filename, err)
		}

		// Make script read-only to prevent accidental modification
		if err := os.Chmod(scriptPath, 0555); err != nil {
			return fmt.Errorf("failed to make %s read-only: %v", filename, err)
		}

		log.Printf("Wrote read-only probe script: %s", scriptPath)
	}
	return nil
}

// writeHealthStatus writes health status to file atomically
func writeHealthStatus(status *HealthStatus, filePath string) error {
	// Simple format with only fields used by probe scripts
	content := fmt.Sprintf("status=%s\nmessage=%s\n",
		status.Status,
		status.Message,
	)

	// Atomic write: write to temp file, then rename
	tmpPath := filePath + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(content), 0644); err != nil {
		return fmt.Errorf("failed to write temp file: %v", err)
	}

	if err := os.Rename(tmpPath, filePath); err != nil {
		return fmt.Errorf("failed to rename temp file: %v", err)
	}

	return nil
}

// performHealthCheck executes a single end-to-end health check
func performHealthCheck(smeeChannelURL string, timeoutSeconds int) *HealthStatus {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSeconds)*time.Second)
	defer cancel()

	testID := uuid.New().String()
	status := &HealthStatus{
		Status:  "failure",
		Message: "Health check failed",
	}

	payload := HealthCheckPayload{Type: "health-check", ID: testID}
	payloadBytes, _ := json.Marshal(payload)

	// Create a channel for this specific request and register it.
	resultChan := make(chan bool, 1)
	mutex.Lock()
	healthChecks[testID] = resultChan
	mutex.Unlock()

	// Ensure we always clean up the map entry for this ID.
	defer func() {
		mutex.Lock()
		delete(healthChecks, testID)
		mutex.Unlock()
	}()

	// Create and send the POST request.
	req, err := http.NewRequestWithContext(ctx, "POST", smeeChannelURL, bytes.NewBuffer(payloadBytes))
	if err != nil {
		status.Message = fmt.Sprintf("Failed to create request: %v", err)
		return status
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: "true" == os.Getenv("INSECURE_SKIP_VERIFY")},
		},
	}

	if _, err = client.Do(req); err != nil {
		status.Message = fmt.Sprintf("Failed to POST to smee server: %v", err)
		return status
	}

	// Wait for the forwardHandler to receive the event, or for the timeout.
	select {
	case <-resultChan:
		status.Status = "success"
		status.Message = "Health check completed successfully"
	case <-ctx.Done():
		status.Message = "Health check timed out waiting for event round-trip"
	}

	return status
}

// runHealthChecker runs the background health checker
func runHealthChecker(ctx context.Context, smeeChannelURL, healthFilePath string, intervalSeconds, timeoutSeconds int) {
	ticker := time.NewTicker(time.Duration(intervalSeconds) * time.Second)
	defer ticker.Stop()

	log.Printf("Starting background health checker (interval: %ds, timeout: %ds)", intervalSeconds, timeoutSeconds)

	for {
		select {
		case <-ctx.Done():
			log.Println("Health checker stopped")
			return
		case <-ticker.C:
			status := performHealthCheck(smeeChannelURL, timeoutSeconds)

			if err := writeHealthStatus(status, healthFilePath); err != nil {
				log.Printf("Failed to write health status: %v", err)
			} else {
				log.Printf("Health check completed: %s (%s)", status.Status, status.Message)
			}

			// Update Prometheus metric
			if status.Status == "success" {
				health_check.Set(1)
			} else {
				health_check.Set(0)
			}
		}
	}
}

func main() {
	log.Println("Starting Smee instrumentation sidecar...")

	// Environment variables
	downstreamServiceURL = os.Getenv("DOWNSTREAM_SERVICE_URL")
	if downstreamServiceURL == "" {
		log.Fatal("FATAL: DOWNSTREAM_SERVICE_URL environment variable must be set.")
	}

	smeeChannelURL := os.Getenv("SMEE_CHANNEL_URL")
	if smeeChannelURL == "" {
		log.Fatal("FATAL: SMEE_CHANNEL_URL environment variable must be set.")
	}

	sharedPath := os.Getenv("SHARED_VOLUME_PATH")
	if sharedPath == "" {
		sharedPath = "/shared"
	}

	healthFilePath := os.Getenv("HEALTH_FILE_PATH")
	if healthFilePath == "" {
		healthFilePath = filepath.Join(sharedPath, "health-status.txt")
	}

	// Parse configuration
	healthCheckInterval := 30
	if intervalStr := os.Getenv("HEALTH_CHECK_INTERVAL_SECONDS"); intervalStr != "" {
		if val, err := strconv.Atoi(intervalStr); err == nil && val > 0 {
			healthCheckInterval = val
		}
	}

	healthCheckTimeout := 20
	if timeoutStr := os.Getenv("HEALTH_CHECK_TIMEOUT_SECONDS"); timeoutStr != "" {
		if val, err := strconv.Atoi(timeoutStr); err == nil && val > 0 {
			healthCheckTimeout = val
		}
	}

	// Write probe scripts to shared volume
	if err := writeScriptsToVolume(sharedPath); err != nil {
		log.Fatalf("FATAL: Failed to write probe scripts: %v", err)
	}

	// Register metrics with Prometheus.
	prometheus.MustRegister(forwardAttempts)
	prometheus.MustRegister(health_check)

	// Start background health checker
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runHealthChecker(ctx, smeeChannelURL, healthFilePath, healthCheckInterval, healthCheckTimeout)

	// --- Relay Server (on port 8080) ---
	relayMux := http.NewServeMux()
	relayMux.HandleFunc("/", forwardHandler)
	go func() {
		log.Println("Relay server listening on :8080")
		if err := http.ListenAndServe(":8080", relayMux); err != nil {
			log.Fatalf("FATAL: Relay server failed: %v", err)
		}
	}()

	// --- Management Server (on port 9100) ---
	mgmtMux := http.NewServeMux()
	mgmtMux.Handle("/metrics", promhttp.Handler())
	go func() {
		log.Println("Management server (metrics) listening on :9100")
		if err := http.ListenAndServe(":9100", mgmtMux); err != nil {
			log.Fatalf("FATAL: Management server failed: %v", err)
		}
	}()

	select {}
}
