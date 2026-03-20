// Package cloudrun provides a client for calling Cloud Run services
// with identity token authentication and service URL auto-discovery.
package cloudrun

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"google.golang.org/api/idtoken"
	"google.golang.org/api/option"
	runapi "google.golang.org/api/run/v2"
)

const (
	maxRetries       = 3
	initialBackoff   = 1 * time.Second
	maxBackoff       = 4 * time.Second
)

// Client calls Cloud Run services using identity token authentication.
type Client struct {
	Project string
	Region  string

	httpClient *http.Client
}

// NewClient creates a Cloud Run client that authenticates with identity tokens.
// The audience is set dynamically per request based on the service URL.
func NewClient(ctx context.Context, project, region string) *Client {
	return &Client{
		Project: project,
		Region:  region,
	}
}

// DiagnoseRequest is the payload sent to the diagnose-agent service.
type DiagnoseRequest struct {
	Query  string `json:"query"`
	Stream bool   `json:"stream,omitempty"`
}

// StreamEvent represents a single NDJSON event from the streaming response.
type StreamEvent struct {
	Event      string                 `json:"event"`                // "tool_call", "tool_result", "done", "error", "text"
	Tool       string                 `json:"tool,omitempty"`       // tool name (for tool_call/tool_result)
	CallID     string                 `json:"call_id,omitempty"`    // unique ID for tool_call correlation
	Parameters map[string]interface{} `json:"parameters,omitempty"` // tool parameters (for tool_call)
	Result     json.RawMessage        `json:"result,omitempty"`     // tool result or final result (for tool_result/done)
	Content    string                 `json:"content,omitempty"`    // text content (for text event)
	Error      string                 `json:"error,omitempty"`      // error message (for error event)
}

// DiagnoseResponse is the response from the diagnose-agent service.
type DiagnoseResponse struct {
	Status    string                 `json:"status"`
	Diagnosis Diagnosis              `json:"diagnosis"`
	Metadata  map[string]interface{} `json:"metadata,omitempty"`
	Error     string                 `json:"error,omitempty"`
}

// Diagnosis contains the structured diagnostic result.
type Diagnosis struct {
	RootCause      string   `json:"root_cause"`
	Confidence     string   `json:"confidence"`
	Evidence       []string `json:"evidence"`
	Recommendation string   `json:"recommendation"`
	Severity       string   `json:"severity"`
}

// DiscoverServiceURL finds the URL of a Cloud Run service by name.
func (c *Client) DiscoverServiceURL(ctx context.Context, serviceName string) (string, error) {
	svc, err := runapi.NewService(ctx, option.WithScopes(runapi.CloudPlatformScope))
	if err != nil {
		return "", wrapAuthError("creating Cloud Run client", err)
	}

	name := fmt.Sprintf("projects/%s/locations/%s/services/%s", c.Project, c.Region, serviceName)
	service, err := svc.Projects.Locations.Services.Get(name).Context(ctx).Do()
	if err != nil {
		return "", wrapAuthError(fmt.Sprintf("discovering service %q", serviceName), err)
	}

	if service.Uri == "" {
		return "", fmt.Errorf("service %q has no URL", serviceName)
	}

	return service.Uri, nil
}

// Diagnose sends a query to the diagnose-agent and returns the response.
func (c *Client) Diagnose(ctx context.Context, serviceURL, query string) (*DiagnoseResponse, error) {
	body, err := json.Marshal(DiagnoseRequest{Query: query})
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	endpoint := strings.TrimRight(serviceURL, "/") + "/diagnose"

	httpClient, err := c.getHTTPClient(ctx, serviceURL)
	if err != nil {
		return nil, err
	}

	resp, err := c.doWithRetry(ctx, httpClient, http.MethodPost, endpoint,
		func() io.Reader { return bytes.NewReader(body) },
		map[string]string{"Content-Type": "application/json"},
	)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("diagnose-agent returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result DiagnoseResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}

	return &result, nil
}

// DiagnoseStream sends a streaming query to the diagnose-agent and calls
// onEvent for each progress event. Returns the final DiagnoseResponse.
func (c *Client) DiagnoseStream(ctx context.Context, serviceURL, query string, onEvent func(StreamEvent)) (*DiagnoseResponse, error) {
	body, err := json.Marshal(DiagnoseRequest{Query: query, Stream: true})
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	endpoint := strings.TrimRight(serviceURL, "/") + "/diagnose"

	httpClient, err := c.getHTTPClient(ctx, serviceURL)
	if err != nil {
		return nil, err
	}

	resp, err := c.doWithRetry(ctx, httpClient, http.MethodPost, endpoint,
		func() io.Reader { return bytes.NewReader(body) },
		map[string]string{"Content-Type": "application/json"},
	)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("diagnose-agent returned %d: %s", resp.StatusCode, string(respBody))
	}

	scanner := bufio.NewScanner(resp.Body)
	// Allow large lines (agent responses can be verbose).
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var event StreamEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}

		if onEvent != nil {
			onEvent(event)
		}

		switch event.Event {
		case "done":
			var result DiagnoseResponse
			if err := json.Unmarshal(event.Result, &result); err != nil {
				return nil, fmt.Errorf("parsing final result: %w", err)
			}
			return &result, nil
		case "error":
			return nil, fmt.Errorf("diagnose-agent error: %s", event.Error)
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading stream: %w", err)
	}

	return nil, fmt.Errorf("stream ended without a final result")
}

