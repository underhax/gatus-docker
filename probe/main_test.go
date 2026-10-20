package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

var errMockTransport = errors.New("simulated transport failure")

var errMockClose = errors.New("simulated response body close error")

var errMockRequest = errors.New("simulated request creation failure")

type mockRoundTripper struct {
	roundTripFunc func(req *http.Request) (*http.Response, error)
}

func (m *mockRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return m.roundTripFunc(req)
}

var currentMockTransport http.RoundTripper

func mockTransportProvider() http.RoundTripper {
	return currentMockTransport
}

func mockFailingCreateRequest(_ context.Context, _, _ string, _ io.Reader) (*http.Request, error) {
	return nil, errMockRequest
}

func absentContext() context.Context {
	return nil
}

func TestCheckEndpoint(t *testing.T) {
	tests := []struct {
		mockTripper *mockRoundTripper
		name        string
		wantCode    int
	}{
		{
			mockTripper: &mockRoundTripper{
				roundTripFunc: func(_ *http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: http.StatusOK,
						Body:       io.NopCloser(strings.NewReader(`{"status": "UP"}`)),
					}, nil
				},
			},
			name:     "healthy response returns success code",
			wantCode: 0,
		},
		{
			mockTripper: &mockRoundTripper{
				roundTripFunc: func(_ *http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: http.StatusOK,
						Body:       io.NopCloser(strings.NewReader(`{"status": "DOWN"}`)),
					}, nil
				},
			},
			name:     "unhealthy payload returns error code",
			wantCode: 1,
		},
		{
			mockTripper: &mockRoundTripper{
				roundTripFunc: func(_ *http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: http.StatusOK,
						Body:       io.NopCloser(strings.NewReader(`not-json`)),
					}, nil
				},
			},
			name:     "malformed payload returns error code",
			wantCode: 1,
		},
		{
			mockTripper: &mockRoundTripper{
				roundTripFunc: func(_ *http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: http.StatusServiceUnavailable,
						Body:       io.NopCloser(strings.NewReader(`{"error": "service unavailable"}`)),
					}, nil
				},
			},
			name:     "service unavailable returns error code",
			wantCode: 1,
		},
		{
			mockTripper: &mockRoundTripper{
				roundTripFunc: func(_ *http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: http.StatusInternalServerError,
						Body:       io.NopCloser(strings.NewReader(`{"error": "internal error"}`)),
					}, nil
				},
			},
			name:     "internal server error returns error code",
			wantCode: 1,
		},
		{
			mockTripper: &mockRoundTripper{
				roundTripFunc: func(_ *http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: http.StatusNotFound,
						Body:       io.NopCloser(strings.NewReader(`{"error": "not found"}`)),
					}, nil
				},
			},
			name:     "not found returns error code",
			wantCode: 1,
		},
		{
			mockTripper: &mockRoundTripper{
				roundTripFunc: func(_ *http.Request) (*http.Response, error) {
					return nil, errMockTransport
				},
			},
			name:     "transport error returns error code",
			wantCode: 1,
		},
	}

	originalTransport := getTransport
	getTransport = mockTransportProvider
	defer func() {
		getTransport = originalTransport
	}()

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			currentMockTransport = tc.mockTripper
			got := CheckEndpoint(context.Background(), "http://gatus.example.com:8080/health")
			if got != tc.wantCode {
				t.Errorf("CheckEndpoint() = %d, want %d", got, tc.wantCode)
			}
		})
	}
}

type closeFailingReader struct {
	io.Reader
}

func (closeFailingReader) Close() error {
	return errMockClose
}

func TestCheckEndpoint_CancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got := CheckEndpoint(ctx, "http://probe.example.org:8080/health")
	if got != 1 {
		t.Errorf("CheckEndpoint() with cancelled context = %d, want 1", got)
	}
}

func mockFailingDoRequest(_ context.Context, _ string) error {
	return errMockTransport
}

func TestCheckEndpoint_MockedFailure(t *testing.T) {
	originalDoRequest := doRequest
	doRequest = mockFailingDoRequest
	defer func() {
		doRequest = originalDoRequest
	}()

	got := CheckEndpoint(context.Background(), "http://health.example:8080/health")
	if got != 1 {
		t.Errorf("CheckEndpoint() with mocked transport error = %d, want 1", got)
	}
}

func TestDefaultCreateRequest(t *testing.T) {
	if _, err := defaultCreateRequest(context.Background(), http.MethodGet, "http://probe.example.org:8080/health", http.NoBody); err != nil {
		t.Fatalf("defaultCreateRequest() error = %v", err)
	}

	if _, err := defaultCreateRequest(absentContext(), http.MethodGet, "http://probe.example.org:8080/health", http.NoBody); err == nil {
		t.Error("defaultCreateRequest() expected an error for an absent context")
	}
}

func TestDefaultDoRequest_RequestCreationFailure(t *testing.T) {
	originalCreateRequest := createRequest
	createRequest = mockFailingCreateRequest
	defer func() {
		createRequest = originalCreateRequest
	}()

	if err := defaultDoRequest(context.Background(), "http://probe.example.org:8080/health"); !errors.Is(err, errMockRequest) {
		t.Errorf("defaultDoRequest() error = %v, want %v", err, errMockRequest)
	}
}

func TestDefaultDoRequest_CloseFailure(t *testing.T) {
	originalTransport := getTransport
	getTransport = mockTransportProvider
	defer func() {
		getTransport = originalTransport
	}()

	currentMockTransport = &mockRoundTripper{
		roundTripFunc: func(_ *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       closeFailingReader{Reader: strings.NewReader(`{"status": "UP"}`)},
			}, nil
		},
	}

	if err := defaultDoRequest(context.Background(), "http://probe.example.org:8080/health"); !errors.Is(err, errMockClose) {
		t.Errorf("defaultDoRequest() error = %v, want %v", err, errMockClose)
	}
}

func TestDefaultDoRequest_InvalidURL(t *testing.T) {
	tests := []struct {
		urlStr string
		name   string
	}{
		{
			urlStr: "http://invalid-domain-with-space :8080",
			name:   "invalid URL with space",
		},
		{
			urlStr: "ftp://127.0.0.1:8080/health",
			name:   "unsupported URL protocol scheme",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := defaultDoRequest(context.Background(), tc.urlStr); err == nil {
				t.Errorf("defaultDoRequest(%q) expected error, got nil", tc.urlStr)
			}
		})
	}
}

func mockExitRecorder(code int) {
	recordedExitCode = code
}

var recordedExitCode = -1

func mockSuccessDoRequest(_ context.Context, _ string) error {
	return nil
}

func TestMain_Execution(t *testing.T) {
	originalExit := exitFunc
	exitFunc = mockExitRecorder
	defer func() {
		exitFunc = originalExit
	}()

	originalDoRequest := doRequest
	doRequest = mockSuccessDoRequest
	defer func() {
		doRequest = originalDoRequest
	}()

	main()

	if recordedExitCode != 0 {
		t.Errorf("main() recorded exit code = %d, want 0", recordedExitCode)
	}
}

func TestMain_ExecutionDefaultEndpointFailure(t *testing.T) {
	originalExit := exitFunc
	exitFunc = mockExitRecorder
	defer func() {
		exitFunc = originalExit
	}()

	originalDoRequest := doRequest
	doRequest = mockFailingDoRequest
	defer func() {
		doRequest = originalDoRequest
	}()

	main()

	if recordedExitCode != 1 {
		t.Errorf("main() recorded exit code = %d, want 1", recordedExitCode)
	}
}
