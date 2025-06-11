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
	healthCheckIDs = make(map[string]bool)
	mutex          = &sync.Mutex{}
	proxy          *httputil.ReverseProxy
)

type HealthCheckPayload struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// forwardHandler proxies requests and intercepts health check events.
func forwardHandler(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("Error reading request body: %v", err)
		http.Error(w, "cannot read request body", http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()

	var healthCheck HealthCheckPayload
	if r.Header.Get("Content-Type") == "application/json" {
		if json.Unmarshal(body, &healthCheck) == nil && healthCheck.Type == "health-check" {
			mutex.Lock()
			healthCheckIDs[healthCheck.ID] = true
			mutex.Unlock()
			log.Printf("Health Check: Intercepted and verified health check event with ID: %s", healthCheck.ID)
			w.WriteHeader(http.StatusOK)
			return
		}
	}

	forwardAttempts.Inc()
	log.Printf("Relaying regular event via ReverseProxy to downstream service")
	// We need to put the body back on the request for the proxy to read it.
	r.Body = io.NopCloser(bytes.NewReader(body))
	proxy.ServeHTTP(w, r)
}

// runClientHealthCheckLoop performs the self-test for the smee-client.
func runClientHealthCheckLoop(smeeChannelURL string) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	healthFilePath := "/tmp/health/live"

	runCheck := func() {
		log.Println("Health Check: Running self-test...")
		testID := uuid.New().String()
		payload := HealthCheckPayload{Type: "health-check", ID: testID}
		payloadBytes, _ := json.Marshal(payload)

		// Create the request to the external Smee channel
		req, err := http.NewRequestWithContext(context.Background(), "POST", smeeChannelURL, bytes.NewBuffer(payloadBytes))
		if err != nil {
			log.Printf("Health Check Error: could not create request: %v", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")

		// This client needs to handle potential corporate proxies
		client := &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: "true" == os.Getenv("INSECURE_SKIP_VERIFY")},
			},
		}

		// Post the event
		if _, err = client.Do(req); err != nil {
			log.Printf("Health Check Error: could not post to Smee server: %v", err)
			os.Remove(healthFilePath) // Fail fast if POST fails
			return
		}

		// Wait for the event to be received by our own forwardHandler
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		found := false
	checkLoop:
		for {
			select {
			case <-ctx.Done():
				break checkLoop
			default:
				mutex.Lock()
				if healthCheckIDs[testID] {
					delete(healthCheckIDs, testID) // Clean up the ID
					found = true
				}
				mutex.Unlock()
				if found {
					break checkLoop
				}
				time.Sleep(100 * time.Millisecond)
			}
		}

		// Update the shared health file based on the result
		if found {
			log.Println("Health Check: PASSED.")
			timestamp := fmt.Sprintf("%d", time.Now().Unix())
			if err := os.WriteFile(healthFilePath, []byte(timestamp), 0644); err != nil {
				log.Printf("Health Check Warning: could not write to heartbeat file: %v", err)
			}
		} else {
			log.Println("Health Check: FAILED.")
			os.Remove(healthFilePath)
		}
	}

	runCheck()
	for range ticker.C {
		runCheck()
	}
}

// main function now directly starts the client-check sidecar.
func main() {
	log.Println("Starting Smee instrumentation sidecar...")

	// --- Load Configuration ---
	downstreamServiceURL := os.Getenv("DOWNSTREAM_SERVICE_URL")
	smeeChannelURL := os.Getenv("SMEE_CHANNEL_URL")
	if downstreamServiceURL == "" || smeeChannelURL == "" {
		log.Fatal("FATAL: DOWNSTREAM_SERVICE_URL and SMEE_CHANNEL_URL environment variables must be set.")
	}

	// --- Initialize the ReverseProxy ---
	downstreamURL, err := url.Parse(downstreamServiceURL)
	if err != nil {
		log.Fatalf("FATAL: Could not parse DOWNSTREAM_SERVICE_URL: %v", err)
	}
	proxy = httputil.NewSingleHostReverseProxy(downstreamURL)

	// --- Register Prometheus Metrics ---
	prometheus.MustRegister(forwardAttempts)

	// --- Start the background health check loop ---
	go runClientHealthCheckLoop(smeeChannelURL)

	// --- Start Relay Server (on port 8080) ---
	relayMux := http.NewServeMux()
	relayMux.HandleFunc("/", forwardHandler)
	go func() {
		log.Println("Relay server listening on :8080")
		if err := http.ListenAndServe(":8080", relayMux); err != nil {
			log.Fatalf("FATAL: Relay server failed: %v", err)
		}
	}()

	// --- Start Management Server (on port 9100) ---
	mgmtMux := http.NewServeMux()
	mgmtMux.Handle("/metrics", promhttp.Handler())
	go func() {
		log.Println("Management server (metrics) listening on :9100")
		if err := http.ListenAndServe(":9100", mgmtMux); err != nil {
			log.Fatalf("FATAL: Management server failed: %v", err)
		}
	}()

	// Keep the main goroutine alive forever
	select {}
}
