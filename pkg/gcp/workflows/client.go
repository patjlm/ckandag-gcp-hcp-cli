// Package workflows provides a client for executing and managing Google Cloud Workflows.
package workflows

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	executions "cloud.google.com/go/workflows/executions/apiv1"
	executionspb "cloud.google.com/go/workflows/executions/apiv1/executionspb"
	wfapi "cloud.google.com/go/workflows/apiv1"
	workflowspb "cloud.google.com/go/workflows/apiv1/workflowspb"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/iterator"
)

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
			"  Ensure your account has the required roles:\n"+
			"    - roles/workflows.invoker (to execute workflows)\n"+
			"    - roles/workflows.viewer (to list workflows)\n\n"+
			"  Check: gcloud projects get-iam-policy <project> --flatten='bindings[].members' --filter='bindings.members:<your-email>'", action)
	case strings.Contains(msg, "NotFound") || strings.Contains(msg, "not found"):
		return fmt.Errorf("%s: resource not found\n\n"+
			"  Verify the workflow exists: gcphcp ops wf list --project <project> --region <region>\n"+
			"  Check --project and --region flags are correct", action)
	case strings.Contains(msg, "Unauthenticated") || strings.Contains(msg, "401"):
		return fmt.Errorf("%s: authentication failed\n\n"+
			"  Run: gcloud auth application-default login\n"+
			"  Or: gcloud auth login", action)
	default:
		return fmt.Errorf("%s: %w", action, err)
	}
}

// Client wraps the Google Cloud Workflows API.
type Client struct {
	Project string
	Region  string

	execClient     *executions.Client
	workflowClient *wfapi.Client
}

// NewClient creates a new Workflows client using Application Default Credentials.
func NewClient(ctx context.Context, project, region string) (*Client, error) {
	execClient, err := executions.NewClient(ctx)
	if err != nil {
		return nil, wrapAuthError("creating workflows client", err)
	}

	wfClient, err := wfapi.NewClient(ctx)
	if err != nil {
		execClient.Close()
		return nil, wrapAuthError("creating workflows client", err)
	}

	return &Client{
		Project:        project,
		Region:         region,
		execClient:     execClient,
		workflowClient: wfClient,
	}, nil
}

// Close releases resources held by the client.
func (c *Client) Close() error {
	var errs []error
	if err := c.execClient.Close(); err != nil {
		errs = append(errs, err)
	}
	if err := c.workflowClient.Close(); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return fmt.Errorf("closing clients: %v", errs)
	}
	return nil
}

// ExecutionResult holds the result of a workflow execution.
type ExecutionResult struct {
	Name      string                 `json:"name"`
	State     string                 `json:"state"`
	Result    map[string]interface{} `json:"result,omitempty"`
	Error     string                 `json:"error,omitempty"`
	Duration  time.Duration          `json:"duration,omitempty"`
	StartTime time.Time              `json:"start_time"`
	EndTime   time.Time              `json:"end_time,omitempty"`
	Callbacks []CallbackInfo         `json:"callbacks,omitempty"`
}

// WorkflowInfo holds metadata about a workflow.
type WorkflowInfo struct {
	Name       string            `json:"name"`
	State      string            `json:"state"`
	RevisionID string            `json:"revision_id"`
	UpdateTime time.Time         `json:"update_time"`
	Labels     map[string]string `json:"labels,omitempty"`
	PamGated   bool              `json:"pam_gated"`
}

// ExecutionInfo holds metadata about a workflow execution.
type ExecutionInfo struct {
	ID        string    `json:"id"`
	State     string    `json:"state"`
	StartTime time.Time `json:"start_time"`
	EndTime   time.Time `json:"end_time,omitempty"`
	Duration  string    `json:"duration,omitempty"`
}

func (c *Client) workflowParent() string {
	return fmt.Sprintf("projects/%s/locations/%s", c.Project, c.Region)
}

func (c *Client) workflowName(name string) string {
	return fmt.Sprintf("projects/%s/locations/%s/workflows/%s", c.Project, c.Region, name)
}

// WorkflowDetail holds detailed metadata about a workflow, including labels.
type WorkflowDetail struct {
	Name           string            `json:"name"`
	State          string            `json:"state"`
	Labels         map[string]string `json:"labels,omitempty"`
	SourceContents string            `json:"source_contents,omitempty"`
}

// GetWorkflow retrieves metadata for a workflow, including labels and source.
func (c *Client) GetWorkflow(ctx context.Context, name string) (*WorkflowDetail, error) {
	wf, err := c.workflowClient.GetWorkflow(ctx, &workflowspb.GetWorkflowRequest{
		Name: c.workflowName(name),
	})
	if err != nil {
		return nil, wrapAuthError("getting workflow '"+name+"'", err)
	}
	return &WorkflowDetail{
		Name:           name,
		State:          wf.State.String(),
		Labels:         wf.Labels,
		SourceContents: wf.GetSourceContents(),
	}, nil
}

