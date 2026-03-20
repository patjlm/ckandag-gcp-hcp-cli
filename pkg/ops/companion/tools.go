// Package companion implements the "ops sre-companion" interactive chat command.
package companion

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ckandag/gcp-hcp-cli/pkg/gcp/cloudrun"
	pamclient "github.com/ckandag/gcp-hcp-cli/pkg/gcp/pam"
	"github.com/ckandag/gcp-hcp-cli/pkg/gcp/workflows"
)

const wfRunPrefix = "wf_run_"

// ToolExecutor handles local tool execution on behalf of the remote agent.
type ToolExecutor struct {
	Project string
	Region  string
}

// Execute dispatches a tool call to the appropriate local handler.
func (e *ToolExecutor) Execute(ctx context.Context, toolName string, params map[string]interface{}) (json.RawMessage, error) {
	switch {
	case toolName == "workflows_invoker_grant_request_and_wait":
		return e.pamGrantAndWait(ctx, params)
	case strings.HasPrefix(toolName, wfRunPrefix):
		wfName := strings.TrimPrefix(toolName, wfRunPrefix)
		return e.wfRun(ctx, wfName, params)
	default:
		return nil, fmt.Errorf("unknown tool: %s", toolName)
	}
}

func (e *ToolExecutor) pamGrantAndWait(ctx context.Context, params map[string]interface{}) (json.RawMessage, error) {
	reason, _ := params["reason"].(string)
	if reason == "" {
		return nil, fmt.Errorf("reason is required")
	}

	durationStr, _ := params["duration"].(string)
	duration := 1 * time.Hour
	if durationStr != "" {
		var err error
		duration, err = time.ParseDuration(durationStr)
		if err != nil {
			return nil, fmt.Errorf("invalid duration %q: %w", durationStr, err)
		}
	}

	entitlement, _ := params["entitlement"].(string)

	client, err := pamclient.NewClient(ctx, e.Project)
	if err != nil {
		return nil, fmt.Errorf("creating PAM client: %w", err)
	}
	defer client.Close()

	// Resolve entitlement
	var entitlementName string
	if entitlement != "" {
		entitlementName = resolveEntitlementName(e.Project, entitlement)
	} else {
		entitlements, err := client.SearchEntitlements(ctx)
		if err != nil {
			return nil, fmt.Errorf("searching entitlements: %w", err)
		}
		if len(entitlements) == 0 {
			return nil, fmt.Errorf("no PAM entitlements found")
		}
		if len(entitlements) > 1 {
			return nil, fmt.Errorf("multiple entitlements found; specify entitlement parameter")
		}
		entitlementName = entitlements[0].Name
	}

	grant, err := client.CreateGrant(ctx, entitlementName, duration, reason)
	if err != nil {
		return nil, fmt.Errorf("requesting grant: %w", err)
	}

	if grant.State == "APPROVAL_AWAITED" {
		grant, err = client.WaitForGrant(ctx, grant.Name)
		if err != nil {
			return nil, fmt.Errorf("waiting for grant: %w", err)
		}
	}

	return json.Marshal(grant)
}

func (e *ToolExecutor) wfRun(ctx context.Context, workflowName string, params map[string]interface{}) (json.RawMessage, error) {
	wfClient, err := workflows.NewClient(ctx, e.Project, e.Region)
	if err != nil {
		return nil, fmt.Errorf("creating workflows client: %w", err)
	}
	defer wfClient.Close()

	// Build workflow arguments from params
	wfArgs := make(map[string]interface{})
	for k, v := range params {
		wfArgs[k] = v
	}

	_, result, err := wfClient.Run(ctx, workflowName, wfArgs)
	if err != nil {
		return nil, fmt.Errorf("running workflow %q: %w", workflowName, err)
	}

	return json.Marshal(result)
}

func resolveEntitlementName(project, entID string) string {
	if strings.Contains(entID, "/") {
		return entID
	}
	return fmt.Sprintf("projects/%s/locations/global/entitlements/%s", project, entID)
}

// DiscoverTools discovers pam-gated workflows and builds tool definitions.
func DiscoverTools(ctx context.Context, project, region string) ([]cloudrun.ToolDef, error) {
	wfClient, err := workflows.NewClient(ctx, project, region)
	if err != nil {
		return nil, fmt.Errorf("creating workflows client: %w", err)
	}
	defer wfClient.Close()

	wfs, err := wfClient.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing workflows: %w", err)
	}

	var tools []cloudrun.ToolDef

	// Add PAM grant tool (always available)
	tools = append(tools, cloudrun.ToolDef{
		Name:        "workflows_invoker_grant_request_and_wait",
		Description: "Request a PAM grant for the workflows-invoker entitlement and wait for activation. Only use this if a workflow execution failed with a permission error.",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"reason": map[string]interface{}{
					"type":        "string",
					"description": "Justification for the grant request",
				},
				"duration": map[string]interface{}{
					"type":        "string",
					"description": "Grant duration (e.g. 1h, 30m). Default: 1h",
				},
				"entitlement": map[string]interface{}{
					"type":        "string",
					"description": "PAM entitlement ID (optional, auto-discovered if omitted)",
				},
			},
			"required": []string{"reason"},
		},
	})

	// Add pam-gated workflows as tools, fetching source to extract parameters
	for _, wf := range wfs {
		if !wf.PamGated {
			continue
		}

		desc := fmt.Sprintf("Run the %s workflow (pam-gated)", wf.Name)
		if d, ok := wf.Labels["description"]; ok {
			desc = d
		}

		properties := map[string]interface{}{}
		var required []string

		// Fetch workflow source to extract parameter definitions
		if detail, err := wfClient.GetWorkflow(ctx, wf.Name); err == nil {
			params := workflows.ParseParams(detail.SourceContents)
			for _, p := range params {
				properties[p.Name] = map[string]interface{}{
					"type":        "string",
					"description": p.Description,
				}
				if p.Required {
					required = append(required, p.Name)
				}
			}
		}

		schema := map[string]interface{}{
			"type":       "object",
			"properties": properties,
		}
		if len(required) > 0 {
			schema["required"] = required
		}

		tools = append(tools, cloudrun.ToolDef{
			Name:        wfRunPrefix + wf.Name,
			Description: desc,
			Parameters:  schema,
		})
	}

	return tools, nil
}
