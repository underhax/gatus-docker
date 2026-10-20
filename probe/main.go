// Package main implements a zero-dependency healthcheck probe that validates
// Gatus server availability against its internal HTTP liveness endpoint.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"
)

const defaultEndpoint = "http://127.0.0.1:8080/health"

const defaultTimeout = 3 * time.Second

type healthResponse struct {
	Status string `json:"status"`
}

func defaultGetTransport() http.RoundTripper {
	return http.DefaultTransport
}

var (
	getTransport = defaultGetTransport
	exitFunc     = os.Exit
)

func defaultCreateRequest(ctx context.Context, method, endpoint string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("creating HTTP request: %w", err)
	}
	return req, nil
}

var createRequest = defaultCreateRequest

func defaultDoRequest(ctx context.Context, endpoint string) (err error) {
	parsedURL, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("parsing probe endpoint: %w", err)
	}

	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		return fmt.Errorf("unsupported probe protocol scheme: %s", parsedURL.Scheme)
	}

	req, err := createRequest(ctx, http.MethodGet, parsedURL.String(), http.NoBody)
	if err != nil {
		return fmt.Errorf("creating probe request: %w", err)
	}

	client := &http.Client{
		Timeout:   defaultTimeout,
		Transport: getTransport(),
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("executing probe request: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("closing response body: %w", closeErr)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected probe status code: %d", resp.StatusCode)
	}

	var result healthResponse
	if decodeErr := json.NewDecoder(resp.Body).Decode(&result); decodeErr != nil {
		return fmt.Errorf("decoding probe response payload: %w", decodeErr)
	}

	if result.Status != "UP" {
		return errors.New("probe health status reported unhealthy")
	}

	return nil
}

var doRequest = defaultDoRequest

// CheckEndpoint queries the target endpoint using HTTP GET to verify service liveness.
func CheckEndpoint(ctx context.Context, endpoint string) int {
	if err := doRequest(ctx, endpoint); err != nil {
		return 1
	}
	return 0
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()

	exitFunc(CheckEndpoint(ctx, defaultEndpoint))
}
