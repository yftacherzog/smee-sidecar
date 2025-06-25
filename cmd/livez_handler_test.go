package main

import (
	"net/http"
	"net/http/httptest"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("livezHandler", func() {
	var (
		recorder *httptest.ResponseRecorder
		request  *http.Request
	)

	BeforeEach(func() {
		recorder = httptest.NewRecorder()
		var err error
		request, err = http.NewRequest("GET", "/livez", nil)
		Expect(err).NotTo(HaveOccurred())
	})

	Context("when called", func() {
		It("should return a 200 OK status", func() {
			livezHandler(recorder, request)
			Expect(recorder.Code).To(Equal(http.StatusOK))
		})

		It("should return 'alive' in the response body", func() {
			livezHandler(recorder, request)
			Expect(recorder.Body.String()).To(ContainSubstring("alive"))
		})

		It("should handle different HTTP methods", func() {
			methods := []string{"GET", "POST", "HEAD", "PUT", "DELETE"}
			for _, method := range methods {
				req, err := http.NewRequest(method, "/livez", nil)
				Expect(err).NotTo(HaveOccurred())
				rec := httptest.NewRecorder()

				livezHandler(rec, req)
				Expect(rec.Code).To(Equal(http.StatusOK), "Method %s should return 200", method)
				Expect(rec.Body.String()).To(ContainSubstring("alive"), "Method %s should return 'alive'", method)
			}
		})
	})

	Context("when handling concurrent requests", func() {
		It("should handle multiple simultaneous requests without issues", func() {
			const numRequests = 10
			responses := make([]*httptest.ResponseRecorder, numRequests)

			// Launch multiple concurrent requests
			done := make(chan bool, numRequests)
			for i := 0; i < numRequests; i++ {
				go func(index int) {
					defer func() { done <- true }()
					req, err := http.NewRequest("GET", "/livez", nil)
					Expect(err).NotTo(HaveOccurred())
					responses[index] = httptest.NewRecorder()
					livezHandler(responses[index], req)
				}(i)
			}

			// Wait for all requests to complete
			for i := 0; i < numRequests; i++ {
				<-done
			}

			// Verify all responses are correct
			for i, response := range responses {
				Expect(response.Code).To(Equal(http.StatusOK), "Request %d should return 200", i)
				Expect(response.Body.String()).To(ContainSubstring("alive"), "Request %d should return 'alive'", i)
			}
		})
	})
})
