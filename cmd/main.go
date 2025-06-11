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

	"github.com/donovanhide/eventsource"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	// Globals for client-check mode
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

// ==========================================================================================
// CLIENT-CHECK MODE LOGIC (Unchanged from last working version)
// ==========================================================================================
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
			healthCheckIDs[healthCheck.ID] = true
			mutex.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}
	}
	forwardAttempts.Inc()
	r.Body = io.NopCloser(bytes.NewReader(body))
	proxy.ServeHTTP(w, r)
}

func runClientHealthCheckLoop(smeeChannelURL string) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	healthFilePath := "/tmp/health/live"

	runCheck := func() {
		testID := uuid.New().String()
		payload := HealthCheckPayload{Type: "health-check", ID: testID}
		payloadBytes, _ := json.Marshal(payload)
		req, err := http.NewRequestWithContext(context.Background(), "POST", smeeChannelURL, bytes.NewBuffer(payloadBytes))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		client := &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: "true" == os.Getenv("INSECURE_SKIP_VERIFY")},
			},
		}
		if _, err = client.Do(req); err != nil {
			os.Remove(healthFilePath)
			return
		}

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
					delete(healthCheckIDs, testID)
					found = true
				}
				mutex.Unlock()
				if found {
					break checkLoop
				}
				time.Sleep(100 * time.Millisecond)
			}
		}

		if found {
			timestamp := fmt.Sprintf("%d", time.Now().Unix())
			os.WriteFile(healthFilePath, []byte(timestamp), 0644)
		} else {
			os.Remove(healthFilePath)
		}
	}
	runCheck()
	for range ticker.C {
		runCheck()
	}
}

func runClientCheckMode() {
	log.Println("Starting sidecar in [client-check] mode...")
	downstreamServiceURL := os.Getenv("DOWNSTREAM_SERVICE_URL")
	smeeChannelURL := os.Getenv("SMEE_CHANNEL_URL")
	if downstreamServiceURL == "" || smeeChannelURL == "" {
		log.Fatal("FATAL: DOWNSTREAM_SERVICE_URL and SMEE_CHANNEL_URL env vars must be set for client-check mode.")
	}
	downstreamURL, err := url.Parse(downstreamServiceURL)
	if err != nil {
		log.Fatalf("FATAL: Could not parse DOWNSTREAM_SERVICE_URL: %v", err)
	}
	proxy = httputil.NewSingleHostReverseProxy(downstreamURL)
	prometheus.MustRegister(forwardAttempts)
	go runClientHealthCheckLoop(smeeChannelURL)
	relayMux := http.NewServeMux()
	relayMux.HandleFunc("/", forwardHandler)
	go func() {
		log.Println("Relay server listening on :8080")
		http.ListenAndServe(":8080", relayMux)
	}()
	mgmtMux := http.NewServeMux()
	mgmtMux.Handle("/metrics", promhttp.Handler())
	go func() {
		log.Println("Management server (metrics) listening on :9100")
		http.ListenAndServe(":9100", mgmtMux)
	}()
	select {}
}

// ==========================================================================================
// SERVER-CHECK MODE LOGIC (Final Simplified "Listen Only" Version)
// ==========================================================================================

// listenForHeartbeat is a dedicated function for the goroutine. Its only job is to listen.
func listenForHeartbeat(ctx context.Context, stream *eventsource.Stream, resultChan chan<- bool) {
	// The stream is created and closed by the parent function. This goroutine just reads from it.
	for {
		select {
		case <-ctx.Done(): // Context was cancelled by the parent function.
			return
		case ev := <-stream.Events:
			log.Printf("Server Health Check: Received initial heartbeat event from server. Type: %s", ev.Event())
			// Send success once and only once. A non-blocking send is safest.
			select {
			case resultChan <- true:
			default:
			}
			// Important: We don't return here. We let the context cancellation handle shutdown.
		case err := <-stream.Errors:
			if err != io.EOF {
				log.Printf("Server Health Check Error: Eventsource client received an error: %v", err)
			}
			return
		}
	}
}

func runServerHealthCheckLoop(smeeServerURL string) {
	healthFilePath := "/tmp/health/live"
	var lastSuccessTime time.Time

	const healthCheckChannel = "smeehealthcheckchannel"
	channelURL := fmt.Sprintf("%s/%s", smeeServerURL, healthCheckChannel)

	// This is now a simple, infinite loop that controls the timing of checks.
	for {
		log.Println("Server Health Check: Running self-test...")
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)

		// 1. Create the request with a context that the library will use.
		req, err := http.NewRequestWithContext(ctx, "GET", channelURL, nil)
		if err != nil {
			log.Printf("Server Health Check Error: Failed to create request: %v", err)
			cancel()
			time.Sleep(30 * time.Second)
			continue
		}

		// 2. Subscribe to the stream. The stream object is now owned by this function.
		stream, err := eventsource.SubscribeWithRequest("", req)
		if err != nil {
			log.Printf("Server Health Check Error: Eventsource client failed to subscribe: %v", err)
			cancel()
			time.Sleep(30 * time.Second)
			continue
		}
		// The stream will be closed reliably when the check function exits.
		defer stream.Close()
		log.Printf("Server Health Check: Subscribing to fixed channel URL: %s", channelURL)

		// 3. Launch the simple listener goroutine.
		resultChan := make(chan bool, 1)
		go listenForHeartbeat(ctx, stream, resultChan)

		// 4. Wait for the goroutine to signal success, or for our overall timeout.
		found := false
		select {
		case found = <-resultChan:
		case <-ctx.Done():
			log.Printf("Server Health Check: Timed out waiting for any event from server. Error: %v", ctx.Err())
		}

		// 5. Explicitly cancel the context to signal the goroutine to exit.
		cancel()

		// 6. Update health file based on the result.
		if found {
			log.Println("Server Health Check: PASSED.")
			lastSuccessTime = time.Now()
		} else {
			log.Println("Server Health Check: FAILED.")
		}

		if !lastSuccessTime.IsZero() && time.Since(lastSuccessTime) < 90*time.Second {
			log.Printf("Server Health status is GOOD. Updating heartbeat file.")
			timestamp := fmt.Sprintf("%d", time.Now().Unix())
			if err := os.WriteFile(healthFilePath, []byte(timestamp), 0644); err != nil {
				log.Printf("Health Check Warning: could not write to heartbeat file: %v", err)
			}
		} else {
			log.Printf("Server Health status is BAD. Removing heartbeat file.")
			os.Remove(healthFilePath)
		}

		// Cooldown period between checks.
		log.Println("Health check cycle finished. Cooldown for 30 seconds...")
		time.Sleep(30 * time.Second)
	}
}

func runServerCheckMode() {
	log.Println("Starting sidecar in [server-check] mode...")
	smeeServerURL := os.Getenv("SMEE_SERVER_URL")
	if smeeServerURL == "" {
		log.Fatal("FATAL: SMEE_SERVER_URL env var must be set for server-check mode (e.g., http://localhost:3333).")
	}
	runServerHealthCheckLoop(smeeServerURL)
}

// ==========================================================================================
// MAIN: Dispatcher
// ==========================================================================================
func main() {
	if len(os.Args) < 2 {
		log.Println("Error: Missing subcommand. Please specify 'client-check' or 'server-check'.")
		os.Exit(1)
	}
	switch os.Args[1] {
	case "client-check":
		runClientCheckMode()
	case "server-check":
		runServerCheckMode()
	default:
		log.Printf("Error: Unknown subcommand '%s'. Please specify 'client-check' or 'server-check'.", os.Args[1])
		os.Exit(1)
	}
}
