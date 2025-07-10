package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

var _ = Describe("forwardHandler", func() {
	var (
		recorder           *httptest.ResponseRecorder
		mockDownstream     *httptest.Server
		downstreamRequests []*http.Request
		requestMutex       sync.Mutex
	)

	BeforeEach(func() {
		recorder = httptest.NewRecorder()
		downstreamRequests = nil

		// Create a mock downstream service
		mockDownstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestMutex.Lock()
			downstreamRequests = append(downstreamRequests, r)
			requestMutex.Unlock()
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("downstream response"))
		}))

		// Set the global downstream service URL for per-request proxy creation
		downstreamServiceURL = mockDownstream.URL

		// Reset global state
		mutex.Lock()
		healthChecks = make(map[string]chan bool)
		mutex.Unlock()

		// Reset HTTP clients for each test
		healthCheckClient = nil
		proxyInstance = nil
		healthCheckOnce = sync.Once{}
		proxyOnce = sync.Once{}
		proxyError = nil

		// Re-create the counter for each test
		forwardAttempts = prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "smee_events_relayed_total",
				Help: "Total number of regular events relayed by the sidecar.",
			},
		)
	})

	AfterEach(func() {
		if mockDownstream != nil {
			mockDownstream.Close()
		}
	})

	Describe("handling regular events", func() {
		It("should forward non-health-check events to downstream service", func() {
			payload := `{"type": "regular-event", "data": "some data"}`
			request, err := http.NewRequest("POST", "/", bytes.NewBufferString(payload))
			Expect(err).NotTo(HaveOccurred())
			request.Header.Set("Content-Type", "application/json")

			forwardHandler(recorder, request)

			Expect(recorder.Code).To(Equal(http.StatusOK))
			Expect(recorder.Body.String()).To(Equal("downstream response"))

			// Verify the request was forwarded to downstream
			requestMutex.Lock()
			Expect(len(downstreamRequests)).To(Equal(1))
			requestMutex.Unlock()

			// Verify the counter was incremented
			Expect(testutil.ToFloat64(forwardAttempts)).To(Equal(1.0))
		})

		It("should NOT set Connection: close header for regular requests", func() {
			payload := `{"type": "webhook", "action": "push", "repository": {"name": "test-repo"}}`
			request, err := http.NewRequest("POST", "/", bytes.NewBufferString(payload))
			Expect(err).NotTo(HaveOccurred())
			request.Header.Set("Content-Type", "application/json")

			forwardHandler(recorder, request)

			Expect(recorder.Code).To(Equal(http.StatusOK))
			Expect(recorder.Body.String()).To(Equal("downstream response"))

			// Verify Connection: close header is NOT set for regular requests
			connectionHeader := recorder.Header().Get("Connection")
			Expect(connectionHeader).NotTo(Equal("close"))

			// Verify the request was forwarded to downstream
			requestMutex.Lock()
			Expect(len(downstreamRequests)).To(Equal(1))
			requestMutex.Unlock()

			// Verify the counter was incremented
			Expect(testutil.ToFloat64(forwardAttempts)).To(Equal(1.0))
		})

		It("should forward non-JSON events to downstream service", func() {
			payload := "plain text event"
			request, err := http.NewRequest("POST", "/", bytes.NewBufferString(payload))
			Expect(err).NotTo(HaveOccurred())
			request.Header.Set("Content-Type", "text/plain")

			forwardHandler(recorder, request)

			Expect(recorder.Code).To(Equal(http.StatusOK))
			Expect(recorder.Body.String()).To(Equal("downstream response"))

			// Verify the request was forwarded to downstream
			requestMutex.Lock()
			Expect(len(downstreamRequests)).To(Equal(1))
			requestMutex.Unlock()

			// Verify the counter was incremented
			Expect(testutil.ToFloat64(forwardAttempts)).To(Equal(1.0))
		})

		It("should forward JSON events that are not health checks", func() {
			payload := `{"type": "webhook", "action": "push", "repository": {"name": "test-repo"}}`
			request, err := http.NewRequest("POST", "/", bytes.NewBufferString(payload))
			Expect(err).NotTo(HaveOccurred())
			request.Header.Set("Content-Type", "application/json")

			forwardHandler(recorder, request)

			Expect(recorder.Code).To(Equal(http.StatusOK))
			Expect(recorder.Body.String()).To(Equal("downstream response"))

			// Verify the request was forwarded to downstream
			requestMutex.Lock()
			Expect(len(downstreamRequests)).To(Equal(1))
			requestMutex.Unlock()

			// Verify the counter was incremented
			Expect(testutil.ToFloat64(forwardAttempts)).To(Equal(1.0))
		})
	})

	Describe("handling health check events", func() {
		It("should intercept health check events using header-based detection", func() {
			testID := "test-health-check-123"

			// Set up a waiting channel for this health check
			resultChan := make(chan bool, 1)
			mutex.Lock()
			healthChecks[testID] = resultChan
			mutex.Unlock()

			// Use header-based approach for health check detection
			payload := fmt.Sprintf(`{"type": "health-check", "id": "%s"}`, testID)
			request, err := http.NewRequest("POST", "/", bytes.NewBufferString(payload))
			Expect(err).NotTo(HaveOccurred())
			request.Header.Set("X-Health-Check-ID", testID)
			request.Header.Set("Content-Type", "application/json")

			forwardHandler(recorder, request)

			// Verify the response
			Expect(recorder.Code).To(Equal(http.StatusOK))

			// Verify the channel was signaled
			Eventually(resultChan).Should(Receive(Equal(true)))

			// Verify the health check is still in the map (cleanup happens in
			// performHealthCheck, not forwardHandler)
			mutex.Lock()
			_, exists := healthChecks[testID]
			// Clean up for the test
			delete(healthChecks, testID)
			mutex.Unlock()
			Expect(exists).To(BeTrue())

			// Verify no downstream request was made
			requestMutex.Lock()
			Expect(len(downstreamRequests)).To(Equal(0))
			requestMutex.Unlock()

			// Verify the counter was NOT incremented (health checks don't count as regular events)
			Expect(testutil.ToFloat64(forwardAttempts)).To(Equal(0.0))
		})

		It("should handle health check events when no channel is waiting", func() {
			testID := "unknown-health-check-456"

			// Use header-based approach
			payload := fmt.Sprintf(`{"type": "health-check", "id": "%s"}`, testID)
			request, err := http.NewRequest("POST", "/", bytes.NewBufferString(payload))
			Expect(err).NotTo(HaveOccurred())
			request.Header.Set("X-Health-Check-ID", testID)
			request.Header.Set("Content-Type", "application/json")

			forwardHandler(recorder, request)

			// Should still return OK even if no channel is waiting
			Expect(recorder.Code).To(Equal(http.StatusOK))

			// Verify no downstream request was made
			requestMutex.Lock()
			Expect(len(downstreamRequests)).To(Equal(0))
			requestMutex.Unlock()

			// Verify the counter was NOT incremented
			Expect(testutil.ToFloat64(forwardAttempts)).To(Equal(0.0))
		})

		It("should forward health check events without header as regular events", func() {
			// Health check JSON without header should be treated as regular event
			payload := `{"type": "health-check", "id": "test-123"}`
			request, err := http.NewRequest("POST", "/", bytes.NewBufferString(payload))
			Expect(err).NotTo(HaveOccurred())
			request.Header.Set("Content-Type", "application/json")
			// NOTE: No X-Health-Check-ID header set

			forwardHandler(recorder, request)

			// Should forward to downstream since no header present
			Expect(recorder.Code).To(Equal(http.StatusOK))
			Expect(recorder.Body.String()).To(Equal("downstream response"))

			// Verify the request was forwarded to downstream
			requestMutex.Lock()
			Expect(len(downstreamRequests)).To(Equal(1))
			requestMutex.Unlock()

			// Verify the counter was incremented
			Expect(testutil.ToFloat64(forwardAttempts)).To(Equal(1.0))
		})

		It("should forward malformed JSON as regular events", func() {
			payload := `{"type": "health-check", "id": "test-123"` // Missing closing brace
			request, err := http.NewRequest("POST", "/", bytes.NewBufferString(payload))
			Expect(err).NotTo(HaveOccurred())
			request.Header.Set("Content-Type", "application/json")
			// NOTE: No X-Health-Check-ID header set

			forwardHandler(recorder, request)

			// Should forward to downstream since no header present
			Expect(recorder.Code).To(Equal(http.StatusOK))
			Expect(recorder.Body.String()).To(Equal("downstream response"))

			// Verify the request was forwarded to downstream
			requestMutex.Lock()
			Expect(len(downstreamRequests)).To(Equal(1))
			requestMutex.Unlock()

			// Verify the counter was incremented
			Expect(testutil.ToFloat64(forwardAttempts)).To(Equal(1.0))
		})
	})

	Describe("error handling", func() {
		It("should handle proxy creation errors", func() {
			// Set an invalid downstream URL
			originalURL := downstreamServiceURL
			downstreamServiceURL = "://invalid-url"

			payload := `{"type": "regular-event", "data": "some data"}`
			request, err := http.NewRequest("POST", "/", bytes.NewBufferString(payload))
			Expect(err).NotTo(HaveOccurred())
			request.Header.Set("Content-Type", "application/json")

			forwardHandler(recorder, request)

			Expect(recorder.Code).To(Equal(http.StatusInternalServerError))
			Expect(recorder.Body.String()).To(ContainSubstring("failed to create proxy"))

			// Restore the original URL
			downstreamServiceURL = originalURL
		})
	})

	Describe("concurrent access", func() {
		It("should handle concurrent health check requests safely", func() {
			const numRequests = 10
			testIDs := make([]string, numRequests)
			resultChans := make([]chan bool, numRequests)

			// Set up multiple health check channels
			mutex.Lock()
			for i := 0; i < numRequests; i++ {
				testIDs[i] = fmt.Sprintf("concurrent-test-%d", i)
				resultChans[i] = make(chan bool, 1)
				healthChecks[testIDs[i]] = resultChans[i]
			}
			mutex.Unlock()

			// Launch concurrent requests
			done := make(chan bool, numRequests)
			for i := 0; i < numRequests; i++ {
				go func(index int) {
					defer func() { done <- true }()

					// Use BOTH headers and JSON body approach for optimal performance + compatibility
					payload := fmt.Sprintf(`{"type": "health-check", "id": "%s"}`, testIDs[index])
					request, err := http.NewRequest("POST", "/", bytes.NewBufferString(payload))
					Expect(err).NotTo(HaveOccurred())
					request.Header.Set("X-Health-Check-ID", testIDs[index])
					request.Header.Set("Content-Type", "application/json")

					recorder := httptest.NewRecorder()
					forwardHandler(recorder, request)

					Expect(recorder.Code).To(Equal(http.StatusOK))
				}(i)
			}

			// Wait for all requests to complete
			for i := 0; i < numRequests; i++ {
				<-done
			}

			// Verify all channels were signaled
			for i := 0; i < numRequests; i++ {
				Eventually(resultChans[i]).Should(Receive(Equal(true)))
			}

			// Verify all health checks are still in the map (cleanup happens in performHealthCheck, not forwardHandler)
			mutex.Lock()
			Expect(len(healthChecks)).To(Equal(numRequests))
			// Clean up for the test
			for _, testID := range testIDs {
				delete(healthChecks, testID)
			}
			mutex.Unlock()
		})
	})

	It("should force connection closure to prevent connection pooling (behavioral test)", func() {
		// Set up the global state needed for forwardHandler to work
		mutex.Lock()
		healthChecks = make(map[string]chan bool)
		mutex.Unlock()

		// Create a real HTTP server using the actual forwardHandler
		testServer := httptest.NewServer(http.HandlerFunc(forwardHandler))
		defer testServer.Close()

		connections := measureConnectionBehaviorWithHealthChecks(testServer.URL, 5)

		// Log the results
		GinkgoWriter.Printf("forwardHandler created %d connections for 5 health check requests\n", connections)

		// We should see more connections being created because
		// Connection: close prevents connection reuse
		// If the fix is working, we should see >= 3 connections for 5 requests
		// If the fix is broken, we'd see only 1-2 connections due to reuse
		Expect(connections).To(BeNumerically(">=", 3),
			"forwardHandler should prevent connection reuse for health checks")
	})
})

