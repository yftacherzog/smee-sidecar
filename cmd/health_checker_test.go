package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

var _ = Describe("Health Checker", func() {
	var (
		tempDir        string
		healthFilePath string
		mockServer     *httptest.Server
	)

	BeforeEach(func() {
		// Create a temporary directory for tests
		var err error
		tempDir, err = os.MkdirTemp("", "smee-test-*")
		Expect(err).NotTo(HaveOccurred())

		healthFilePath = filepath.Join(tempDir, "health-status.txt")

		// Reset global state
		mutex.Lock()
		healthChecks = make(map[string]chan bool)
		mutex.Unlock()

		// Re-create the gauge for each test
		health_check = prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "health_check",
				Help: "Indicates the outcome of the last completed health check (1 for OK, 0 for failure).",
			},
		)
	})

	AfterEach(func() {
		if mockServer != nil {
			mockServer.Close()
		}
		os.RemoveAll(tempDir)
	})

	Describe("writeHealthStatus", func() {
		It("should write health status to file in correct format", func() {
			status := &HealthStatus{
				Status:  "success",
				Message: "Health check completed successfully",
			}

			err := writeHealthStatus(status, healthFilePath)
			Expect(err).NotTo(HaveOccurred())

			content, err := os.ReadFile(healthFilePath)
			Expect(err).NotTo(HaveOccurred())

			expectedContent := "status=success\nmessage=Health check completed successfully\n"
			Expect(string(content)).To(Equal(expectedContent))
		})

		It("should handle failure status", func() {
			status := &HealthStatus{
				Status:  "failure",
				Message: "Health check failed",
			}

			err := writeHealthStatus(status, healthFilePath)
			Expect(err).NotTo(HaveOccurred())

			content, err := os.ReadFile(healthFilePath)
			Expect(err).NotTo(HaveOccurred())

			Expect(string(content)).To(ContainSubstring("status=failure"))
			Expect(string(content)).To(ContainSubstring("message=Health check failed"))
		})

		It("should write health status to file correctly", func() {
			// This test ensures health status is written to file properly
			status := &HealthStatus{
				Status:  "success",
				Message: "First write",
			}

			err := writeHealthStatus(status, healthFilePath)
			Expect(err).NotTo(HaveOccurred())

			// Update with new status
			status.Message = "Second write"
			err = writeHealthStatus(status, healthFilePath)
			Expect(err).NotTo(HaveOccurred())

			content, err := os.ReadFile(healthFilePath)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(content)).To(ContainSubstring("Second write"))

			// Ensure no temp file is left behind
			tempFile := healthFilePath + ".tmp"
			_, err = os.Stat(tempFile)
			Expect(os.IsNotExist(err)).To(BeTrue())
		})
	})

	Describe("writeScriptsToVolume", func() {
		It("should write probe scripts to shared volume", func() {
			err := writeScriptsToVolume(tempDir)
			Expect(err).NotTo(HaveOccurred())

			// Check that all scripts were created
			smeeScriptPath := filepath.Join(tempDir, "check-smee-health.sh")
			sidecarScriptPath := filepath.Join(tempDir, "check-sidecar-health.sh")
			fileAgeScriptPath := filepath.Join(tempDir, "check-file-age.sh")

			_, err = os.Stat(smeeScriptPath)
			Expect(err).NotTo(HaveOccurred())

			_, err = os.Stat(sidecarScriptPath)
			Expect(err).NotTo(HaveOccurred())

			_, err = os.Stat(fileAgeScriptPath)
			Expect(err).NotTo(HaveOccurred())

			// Check that scripts are executable but read-only (0555)
			smeeInfo, err := os.Stat(smeeScriptPath)
			Expect(err).NotTo(HaveOccurred())
			// Extracts just the file permission bits from the file mode
			Expect(smeeInfo.Mode() & 0777).To(Equal(os.FileMode(0555)))

			sidecarInfo, err := os.Stat(sidecarScriptPath)
			Expect(err).NotTo(HaveOccurred())
			Expect(sidecarInfo.Mode() & 0777).To(Equal(os.FileMode(0555)))

			fileAgeInfo, err := os.Stat(fileAgeScriptPath)
			Expect(err).NotTo(HaveOccurred())
			Expect(fileAgeInfo.Mode() & 0777).To(Equal(os.FileMode(0555)))
		})

		It("should write valid script content", func() {
			err := writeScriptsToVolume(tempDir)
			Expect(err).NotTo(HaveOccurred())

			// Check that the smee health script contains expected content
			smeeScriptPath := filepath.Join(tempDir, "check-smee-health.sh")
			content, err := os.ReadFile(smeeScriptPath)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(content)).To(ContainSubstring("#!/bin/bash"))
			Expect(string(content)).To(ContainSubstring("health-status"))
		})

		It("should handle overwriting existing read-only scripts", func() {
			// First call to create the scripts
			err := writeScriptsToVolume(tempDir)
			Expect(err).NotTo(HaveOccurred())

			// Verify scripts exist and are read-only
			smeeScriptPath := filepath.Join(tempDir, "check-smee-health.sh")
			sidecarScriptPath := filepath.Join(tempDir, "check-sidecar-health.sh")
			fileAgeScriptPath := filepath.Join(tempDir, "check-file-age.sh")

			smeeInfo, err := os.Stat(smeeScriptPath)
			Expect(err).NotTo(HaveOccurred())
			Expect(smeeInfo.Mode() & 0777).To(Equal(os.FileMode(0555)))

			sidecarInfo, err := os.Stat(sidecarScriptPath)
			Expect(err).NotTo(HaveOccurred())
			Expect(sidecarInfo.Mode() & 0777).To(Equal(os.FileMode(0555)))

			fileAgeInfo, err := os.Stat(fileAgeScriptPath)
			Expect(err).NotTo(HaveOccurred())
			Expect(fileAgeInfo.Mode() & 0777).To(Equal(os.FileMode(0555)))

			// Second call should succeed even with read-only files
			// This simulates what happens when a container restarts and tries to recreate scripts
			err = writeScriptsToVolume(tempDir)
			Expect(err).NotTo(HaveOccurred(),
				"Second call to writeScriptsToVolume should succeed even with existing read-only files")

			// Verify scripts are still readable and executable after overwrite
			smeeInfo2, err := os.Stat(smeeScriptPath)
			Expect(err).NotTo(HaveOccurred())
			Expect(smeeInfo2.Mode() & 0777).To(Equal(os.FileMode(0555)))

			sidecarInfo2, err := os.Stat(sidecarScriptPath)
			Expect(err).NotTo(HaveOccurred())
			Expect(sidecarInfo2.Mode() & 0777).To(Equal(os.FileMode(0555)))

			fileAgeInfo2, err := os.Stat(fileAgeScriptPath)
			Expect(err).NotTo(HaveOccurred())
			Expect(fileAgeInfo2.Mode() & 0777).To(Equal(os.FileMode(0555)))

			// Verify content is still valid
			content, err := os.ReadFile(smeeScriptPath)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(content)).To(ContainSubstring("#!/bin/bash"))
			Expect(string(content)).To(ContainSubstring("health-status"))
		})
	})

	Describe("performHealthCheck", func() {
		Context("when health check succeeds", func() {
			BeforeEach(func() {
				// Mock server that simulates successful round-trip
				mockServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var payload HealthCheckPayload
					body, err := io.ReadAll(r.Body)
					Expect(err).NotTo(HaveOccurred())

					err = json.Unmarshal(body, &payload)
					Expect(err).NotTo(HaveOccurred())

					// Simulate the forwardHandler behavior
					mutex.Lock()
					if ch, ok := healthChecks[payload.ID]; ok {
						go func() {
							ch <- true
						}()
					}
					mutex.Unlock()

					w.WriteHeader(http.StatusOK)
				}))
			})

			It("should return success status", func() {
				status := performHealthCheck(mockServer.URL, 5)
				Expect(status.Status).To(Equal("success"))
				Expect(status.Message).To(Equal("Health check completed successfully"))
			})
		})

		Context("when health check times out", func() {
			BeforeEach(func() {
				// Mock server that never responds with the expected signal
				mockServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					// Accept the request but don't signal the channel
					w.WriteHeader(http.StatusOK)
				}))
			})

			It("should return failure status due to timeout", func() {
				status := performHealthCheck(mockServer.URL, 1) // 1 second timeout
				Expect(status.Status).To(Equal("failure"))
				Expect(status.Message).To(ContainSubstring("Health check timed out"))
			})
		})

		Context("when server is unreachable", func() {
			It("should return failure status", func() {
				status := performHealthCheck("http://localhost:99999", 5) // Invalid URL
				Expect(status.Status).To(Equal("failure"))
				Expect(status.Message).To(ContainSubstring("Failed to POST to smee server"))
			})
		})
	})

	Describe("runHealthChecker", func() {
		Context("when running background health checker", func() {
			It("should perform health checks at regular intervals", func() {
				// Mock server for testing
				requestCount := 0
				mockServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requestCount++

					var payload HealthCheckPayload
					body, err := io.ReadAll(r.Body)
					if err == nil {
						json.Unmarshal(body, &payload)
						mutex.Lock()
						if ch, ok := healthChecks[payload.ID]; ok {
							go func() {
								ch <- true
							}()
						}
						mutex.Unlock()
					}

					w.WriteHeader(http.StatusOK)
				}))

				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()

				// Start the health checker with a very short interval
				go runHealthChecker(ctx, mockServer.URL, healthFilePath, 1, 5) // 1 second interval

				// Wait for a few health checks to complete
				Eventually(func() int {
					return requestCount
				}, time.Second*3, time.Millisecond*100).Should(BeNumerically(">=", 2))

				// Check that health status file was created and updated
				Eventually(func() bool {
					_, err := os.Stat(healthFilePath)
					return err == nil
				}, time.Second*2, time.Millisecond*100).Should(BeTrue())

				// Check that the file contains success status
				Eventually(func() string {
					content, err := os.ReadFile(healthFilePath)
					if err != nil {
						return ""
					}
					return string(content)
				}, time.Second*2, time.Millisecond*100).Should(ContainSubstring("status=success"))

				// Check that the Prometheus metric is set correctly
				Eventually(func() float64 {
					return testutil.ToFloat64(health_check)
				}, time.Second*2, time.Millisecond*100).Should(Equal(1.0))

				// Cancel the context to stop the health checker
				cancel()
			})

			It("should handle health check failures and update metrics", func() {
				// Mock server that causes timeouts
				mockServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					// Don't signal the channel - this will cause timeouts
					w.WriteHeader(http.StatusOK)
				}))

				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()

				// Start the health checker with short timeout
				go runHealthChecker(ctx, mockServer.URL, healthFilePath, 1, 1) // 1 second interval, 1 second timeout

				// Wait for health check to fail
				Eventually(func() string {
					content, err := os.ReadFile(healthFilePath)
					if err != nil {
						return ""
					}
					return string(content)
				}, time.Second*3, time.Millisecond*100).Should(ContainSubstring("status=failure"))

				// Check that the Prometheus metric is set correctly for failure
				Eventually(func() float64 {
					return testutil.ToFloat64(health_check)
				}, time.Second*3, time.Millisecond*100).Should(Equal(0.0))

				cancel()
			})

			It("should stop when context is cancelled", func() {
				mockServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusOK)
				}))

				ctx, cancel := context.WithCancel(context.Background())

				// Start the health checker
				done := make(chan bool)
				go func() {
					runHealthChecker(ctx, mockServer.URL, healthFilePath, 1, 5)
					done <- true
				}()

				// Cancel the context after a short time
				time.Sleep(100 * time.Millisecond)
				cancel()

				// The health checker should stop
				Eventually(done, time.Second*2).Should(Receive())
			})
		})
	})
})
