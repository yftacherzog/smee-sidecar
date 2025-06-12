package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	forwardAttempts = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "smee_events_relayed_total",
			Help: "Total number of regular events relayed by the sidecar.",
		},
	)
	// The mutex protects a map where the KEY is the test ID
	// and the VALUE is a channel that the handler will wait on.
	healthChecks = make(map[string]chan bool)
	mutex        = &sync.Mutex{}
	proxy        *httputil.ReverseProxy
)

type HealthCheckPayload struct {
	Type string `json:"type"`
	ID   string `json:"id"`
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
	proxy.ServeHTTP(w, r)
}

// healthzHandler performs a single, synchronous end-to-end check.
func healthzHandler(w http.ResponseWriter, r *http.Request) {
	smeeChannelURL := os.Getenv("SMEE_CHANNEL_URL")
	if smeeChannelURL == "" {
		log.Println("Healthz Error: SMEE_CHANNEL_URL env var is not set.")
		http.Error(w, "Sidecar not configured", http.StatusInternalServerError)
		return
	}

	// Read timeout from environment variable, with a default of 20 seconds.
	timeoutSeconds := 20
	if timeoutStr := os.Getenv("HEALTHZ_TIMEOUT_SECONDS"); timeoutStr != "" {
		if val, err := strconv.Atoi(timeoutStr); err == nil && val > 0 {
			timeoutSeconds = val
			log.Printf("Using custom healthz timeout: %d seconds", timeoutSeconds)
		} else {
			log.Printf("Invalid HEALTHZ_TIMEOUT_SECONDS value '%s'. Falling back to default of %d seconds.", timeoutStr, timeoutSeconds)
		}
	}

	// The overall timeout for the probe to complete.
	timeoutDuration := time.Duration(timeoutSeconds) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeoutDuration)
	defer cancel()

	testID := uuid.New().String()
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
		http.Error(w, "Failed to create health check request", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: "true" == os.Getenv("INSECURE_SKIP_VERIFY")},
		},
	}

	if _, err = client.Do(req); err != nil {
		log.Printf("Healthz Error: could not post to Smee server: %v", err)
		http.Error(w, "Failed to POST to Smee server", http.StatusServiceUnavailable)
		return
	}

	// Wait for the forwardHandler to receive the event, or for the timeout.
	select {
	case <-resultChan:
		log.Println("Healthz check PASSED.")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "OK")
	case <-ctx.Done():
		log.Println("Healthz check FAILED: timed out waiting for event round-trip.")
		http.Error(w, "Health check timed out", http.StatusServiceUnavailable)
	}
}

func main() {
	log.Println("Starting Smee instrumentation sidecar...")

	downstreamServiceURL := os.Getenv("DOWNSTREAM_SERVICE_URL")
	if downstreamServiceURL == "" {
		log.Fatal("FATAL: DOWNSTREAM_SERVICE_URL environment variable must be set.")
	}
	downstreamURL, err := url.Parse(downstreamServiceURL)
	if err != nil {
		log.Fatalf("FATAL: Could not parse DOWNSTREAM_SERVICE_URL: %v", err)
	}
	proxy = httputil.NewSingleHostReverseProxy(downstreamURL)

	prometheus.MustRegister(forwardAttempts)

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
	mgmtMux.HandleFunc("/healthz", healthzHandler)
	go func() {
		log.Println("Management server (metrics, healthz) listening on :9100")
		if err := http.ListenAndServe(":9100", mgmtMux); err != nil {
			log.Fatalf("FATAL: Management server failed: %v", err)
		}
	}()

	select {}
}