// WorkflowParam describes a parameter parsed from a workflow's source header.
type WorkflowParam struct {
	Name        string `json:"name"`
	Required    bool   `json:"required"`
	Description string `json:"description"`
}

// ParseParams extracts parameter definitions from the workflow source comments.
// It looks for lines matching: #   - param_name (required|optional): description
func ParseParams(source string) []WorkflowParam {
	var params []WorkflowParam
	inParams := false

	for _, line := range strings.Split(source, "\n") {
		trimmed := strings.TrimSpace(line)

		if trimmed == "# Parameters:" {
			inParams = true
			continue
		}

		if inParams {
			// End of parameters section: non-comment or non-parameter line
			if !strings.HasPrefix(trimmed, "#") {
				break
			}
			content := strings.TrimPrefix(trimmed, "#")
			content = strings.TrimSpace(content)
			if !strings.HasPrefix(content, "- ") {
				break
			}
			content = strings.TrimPrefix(content, "- ")

			// Parse: param_name (required|optional): description
			parts := strings.SplitN(content, ":", 2)
			if len(parts) != 2 {
				continue
			}
			nameAndReq := strings.TrimSpace(parts[0])
			desc := strings.TrimSpace(parts[1])

			required := false
			name := nameAndReq
			if idx := strings.Index(nameAndReq, " ("); idx > 0 {
				name = nameAndReq[:idx]
				qualifier := nameAndReq[idx:]
				required = strings.Contains(qualifier, "required")
			}

			params = append(params, WorkflowParam{
				Name:        name,
				Required:    required,
				Description: desc,
			})
		}
	}
	return params
}

// Execute starts a workflow and returns the execution name.
func (c *Client) Execute(ctx context.Context, workflowName string, args map[string]interface{}) (string, error) {
	argJSON, err := json.Marshal(args)
	if err != nil {
		return "", fmt.Errorf("marshaling arguments: %w", err)
	}

	exec, err := c.execClient.CreateExecution(ctx, &executionspb.CreateExecutionRequest{
		Parent: c.workflowName(workflowName),
		Execution: &executionspb.Execution{
			Argument: string(argJSON),
		},
	})
	if err != nil {
		return "", wrapAuthError("executing workflow '"+workflowName+"'", err)
	}

	return exec.Name, nil
}

// Run executes a workflow and waits for it to complete.
func (c *Client) Run(ctx context.Context, workflowName string, args map[string]interface{}) (string, *ExecutionResult, error) {
	execName, err := c.Execute(ctx, workflowName, args)
	if err != nil {
		return "", nil, err
	}

	result, err := c.WaitForCompletion(ctx, execName)
	return execName, result, err
}

// GetExecution retrieves the current state of an execution by its full name.
func (c *Client) GetExecution(ctx context.Context, executionName string) (*ExecutionResult, error) {
	exec, err := c.execClient.GetExecution(ctx, &executionspb.GetExecutionRequest{
		Name: executionName,
	})
	if err != nil {
		return nil, wrapAuthError("getting execution status", err)
	}

	result := &ExecutionResult{
		Name:      exec.Name,
		State:     exec.State.String(),
		StartTime: exec.StartTime.AsTime(),
	}

	if exec.EndTime != nil {
		result.EndTime = exec.EndTime.AsTime()
		result.Duration = result.EndTime.Sub(result.StartTime)
	}

	switch result.State {
	case "SUCCEEDED":
		var parsed map[string]interface{}
		if err := json.Unmarshal([]byte(exec.Result), &parsed); err != nil {
			result.Result = map[string]interface{}{"raw": exec.Result}
		} else {
			result.Result = parsed
		}
	case "FAILED":
		if exec.Error != nil {
			result.Error = exec.Error.Context
		}
	}

	return result, nil
}

// WaitForCompletion polls until the execution finishes.
func (c *Client) WaitForCompletion(ctx context.Context, executionName string) (*ExecutionResult, error) {
	pollInterval := 500 * time.Millisecond
	maxPoll := 2 * time.Second

	for {
		exec, err := c.execClient.GetExecution(ctx, &executionspb.GetExecutionRequest{
			Name: executionName,
		})
		if err != nil {
			return nil, wrapAuthError("checking execution status", err)
		}

		state := exec.State.String()

		if state != "ACTIVE" && state != "QUEUED" {
			result := &ExecutionResult{
				Name:      exec.Name,
				State:     state,
				StartTime: exec.StartTime.AsTime(),
			}

			if exec.EndTime != nil {
				result.EndTime = exec.EndTime.AsTime()
				result.Duration = result.EndTime.Sub(result.StartTime)
			}

			switch state {
			case "SUCCEEDED":
				var parsed map[string]interface{}
				if err := json.Unmarshal([]byte(exec.Result), &parsed); err != nil {
					result.Result = map[string]interface{}{"raw": exec.Result}
				} else {
					result.Result = parsed
				}
			case "FAILED":
				if exec.Error != nil {
					result.Error = exec.Error.Context
				}
			}

			return result, nil
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pollInterval):
		}

		if pollInterval < maxPoll {
			pollInterval = pollInterval * 2
			if pollInterval > maxPoll {
				pollInterval = maxPoll
			}
		}
	}
}

