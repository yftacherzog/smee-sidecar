package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

var _ = Describe("forwardHandler", func() {
	var (
		// recorder captures the HTTP response written by the handler.
		recorder *httptest.ResponseRecorder
		// downstreamServer simulates the real downstream service that receives webhooks.
		downstreamServer *httptest.Server
		// downstreamCalled is a flag to check if the downstream server was contacted.
		downstreamCalled bool
	)

	// BeforeEach runs before each "It" block, setting up a clean state for every test.
	BeforeEach(func() {
		// Reset our flag.
		downstreamCalled = false
		// Set up a mock downstream server for this test.
		downstreamServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			downstreamCalled = true // Mark that this server was called.
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("OK"))
		}))

		// Configure the global reverse proxy to point to our mock server.
		downstreamURL, err := url.Parse(downstreamServer.URL)
		Expect(err).NotTo(HaveOccurred())
		proxy = httputil.NewSingleHostReverseProxy(downstreamURL)

		// Reset the state for each test.
		recorder = httptest.NewRecorder()
		mutex.Lock()
		healthChecks = make(map[string]chan bool)
		mutex.Unlock()

		// Re-create the counter before each test to ensure test isolation.
		// This prevents state from one test leaking into another.
		forwardAttempts = prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "smee_events_relayed_total",
				Help: "Total number of regular events relayed by the sidecar.",
			},
		)
	})

	// AfterEach runs after each test to clean up resources.
	AfterEach(func() {
		downstreamServer.Close()
	})

	Context("when receiving a regular event", func() {
		var request *http.Request

		BeforeEach(func() {
			// Create a simple POST request that is not a health check.
			reqBody := bytes.NewBufferString("some webhook data")
			request, _ = http.NewRequest("POST", "/", reqBody)
			request.Header.Set("Content-Type", "text/plain")
		})

		It("should proxy the request to the downstream service", func() {
			forwardHandler(recorder, request)
			// Expect that our mock downstream service was contacted.
			Expect(downstreamCalled).To(BeTrue())
			// Expect that the handler passed the downstream's "200 OK" back to the caller.
			Expect(recorder.Code).To(Equal(http.StatusOK))
		})

		It("should increment the forwardAttempts counter", func() {
			// Get the value of the counter before the handler is called.
			before := testutil.ToFloat64(forwardAttempts)
			Expect(before).To(BeZero()) // We can now assert it starts at 0.
			forwardHandler(recorder, request)
			// Get the value after and assert that it increased by one.
			after := testutil.ToFloat64(forwardAttempts)
			Expect(after).To(Equal(before + 1))
		})
	})

	Context("when receiving a valid health check event", func() {
		var (
			testID     string
			resultChan chan bool
			request    *http.Request
		)

		BeforeEach(func() {
			testID = "test-id-123"
			// The channel must be buffered to prevent the sender from blocking.
			resultChan = make(chan bool, 1)

			// Manually add an entry to the map, simulating a pending health check.
			mutex.Lock()
			healthChecks[testID] = resultChan
			mutex.Unlock()

			// Create the specific health check JSON payload.
			payload := HealthCheckPayload{Type: "health-check", ID: testID}
			body, _ := json.Marshal(payload)
			request, _ = http.NewRequest("POST", "/", bytes.NewBuffer(body))
			request.Header.Set("Content-Type", "application/json")
		})

		It("should intercept the event and NOT proxy it", func() {
			forwardHandler(recorder, request)
			// Assert that the downstream service was never contacted.
			Expect(downstreamCalled).To(BeFalse())
		})

		It("should return a 200 OK status", func() {
			forwardHandler(recorder, request)
			Expect(recorder.Code).To(Equal(http.StatusOK))
		})

		It("should send a signal on the correct channel", func() {
			forwardHandler(recorder, request)
			// Gomega's "Receive" matcher checks if a value comes through the channel.
			// It has a default timeout of 1 second.
			Expect(resultChan).To(Receive())
		})

		It("should clean up the entry from the healthChecks map", func() {
			forwardHandler(recorder, request)
			mutex.Lock()
			// Assert that the key for our test ID has been deleted.
			Expect(healthChecks).NotTo(HaveKey(testID))
			mutex.Unlock()
		})
	})

	Context("when receiving an orphaned health check event", func() {
		It("should handle it gracefully without proxying or panicking", func() {
			// Create a health check payload for an ID that is NOT in our map.
			payload := HealthCheckPayload{Type: "health-check", ID: "orphan-id-456"}
			body, _ := json.Marshal(payload)
			request, _ := http.NewRequest("POST", "/", bytes.NewBuffer(body))
			request.Header.Set("Content-Type", "application/json")

			// We need to use a function to wrap the handler call so Gomega can check for panics.
			handlerFunc := func() {
				forwardHandler(recorder, request)
			}

			// Assert that the function does not panic.
			Expect(handlerFunc).ShouldNot(Panic())
			// Assert that the request was not forwarded.
			Expect(downstreamCalled).To(BeFalse())
			// Assert that it still returns OK.
			Expect(recorder.Code).To(Equal(http.StatusOK))
		})
	})
})
