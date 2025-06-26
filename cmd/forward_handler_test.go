package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync"

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

		// Set up the proxy to point to our mock downstream
		downstreamURL, err := url.Parse(mockDownstream.URL)
		Expect(err).NotTo(HaveOccurred())
		proxy = httputil.NewSingleHostReverseProxy(downstreamURL)

		// Reset global state
		mutex.Lock()
		healthChecks = make(map[string]chan bool)
		mutex.Unlock()

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
		It("should intercept health check events and signal waiting channel", func() {
			testID := "test-health-check-123"

			// Set up a waiting channel for this health check
			resultChan := make(chan bool, 1)
			mutex.Lock()
			healthChecks[testID] = resultChan
			mutex.Unlock()

			payload := HealthCheckPayload{
				Type: "health-check",
				ID:   testID,
			}
			payloadBytes, err := json.Marshal(payload)
			Expect(err).NotTo(HaveOccurred())

			request, err := http.NewRequest("POST", "/", bytes.NewBuffer(payloadBytes))
			Expect(err).NotTo(HaveOccurred())
			request.Header.Set("Content-Type", "application/json")

			forwardHandler(recorder, request)

			// Verify the response
			Expect(recorder.Code).To(Equal(http.StatusOK))

			// Verify the channel was signaled
			Eventually(resultChan).Should(Receive(Equal(true)))

			// Verify the health check was cleaned up from the map
			mutex.Lock()
			_, exists := healthChecks[testID]
			mutex.Unlock()
			Expect(exists).To(BeFalse())

			// Verify no downstream request was made
			requestMutex.Lock()
			Expect(len(downstreamRequests)).To(Equal(0))
			requestMutex.Unlock()

			// Verify the counter was NOT incremented (health checks don't count as regular events)
			Expect(testutil.ToFloat64(forwardAttempts)).To(Equal(0.0))
		})

		It("should handle health check events when no channel is waiting", func() {
			testID := "unknown-health-check-456"

			payload := HealthCheckPayload{
				Type: "health-check",
				ID:   testID,
			}
			payloadBytes, err := json.Marshal(payload)
			Expect(err).NotTo(HaveOccurred())

			request, err := http.NewRequest("POST", "/", bytes.NewBuffer(payloadBytes))
			Expect(err).NotTo(HaveOccurred())
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

		It("should handle malformed JSON gracefully", func() {
			payload := `{"type": "health-check", "id": "test-123"` // Missing closing brace
			request, err := http.NewRequest("POST", "/", bytes.NewBufferString(payload))
			Expect(err).NotTo(HaveOccurred())
			request.Header.Set("Content-Type", "application/json")

			forwardHandler(recorder, request)

			// Should forward to downstream since JSON parsing failed
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
		It("should handle request body read errors", func() {
			// Create a request with a body that will cause an error when read
			request, err := http.NewRequest("POST", "/", nil)
			Expect(err).NotTo(HaveOccurred())
			request.Body = &errorReader{}

			forwardHandler(recorder, request)

			Expect(recorder.Code).To(Equal(http.StatusInternalServerError))
			Expect(recorder.Body.String()).To(ContainSubstring("cannot read request body"))
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

					payload := HealthCheckPayload{
						Type: "health-check",
						ID:   testIDs[index],
					}
					payloadBytes, err := json.Marshal(payload)
					Expect(err).NotTo(HaveOccurred())

					request, err := http.NewRequest("POST", "/", bytes.NewBuffer(payloadBytes))
					Expect(err).NotTo(HaveOccurred())
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

			// Verify all health checks were cleaned up
			mutex.Lock()
			Expect(len(healthChecks)).To(Equal(0))
			mutex.Unlock()
		})
	})
})

// errorReader is a helper type that always returns an error when read
type errorReader struct{}

func (e *errorReader) Read(p []byte) (n int, err error) {
	return 0, fmt.Errorf("simulated read error")
}

func (e *errorReader) Close() error {
	return nil
}