func TestForwardHandler_NonHealthCheckRequest(t *testing.T) {
	// Create a test downstream server
	downstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify the request was forwarded correctly (accept trailing slash)
		if r.URL.Path != "/webhook" && r.URL.Path != "/webhook/" {
			t.Errorf("Expected path /webhook or /webhook/, got %s", r.URL.Path)
		}

		// Read and verify the body was forwarded correctly
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("Failed to read forwarded body: %v", err)
		}

		expectedBody := `{"type":"webhook","data":"test"}`
		if string(body) != expectedBody {
			t.Errorf("Expected body %s, got %s", expectedBody, string(body))
		}

		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer downstreamServer.Close()

	// Set the downstream service URL
	downstreamServiceURL = downstreamServer.URL + "/webhook"

	// Create a test request (non-health-check)
	requestBody := `{"type":"webhook","data":"test"}`
	req := httptest.NewRequest("POST", "/", strings.NewReader(requestBody))
	req.Header.Set("Content-Type", "application/json")
	// Note: No X-Health-Check-ID header, so it should be forwarded

	// Create a response recorder
	w := httptest.NewRecorder()

	// Call the handler
	forwardHandler(w, req)

	// Verify the response
	if w.Code != http.StatusOK {
		t.Errorf("Expected status code %d, got %d", http.StatusOK, w.Code)
	}

	if w.Body.String() != "ok" {
		t.Errorf("Expected response body 'ok', got %s", w.Body.String())
	}
}

// measureConnectionBehaviorWithHealthChecks measures how many connections are created
// when making health check requests to the server
func measureConnectionBehaviorWithHealthChecks(serverURL string, requestCount int) int {
	var connectionCount int32

	// Create a custom transport to count connections
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			atomic.AddInt32(&connectionCount, 1)
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
		DisableKeepAlives: false,
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   time.Second * 5,
	}

	// Make multiple health check requests with the X-Health-Check-ID header
	for i := 0; i < requestCount; i++ {
		req, err := http.NewRequest("GET", serverURL, nil)
		if err != nil {
			continue
		}

		// Add health check header to trigger our fix
		req.Header.Set("X-Health-Check-ID", fmt.Sprintf("test-%d", i))

		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		resp.Body.Close()

		// Brief pause to ensure connections are properly handled
		time.Sleep(10 * time.Millisecond)
	}

	return int(atomic.LoadInt32(&connectionCount))
}
