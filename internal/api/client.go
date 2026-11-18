package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const requestTimeout = 30 * time.Second

// RunnerClient is the runner's connection to the Backproof v1 API.
type RunnerClient struct {
	api     *ClientWithResponses
	baseURL string
}

// New returns a client for the API at baseURL.
func New(baseURL, runnerVersion string) (*RunnerClient, error) {
	httpClient := &http.Client{
		Timeout: requestTimeout,
		// Requests carry tokens, which must never follow a redirect to another address.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	api, err := NewClientWithResponses(baseURL,
		WithHTTPClient(httpClient),
		WithRequestEditorFn(func(_ context.Context, request *http.Request) error {
			request.Header.Set("User-Agent", "backproof-runner/"+runnerVersion)
			return nil
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("create API client: %w", err)
	}
	return &RunnerClient{api: api, baseURL: baseURL}, nil
}

// Register trades an install token for a runner credential.
func (c *RunnerClient) Register(ctx context.Context, request RegisterRequest) (RegisterResponse, error) {
	response, err := c.api.RegisterRunnerWithResponse(ctx, request)
	if err != nil {
		return RegisterResponse{}, fmt.Errorf("reach the Backproof API at %s: %w", c.baseURL, err)
	}
	if response.JSON201 == nil {
		return RegisterResponse{}, newResponseError(response.StatusCode(), response.Body)
	}
	return *response.JSON201, nil
}

// ResponseError is an answer from the API that reports a problem.
type ResponseError struct {
	Status  int
	Code    string
	Message string
}

// Error returns the API's explanation, which says what to do next.
func (e *ResponseError) Error() string {
	return e.Message
}

func newResponseError(status int, body []byte) *ResponseError {
	var problem Error
	if err := json.Unmarshal(body, &problem); err != nil || problem.Message == "" {
		return &ResponseError{
			Status:  status,
			Message: fmt.Sprintf("The Backproof API answered with status %d. Try again in a few minutes.", status),
		}
	}
	return &ResponseError{Status: status, Code: problem.Code, Message: problem.Message}
}
