package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("healthzHandler", func() {
	var (
		recorder       *httptest.ResponseRecorder
		request        *http.Request
		mockSmeeServer *httptest.Server
	)

	BeforeEach(func() {
		recorder = httptest.NewRecorder()
		request, _ = http.NewRequest("GET", "/healthz", nil)

		// Reset global state before each test to ensure isolation.
		mutex.Lock()
		healthChecks = make(map[string]chan bool)
		mutex.Unlock()
	})

	AfterEach(func() {
		// Ensure mock server is closed if it was created.
		if mockSmeeServer != nil {
			mockSmeeServer.Close()
		}
		// Clean up environment variables after each test.
		os.Unsetenv("SMEE_CHANNEL_URL")
		os.Unsetenv("HEALTHZ_TIMEOUT_SECONDS")
	})

	Context("when the health check round-trip is successful", func() {
		BeforeEach(func() {
			// This mock server simulates the Smee.io relay. When it gets the probe,
			// it immediately finds the waiting channel and sends the success signal,
			// just like the real forwardHandler would.
			mockSmeeServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload HealthCheckPayload
				err := json.NewDecoder(r.Body).Decode(&payload)
				Expect(err).NotTo(HaveOccurred())

				mutex.Lock()
				// Find the channel registered by healthzHandler and signal it.
				if ch, ok := healthChecks[payload.ID]; ok {
					ch <- true
				}
				mutex.Unlock()

				w.WriteHeader(http.StatusOK)
			}))

			// Configure the handler to use our mock server.
			os.Setenv("SMEE_CHANNEL_URL", mockSmeeServer.URL)
		})

		It("should return a 200 OK status", func() {
			healthzHandler(recorder, request)
			Expect(recorder.Code).To(Equal(http.StatusOK))
			Expect(recorder.Body.String()).To(ContainSubstring("OK"))
		})
	})

	Context("when the health check times out", func() {
		BeforeEach(func() {
			// This mock server simulates a broken or slow Smee relay.
			// It receives the request but NEVER sends the signal back, forcing a timeout.
			mockSmeeServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Do nothing to complete the loop.
				w.WriteHeader(http.StatusAccepted)
			}))

			os.Setenv("SMEE_CHANNEL_URL", mockSmeeServer.URL)
			// Set a very short timeout to make the test run quickly.
			os.Setenv("HEALTHZ_TIMEOUT_SECONDS", "1")
		})

		It("should return a 503 Service Unavailable status", func() {
			healthzHandler(recorder, request)
			Expect(recorder.Code).To(Equal(http.StatusServiceUnavailable))
			Expect(recorder.Body.String()).To(ContainSubstring("Health check timed out"))
		})
	})

	Context("when SMEE_CHANNEL_URL is not configured", func() {
		BeforeEach(func() {
			// Ensure the required environment variable is not set.
			os.Unsetenv("SMEE_CHANNEL_URL")
		})

		It("should return a 500 Internal Server Error", func() {
			healthzHandler(recorder, request)
			Expect(recorder.Code).To(Equal(http.StatusInternalServerError))
			Expect(recorder.Body.String()).To(ContainSubstring("Sidecar not configured"))
		})
	})
})