// isTransientStatus returns true for HTTP status codes that indicate a
// transient error (typically Cloud Run cold start or infrastructure issues).
func isTransientStatus(code int) bool {
	return code == http.StatusBadGateway || code == http.StatusServiceUnavailable || code == http.StatusGatewayTimeout
}

// doWithRetry executes an HTTP request, retrying on transient status codes
// (502, 503, 504) with exponential backoff. The bodyFunc is called on each
// attempt to provide a fresh request body.
func (c *Client) doWithRetry(ctx context.Context, httpClient *http.Client, method, url string, bodyFunc func() io.Reader, headers map[string]string) (*http.Response, error) {
	backoff := initialBackoff

	for attempt := 0; attempt <= maxRetries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, url, bodyFunc())
		if err != nil {
			return nil, fmt.Errorf("creating request: %w", err)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}

		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, wrapAuthError("calling diagnose-agent", err)
		}

		if !isTransientStatus(resp.StatusCode) || attempt == maxRetries {
			return resp, nil
		}

		// Drain body before retry to allow connection reuse.
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		fmt.Fprintf(os.Stderr,
			"diagnose-agent returned %d, retrying in %s (attempt %d/%d)\n",
			resp.StatusCode, backoff, attempt+1, maxRetries,
		)

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}

		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}

	// unreachable, but keeps the compiler happy
	return nil, fmt.Errorf("retry loop exited unexpectedly")
}

func (c *Client) getHTTPClient(ctx context.Context, audience string) (*http.Client, error) {
	if c.httpClient != nil {
		return c.httpClient, nil
	}

	// Try service account credentials first (idtoken library).
	client, err := idtoken.NewClient(ctx, audience)
	if err == nil {
		c.httpClient = client
		return client, nil
	}

	// Fall back to gcloud for user credentials (authorized_user type).
	if strings.Contains(err.Error(), "unsupported credentials type") {
		return c.gcloudIdentityTokenClient(ctx, audience)
	}

	return nil, wrapAuthError("obtaining identity token", err)
}

// gcloudIdentityTokenClient creates an HTTP client that uses
// `gcloud auth print-identity-token` for authentication. This supports
// user credentials which the idtoken library does not handle.
func (c *Client) gcloudIdentityTokenClient(ctx context.Context, audience string) (*http.Client, error) {
	token, err := gcloudIdentityToken(ctx, audience)
	if err != nil {
		return nil, err
	}

	client := &http.Client{
		Transport: &identityTokenTransport{
			base:     http.DefaultTransport,
			audience: audience,
			token:    token,
		},
	}
	c.httpClient = client
	return client, nil
}

