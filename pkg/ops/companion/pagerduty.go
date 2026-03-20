package companion

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const pagerdutyAPI = "https://api.pagerduty.com"

// PDClient is a lightweight PagerDuty REST API client.
type PDClient struct {
	token      string
	httpClient *http.Client
}

// NewPDClient creates a PagerDuty client, resolving the token from:
// 1. PAGERDUTY_TOKEN env var
// 2. ~/.config/pagerduty-cli/config.json (pd CLI config)
func NewPDClient() (*PDClient, error) {
	token := os.Getenv("PAGERDUTY_TOKEN")
	if token == "" {
		token = readPDCLIToken()
	}
	if token == "" {
		return nil, fmt.Errorf("PagerDuty token not found. Set PAGERDUTY_TOKEN or login with the pd CLI")
	}
	return &PDClient{
		token:      token,
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}, nil
}

func readPDCLIToken() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".config", "pagerduty-cli", "config.json"))
	if err != nil {
		return ""
	}
	var config map[string]interface{}
	if err := json.Unmarshal(data, &config); err != nil {
		return ""
	}
	if accounts, ok := config["accounts"].([]interface{}); ok && len(accounts) > 0 {
		if acct, ok := accounts[0].(map[string]interface{}); ok {
			if token, ok := acct["token"].(string); ok {
				return token
			}
		}
	}
	if token, ok := config["token"].(string); ok {
		return token
	}
	return ""
}

func (c *PDClient) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pagerdutyAPI+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Token token="+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("PagerDuty API request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("PagerDuty API returned %d: %s", resp.StatusCode, string(body))
	}
	return body, nil
}

// PDIncidentContext holds the formatted incident data for the agent.
type PDIncidentContext struct {
	IncidentID string
	Summary    string // formatted text to inject into conversation
}

// FetchIncidentContext retrieves an incident and its alerts, formatted for the agent.
func (c *PDClient) FetchIncidentContext(ctx context.Context, incidentID string) (*PDIncidentContext, error) {
	// Fetch incident
	incData, err := c.get(ctx, "/incidents/"+incidentID)
	if err != nil {
		return nil, fmt.Errorf("fetching incident: %w", err)
	}

	var incResp struct {
		Incident struct {
			ID          string `json:"id"`
			Title       string `json:"title"`
			Status      string `json:"status"`
			Urgency     string `json:"urgency"`
			Description string `json:"description"`
			CreatedAt   string `json:"created_at"`
			HTMLURL     string `json:"html_url"`
			Service     struct {
				Summary string `json:"summary"`
			} `json:"service"`
			Teams []struct {
				Summary string `json:"summary"`
			} `json:"teams"`
			Assignments []struct {
				Assignee struct {
					Summary string `json:"summary"`
				} `json:"assignee"`
			} `json:"assignments"`
		} `json:"incident"`
	}
	if err := json.Unmarshal(incData, &incResp); err != nil {
		return nil, fmt.Errorf("parsing incident: %w", err)
	}
	inc := incResp.Incident

	// Fetch alerts
	alertsData, err := c.get(ctx, "/incidents/"+incidentID+"/alerts")
	if err != nil {
		return nil, fmt.Errorf("fetching alerts: %w", err)
	}

	var alertsResp struct {
		Alerts []struct {
			AlertKey string `json:"alert_key"`
			Status   string `json:"status"`
			Summary  string `json:"summary"`
			Severity string `json:"severity"`
			Body     struct {
				Details map[string]interface{} `json:"details"`
			} `json:"body"`
		} `json:"alerts"`
	}
	if err := json.Unmarshal(alertsData, &alertsResp); err != nil {
		return nil, fmt.Errorf("parsing alerts: %w", err)
	}

	// Format context
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("## PagerDuty Incident %s\n", inc.ID))
	sb.WriteString(fmt.Sprintf("- **Title**: %s\n", inc.Title))
	sb.WriteString(fmt.Sprintf("- **Status**: %s\n", inc.Status))
	sb.WriteString(fmt.Sprintf("- **Urgency**: %s\n", inc.Urgency))
	sb.WriteString(fmt.Sprintf("- **Service**: %s\n", inc.Service.Summary))
	sb.WriteString(fmt.Sprintf("- **Created**: %s\n", inc.CreatedAt))
	sb.WriteString(fmt.Sprintf("- **URL**: %s\n", inc.HTMLURL))

	if len(inc.Assignments) > 0 {
		var names []string
		for _, a := range inc.Assignments {
			names = append(names, a.Assignee.Summary)
		}
		sb.WriteString(fmt.Sprintf("- **Assigned to**: %s\n", strings.Join(names, ", ")))
	}

	if inc.Description != "" {
		sb.WriteString(fmt.Sprintf("\n### Description\n%s\n", inc.Description))
	}

	if len(alertsResp.Alerts) > 0 {
		sb.WriteString(fmt.Sprintf("\n### Alerts (%d)\n", len(alertsResp.Alerts)))
		for i, alert := range alertsResp.Alerts {
			sb.WriteString(fmt.Sprintf("\n**Alert %d**: %s\n", i+1, alert.Summary))
			sb.WriteString(fmt.Sprintf("- Status: %s, Severity: %s\n", alert.Status, alert.Severity))
			if alert.AlertKey != "" {
				sb.WriteString(fmt.Sprintf("- Alert Key: %s\n", alert.AlertKey))
			}
			if len(alert.Body.Details) > 0 {
				detailsJSON, _ := json.MarshalIndent(alert.Body.Details, "  ", "  ")
				sb.WriteString(fmt.Sprintf("- Details:\n  %s\n", string(detailsJSON)))
			}
		}
	}

	return &PDIncidentContext{
		IncidentID: inc.ID,
		Summary:    sb.String(),
	}, nil
}
