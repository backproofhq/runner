package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/backproofhq/runner/internal/config"
)

const (
	requestTimeout      = 30 * time.Second
	defaultPollInterval = 30 * time.Second
)

// ErrLeaseLost means this runner no longer holds the job, so it must stop working on it.
var ErrLeaseLost = errors.New("this runner no longer holds the job")

// RunnerClient is the runner's connection to the Backproof v1 API.
type RunnerClient struct {
	api     *ClientWithResponses
	baseURL string
}

// New returns a client for the calls that need no runner token.
func New(baseURL, runnerVersion string) (*RunnerClient, error) {
	return NewAuthorized(baseURL, runnerVersion, config.Secret{})
}

// NewAuthorized returns a client that identifies itself with the runner token.
func NewAuthorized(baseURL, runnerVersion string, token config.Secret) (*RunnerClient, error) {
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
			if token.IsSet() {
				request.Header.Set("Authorization", "Bearer "+token.Reveal())
			}
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

// PollResult is the API's answer to a check-in.
type PollResult struct {
	// Job is nil when no job is due.
	Job           *Job
	LatestVersion string
	MinVersion    string
	NextPollAfter time.Duration
	// UpdateRequired explains why a runner below the minimum version gets no jobs.
	UpdateRequired string
}

// Poll checks in and claims a job if one is due.
func (c *RunnerClient) Poll(ctx context.Context, request PollRequest) (PollResult, error) {
	response, err := c.api.PollRunnerWithResponse(ctx, request)
	if err != nil {
		return PollResult{}, fmt.Errorf("reach the Backproof API at %s: %w", c.baseURL, err)
	}

	status := response.StatusCode()
	if status != http.StatusOK && status != http.StatusNoContent && status != http.StatusUpgradeRequired {
		return PollResult{}, newHTTPError(response.HTTPResponse, response.Body)
	}

	headers := response.HTTPResponse.Header
	result := PollResult{
		Job:           response.JSON200,
		LatestVersion: headers.Get("Backproof-Latest-Version"),
		MinVersion:    headers.Get("Backproof-Min-Version"),
		NextPollAfter: secondsHeader(headers, "Backproof-Next-Poll-After", defaultPollInterval),
	}
	if status == http.StatusUpgradeRequired {
		result.UpdateRequired = newResponseError(status, response.Body).Message
	}
	return result, nil
}

// StatusResult is what Backproof knows about this runner.
type StatusResult struct {
	Runner        RunnerStatus
	LatestVersion string
	MinVersion    string
}

// Status reads the runner's registration without checking in or claiming a job.
func (c *RunnerClient) Status(ctx context.Context) (StatusResult, error) {
	response, err := c.api.GetRunnerStatusWithResponse(ctx)
	if err != nil {
		return StatusResult{}, fmt.Errorf("reach the Backproof API at %s: %w", c.baseURL, err)
	}
	if response.JSON200 == nil {
		return StatusResult{}, newHTTPError(response.HTTPResponse, response.Body)
	}
	headers := response.HTTPResponse.Header
	return StatusResult{
		Runner:        *response.JSON200,
		LatestVersion: headers.Get("Backproof-Latest-Version"),
		MinVersion:    headers.Get("Backproof-Min-Version"),
	}, nil
}

// Heartbeat renews a job's lease and reports its current stage.
func (c *RunnerClient) Heartbeat(ctx context.Context, jobID JobId, stage Stage) (HeartbeatResponse, error) {
	response, err := c.api.HeartbeatJobWithResponse(ctx, jobID, HeartbeatRequest{Stage: stage})
	if err != nil {
		return HeartbeatResponse{}, fmt.Errorf("reach the Backproof API at %s: %w", c.baseURL, err)
	}
	if response.JSON200 != nil {
		return *response.JSON200, nil
	}
	return HeartbeatResponse{}, jobError(response.HTTPResponse, response.Body)
}

// SubmitResult reports the outcome of a job.
func (c *RunnerClient) SubmitResult(ctx context.Context, jobID JobId, result JobResult) error {
	response, err := c.api.SubmitJobResultWithResponse(ctx, jobID, result)
	if err != nil {
		return fmt.Errorf("reach the Backproof API at %s: %w", c.baseURL, err)
	}
	if response.StatusCode() == http.StatusAccepted {
		return nil
	}
	return jobError(response.HTTPResponse, response.Body)
}

func jobError(response *http.Response, body []byte) error {
	if response.StatusCode == http.StatusConflict || response.StatusCode == http.StatusNotFound {
		return ErrLeaseLost
	}
	return newHTTPError(response, body)
}

func secondsHeader(headers http.Header, name string, fallback time.Duration) time.Duration {
	seconds, err := strconv.Atoi(headers.Get(name))
	if err != nil || seconds < 1 {
		return fallback
	}
	return time.Duration(seconds) * time.Second
}

// ResponseError is an answer from the API that reports a problem.
type ResponseError struct {
	Status  int
	Code    string
	Message string
	// RetryAfter is how long the API asks the runner to wait, when it says so.
	RetryAfter time.Duration
}

// Retryable reports whether the same request may succeed later.
func (e *ResponseError) Retryable() bool {
	return e.Status == http.StatusTooManyRequests || e.Status >= http.StatusInternalServerError
}

// Error returns the API's explanation, which says what to do next.
func (e *ResponseError) Error() string {
	return e.Message
}

func newHTTPError(response *http.Response, body []byte) *ResponseError {
	problem := newResponseError(response.StatusCode, body)
	problem.RetryAfter = secondsHeader(response.Header, "Retry-After", 0)
	return problem
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