// gcloudIdentityToken obtains an identity token via gcloud CLI.
// For user credentials, --audiences is not supported so we omit it;
// Cloud Run accepts the default identity token.
func gcloudIdentityToken(ctx context.Context, audience string) (string, error) {
	cmd := exec.CommandContext(ctx, "gcloud", "auth", "print-identity-token")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("obtaining identity token via gcloud: %w\n\n"+
			"  Ensure you are logged in: gcloud auth login", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// identityTokenTransport adds a Bearer identity token to requests.
type identityTokenTransport struct {
	base     http.RoundTripper
	audience string
	token    string
}

func (t *identityTokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(req)
}

// ToolDef describes a tool that the CLI can execute locally.
type ToolDef struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters"`
}

// ChatMessage represents a single message in the conversation history.
type ChatMessage struct {
	Role    string `json:"role"`    // "user" or "assistant"
	Content string `json:"content"`
}

// ChatToolResult carries the result of a locally-executed tool call.
type ChatToolResult struct {
	CallID     string                 `json:"call_id"`
	Tool       string                 `json:"tool"`
	Parameters map[string]interface{} `json:"parameters,omitempty"`
	Result     json.RawMessage        `json:"result"`
	Error      string                 `json:"error,omitempty"`
}

// ChatRequest is the payload for each conversational turn.
type ChatRequest struct {
	History       []ChatMessage   `json:"history"`
	Tools         []ToolDef       `json:"tools"`
	ToolResult    *ChatToolResult `json:"tool_result,omitempty"`
	MaxIterations int             `json:"max_iterations,omitempty"`
}

// ChatTurnResult is returned by ChatStream after consuming the NDJSON stream.
type ChatTurnResult struct {
	PendingToolCall *StreamEvent // non-nil if stream ended with a tool_call event
}

// ChatStream sends a chat request and streams NDJSON events via onEvent.
// If the stream ends with a tool_call event, ChatTurnResult.PendingToolCall is set.
func (c *Client) ChatStream(ctx context.Context, serviceURL string, req ChatRequest, onEvent func(StreamEvent)) (*ChatTurnResult, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	endpoint := strings.TrimRight(serviceURL, "/") + "/chat"

	httpClient, err := c.getHTTPClient(ctx, serviceURL)
	if err != nil {
		return nil, err
	}

	resp, err := c.doWithRetry(ctx, httpClient, http.MethodPost, endpoint,
		func() io.Reader { return bytes.NewReader(body) },
		map[string]string{"Content-Type": "application/json"},
	)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("sre-companion-agent returned %d: %s", resp.StatusCode, string(respBody))
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	result := &ChatTurnResult{}

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var event StreamEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}

		if onEvent != nil {
			onEvent(event)
		}

		switch event.Event {
		case "done":
			return result, nil
		case "error":
			return nil, fmt.Errorf("sre-companion-agent error: %s", event.Error)
		case "tool_call":
			result.PendingToolCall = &event
			return result, nil
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading stream: %w", err)
	}

	return nil, fmt.Errorf("stream ended without a done or tool_call event")
}

// ParseResponse parses a raw JSON byte slice into a DiagnoseResponse.
func ParseResponse(data []byte) (*DiagnoseResponse, error) {
	var resp DiagnoseResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parsing diagnose response: %w", err)
	}
	return &resp, nil
}

func wrapAuthError(action string, err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "could not find default credentials"):
		return fmt.Errorf("%s: no GCP credentials found\n\n"+
			"  Run: gcloud auth application-default login\n"+
			"  Or set GOOGLE_APPLICATION_CREDENTIALS to a service account key file", action)
	case strings.Contains(msg, "token expired") || strings.Contains(msg, "oauth2: token expired"):
		return fmt.Errorf("%s: GCP credentials have expired\n\n"+
			"  Run: gcloud auth application-default login", action)
	case strings.Contains(msg, "PermissionDenied") || strings.Contains(msg, "permission denied") || strings.Contains(msg, "403"):
		return fmt.Errorf("%s: permission denied\n\n"+
			"  Ensure your account has the Cloud Run Invoker role:\n"+
			"    - roles/run.invoker\n\n"+
			"  Check: gcloud projects get-iam-policy <project> --flatten='bindings[].members' --filter='bindings.members:<your-email>'", action)
	case strings.Contains(msg, "NotFound") || strings.Contains(msg, "not found"):
		return fmt.Errorf("%s: service not found\n\n"+
			"  Verify the diagnose-agent is deployed:\n"+
			"    gcloud run services list --project <project> --region <region>\n"+
			"  Check --project and --region flags are correct", action)
	case strings.Contains(msg, "Unauthenticated") || strings.Contains(msg, "401"):
		return fmt.Errorf("%s: authentication failed\n\n"+
			"  Run: gcloud auth application-default login\n"+
			"  Or: gcloud auth login", action)
	default:
		return fmt.Errorf("%s: %w", action, err)
	}
}
