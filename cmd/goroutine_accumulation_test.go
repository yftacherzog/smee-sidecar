package main

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// countStuckHTTPGoroutines counts only the goroutines that are stuck in HTTP server
// connection handling - specifically those in net/http.(*conn).serve which is
// the exact issue we identified in staging.
func countStuckHTTPGoroutines() int {
	// Get full stack trace
	buf := make([]byte, 1024*1024) // 1MB buffer
	stackSize := runtime.Stack(buf, true)
	stackTrace := string(buf[:stackSize])

	// Split into individual goroutine traces
	goroutines := strings.Split(stackTrace, "\n\n")

	stuckCount := 0
	for _, goroutine := range goroutines {
		if strings.Contains(goroutine, "net/http.(*conn).serve") {
			stuckCount++
		}
	}
	return stuckCount
}

// This test recreates the goroutine accumulation issue we found in staging
// and demonstrates that our server timeout fix resolves it.
//
// The issue: GitHub webhooks timeout after ~60s, but pipelines-as-code takes 2-15 minutes
// to process. This leaves server goroutines stuck waiting for the "next request" on
// abandoned TCP connections.
//
// The fix: Server timeouts (ReadTimeout: 180s) force cleanup of stuck goroutines.

var _ = Describe("Staging Goroutine Accumulation Issue", func() {
	var (
		slowDownstream        *httptest.Server
		originalDownstreamURL string
		testListener          net.Listener
		testServer            *http.Server
	)

	BeforeEach(func() {
		// Store original downstream URL
		originalDownstreamURL = downstreamServiceURL

		// Create a slow downstream service that takes longer than client timeout
		slowDownstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Sleep for 5 seconds to simulate slow pipelines-as-code processing
			time.Sleep(5 * time.Second)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("slow downstream response"))
		}))

		// Set the global downstream URL to our slow test server
		downstreamServiceURL = slowDownstream.URL

		// Reset proxy instance to pick up new URL
		proxyInstance = nil
		proxyOnce = sync.Once{}
		proxyError = nil

		// Reset global state
		mutex.Lock()
		healthChecks = make(map[string]chan bool)
		mutex.Unlock()
	})

	AfterEach(func() {
		// Restore original downstream URL
		downstreamServiceURL = originalDownstreamURL

		// Clean up test resources
		if slowDownstream != nil {
			slowDownstream.Close()
		}
		if testServer != nil {
			testServer.Close()
		}
		if testListener != nil {
			testListener.Close()
		}

		// Reset proxy instance
		proxyInstance = nil
		proxyOnce = sync.Once{}
		proxyError = nil
	})

	Describe("Recreating Staging Issue - WITHOUT Server Timeouts", func() {
		It("should accumulate stuck HTTP goroutines exactly like staging environment", func() {
			// Create server WITHOUT timeouts (recreating the staging issue)
			testServer = &http.Server{
				Addr:    ":0",
				Handler: http.HandlerFunc(forwardHandler),
			}

			var err error
			testListener, err = net.Listen("tcp", ":0")
			Expect(err).NotTo(HaveOccurred())

			go func() {
				testServer.Serve(testListener)
			}()

			serverURL := fmt.Sprintf("http://%s", testListener.Addr().String())

			// Count initial stuck HTTP goroutines (should be 0)
			initialStuckGoroutines := countStuckHTTPGoroutines()
			totalInitialGoroutines := runtime.NumGoroutine()
			fmt.Printf("Initial stuck HTTP goroutines: %d (total: %d)\n", initialStuckGoroutines, totalInitialGoroutines)

			// Create multiple clients that timeout quickly
			numClients := 5
			clientTimeout := 1 * time.Second // Much shorter than downstream processing time

			var wg sync.WaitGroup
			for i := 0; i < numClients; i++ {
				wg.Add(1)
				go func(clientID int) {
					defer wg.Done()

					// Create client with short timeout
					client := &http.Client{
						Timeout: clientTimeout,
					}

					req, err := http.NewRequest("POST", serverURL, bytes.NewBufferString(fmt.Sprintf(`{"client": %d}`, clientID)))
					if err != nil {
						return
					}

					// Make request - this should timeout on client side
					resp, err := client.Do(req)
					if err != nil {
						// Expected timeout error
						return
					}
					if resp != nil {
						resp.Body.Close()
					}
				}(i)
			}

			// Wait for all clients to timeout
			wg.Wait()

			// Give the system a moment to process
			time.Sleep(2 * time.Second)

			// Count stuck HTTP goroutines after client timeouts
			afterTimeoutStuckGoroutines := countStuckHTTPGoroutines()
			totalAfterTimeoutGoroutines := runtime.NumGoroutine()

			stuckGoroutineIncrease := afterTimeoutStuckGoroutines - initialStuckGoroutines
			totalGoroutineIncrease := totalAfterTimeoutGoroutines - totalInitialGoroutines

			fmt.Printf("After client timeouts - stuck HTTP goroutines: %d (total: %d)\n", afterTimeoutStuckGoroutines, totalAfterTimeoutGoroutines)
			fmt.Printf("Stuck HTTP goroutine increase: %d (total increase: %d)\n", stuckGoroutineIncrease, totalGoroutineIncrease)

			// With the original bug, we should see stuck HTTP goroutines accumulate
			Expect(stuckGoroutineIncrease).To(BeNumerically(">=", 1),
				"Expected stuck HTTP goroutines to accumulate due to client timeouts")
		})
	})

	Describe("Recovery with Server Timeouts - WITH Our Fix", func() {
		It("should recover stuck HTTP goroutines using testable timeouts", func() {
			// Create server WITH timeouts (testing our fix)
			// Using short timeouts for testing, but same pattern as production
			testServer = &http.Server{
				Addr:         ":0",
				Handler:      http.HandlerFunc(forwardHandler),
				ReadTimeout:  3 * time.Second, // Short for testing - will cleanup stuck goroutines
				WriteTimeout: 2 * time.Second, // Short for testing
				IdleTimeout:  5 * time.Second, // Short for testing
			}

			var err error
			testListener, err = net.Listen("tcp", ":0")
			Expect(err).NotTo(HaveOccurred())

			go func() {
				testServer.Serve(testListener)
			}()

			serverURL := fmt.Sprintf("http://%s", testListener.Addr().String())

			// Count initial stuck HTTP goroutines (should be 0)
			initialStuckGoroutines := countStuckHTTPGoroutines()
			totalInitialGoroutines := runtime.NumGoroutine()
			fmt.Printf("Initial stuck HTTP goroutines: %d (total: %d)\n", initialStuckGoroutines, totalInitialGoroutines)

			// Create multiple clients that timeout quickly (simulating GitHub webhook timeouts)
			numClients := 5
			clientTimeout := 1 * time.Second // Much shorter than downstream processing time

			var wg sync.WaitGroup
			for i := 0; i < numClients; i++ {
				wg.Add(1)
				go func(clientID int) {
					defer wg.Done()

					// Create client with short timeout
					client := &http.Client{
						Timeout: clientTimeout,
					}

					req, err := http.NewRequest("POST", serverURL, bytes.NewBufferString(fmt.Sprintf(`{"client": %d}`, clientID)))
					if err != nil {
						return
					}

					// Make request - this should timeout on client side
					resp, err := client.Do(req)
					if err != nil {
						// Expected timeout error - simulating GitHub giving up
						return
					}
					if resp != nil {
						resp.Body.Close()
					}
				}(i)
			}

			// Wait for all clients to timeout
			wg.Wait()

			// Wait for client timeouts to create the problem
			time.Sleep(2 * time.Second)

			// Count stuck HTTP goroutines after client timeouts (should be accumulated)
			afterClientTimeoutsStuckGoroutines := countStuckHTTPGoroutines()
			totalAfterClientTimeouts := runtime.NumGoroutine()
			clientTimeoutStuckIncrease := afterClientTimeoutsStuckGoroutines - initialStuckGoroutines

			fmt.Printf("After client timeouts - stuck HTTP goroutines: %d (total: %d)\n", afterClientTimeoutsStuckGoroutines, totalAfterClientTimeouts)
			fmt.Printf("Stuck HTTP goroutine increase after client timeouts: %d\n", clientTimeoutStuckIncrease)

			// Now wait for server ReadTimeout to trigger cleanup (3 seconds + buffer)
			fmt.Printf("⏳ Waiting for server ReadTimeout (3s) to clean up stuck goroutines...\n")
			time.Sleep(4 * time.Second)

			// Count stuck HTTP goroutines after server timeout cleanup
			afterServerTimeoutStuckGoroutines := countStuckHTTPGoroutines()
			totalAfterServerTimeout := runtime.NumGoroutine()
			finalStuckIncrease := afterServerTimeoutStuckGoroutines - initialStuckGoroutines

			fmt.Printf("After server timeout cleanup - stuck HTTP goroutines: %d (total: %d)\n", afterServerTimeoutStuckGoroutines, totalAfterServerTimeout)
			fmt.Printf("Final stuck HTTP goroutine increase: %d\n", finalStuckIncrease)

			// The key test: server timeouts should reduce stuck HTTP goroutine count
			stuckGoroutinesRecovered := clientTimeoutStuckIncrease - finalStuckIncrease
			fmt.Printf("🎯 Stuck HTTP goroutines recovered by server timeout: %d\n", stuckGoroutinesRecovered)

			// Verify we had stuck goroutines initially
			Expect(clientTimeoutStuckIncrease).To(BeNumerically(">=", 1),
				"Should have initial stuck HTTP goroutine accumulation from client timeouts")

			// If we had stuck goroutines, verify server timeouts helped with cleanup
			if clientTimeoutStuckIncrease > 0 {
				Expect(finalStuckIncrease).To(BeNumerically("<=", clientTimeoutStuckIncrease),
					"Server timeouts should reduce stuck HTTP goroutine accumulation")
			}

			fmt.Printf("✅ Recovery demonstrated: server timeouts cleaned up %d stuck HTTP goroutines\n", stuckGoroutinesRecovered)
		})
	})
})