// ListExecutions returns recent executions for a specific workflow.
func (c *Client) ListExecutions(ctx context.Context, workflow string, limit int) ([]ExecutionInfo, error) {
	var result []ExecutionInfo

	it := c.execClient.ListExecutions(ctx, &executionspb.ListExecutionsRequest{
		Parent:   c.workflowName(workflow),
		PageSize: int32(limit),
	})

	for {
		exec, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, wrapAuthError("listing executions for '"+workflow+"'", err)
		}

		info := ExecutionInfo{
			State: exec.State.String(),
		}

		parts := strings.Split(exec.Name, "/")
		info.ID = parts[len(parts)-1]

		if exec.StartTime != nil {
			info.StartTime = exec.StartTime.AsTime()
		}
		if exec.EndTime != nil {
			info.EndTime = exec.EndTime.AsTime()
			d := info.EndTime.Sub(info.StartTime)
			info.Duration = d.Round(time.Millisecond).String()
		}

		result = append(result, info)

		if len(result) >= limit {
			break
		}
	}

	return result, nil
}

// List returns all workflows in the project/region, including PAM-gated status
// detected via GCP Resource Tags.
func (c *Client) List(ctx context.Context) ([]WorkflowInfo, error) {
	var result []WorkflowInfo

	it := c.workflowClient.ListWorkflows(ctx, &workflowspb.ListWorkflowsRequest{
		Parent: c.workflowParent(),
	})

	// fullNames tracks the full resource name for each workflow index
	var fullNames []string

	for {
		wf, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, wrapAuthError("listing workflows", err)
		}

		shortName := wf.Name
		if len(c.workflowParent()) < len(wf.Name) {
			shortName = wf.Name[len(c.workflowParent())+len("/workflows/"):]
		}

		info := WorkflowInfo{
			Name:       shortName,
			State:      wf.State.String(),
			RevisionID: wf.RevisionId,
			Labels:     wf.Labels,
		}
		if wf.UpdateTime != nil {
			info.UpdateTime = wf.UpdateTime.AsTime()
		}
		result = append(result, info)
		fullNames = append(fullNames, wf.Name)
	}

	// Resolve PAM-gated status from GCP Resource Tags (best-effort)
	pamGated := c.fetchPamGatedFlags(ctx, fullNames)
	for i := range result {
		result[i].PamGated = pamGated[i]
	}

	return result, nil
}

// fetchPamGatedFlags concurrently checks tag bindings for each workflow
// to determine if it has the pam-gated=true tag.
func (c *Client) fetchPamGatedFlags(ctx context.Context, fullNames []string) []bool {
	flags := make([]bool, len(fullNames))
	if len(fullNames) == 0 {
		return flags
	}

	httpClient, err := google.DefaultClient(ctx, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		return flags // best-effort
	}

	var wg sync.WaitGroup
	for i, name := range fullNames {
		wg.Add(1)
		go func(idx int, wfName string) {
			defer wg.Done()
			flags[idx] = checkPamGatedTag(ctx, httpClient, c.Region, wfName)
		}(i, name)
	}
	wg.Wait()

	return flags
}

// tagBindingsResponse is the JSON response from the tag bindings API.
type tagBindingsResponse struct {
	TagBindings []struct {
		TagValueNamespacedName string `json:"tagValueNamespacedName"`
	} `json:"tagBindings"`
}

// CheckPamGatedTag checks if a workflow has the pam-gated=true resource tag.
// It uses the workflow short name and constructs the full resource path internally.
func CheckPamGatedTag(ctx context.Context, project, region, workflowName string) bool {
	httpClient, err := google.DefaultClient(ctx, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		return false
	}
	fullName := fmt.Sprintf("projects/%s/locations/%s/workflows/%s", project, region, workflowName)
	return checkPamGatedTag(ctx, httpClient, region, fullName)
}

// checkPamGatedTag queries the Resource Manager tag bindings API for a workflow
// and returns true if it has a pam-gated/true tag.
func checkPamGatedTag(ctx context.Context, httpClient *http.Client, region, workflowFullName string) bool {
	parent := url.QueryEscape("//workflows.googleapis.com/" + workflowFullName)
	apiURL := fmt.Sprintf("https://%s-cloudresourcemanager.googleapis.com/v3/tagBindings?parent=%s", region, parent)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return false
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false
	}

	var result tagBindingsResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return false
	}

	for _, tb := range result.TagBindings {
		if strings.HasSuffix(tb.TagValueNamespacedName, "/pam-gated/true") {
			return true
		}
	}
	return false
}
