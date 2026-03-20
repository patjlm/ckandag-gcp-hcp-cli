package companion

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ckandag/gcp-hcp-cli/pkg/gcp/cloudrun"
	"github.com/ergochat/readline"
	"github.com/spf13/cobra"
)

// ANSI escape codes for styling
const (
	bold    = "\033[1m"
	dim     = "\033[2m"
	italic  = "\033[3m"
	reset   = "\033[0m"
	cyan    = "\033[36m"
	yellow  = "\033[33m"
	green   = "\033[32m"
	red = "\033[31m"
)

// NewCompanionCmd creates the sre-companion interactive chat command.
func NewCompanionCmd() *cobra.Command {
	var (
		serviceName string
		timeout     time.Duration
	)

	cmd := &cobra.Command{
		Use:   "sre-companion",
		Short: "Interactive AI-powered SRE companion with local tool execution",
		Long: `Start an interactive chat session with the SRE companion agent.

The agent can investigate cluster issues using remote tools and request
local execution of pam-gated workflows. All local tool executions require
explicit user confirmation.

Available local tools are dynamically discovered from pam-gated workflows.

Examples:
  # Start an interactive session
  gcphcp ops sre-companion

  # Use a custom service name
  gcphcp ops sre-companion --service-name my-companion-agent`,

		RunE: func(cmd *cobra.Command, args []string) error {
			project, _ := cmd.Flags().GetString("project")
			region, _ := cmd.Flags().GetString("region")

			if project == "" {
				return fmt.Errorf("--project is required (or set GCPHCP_PROJECT)")
			}
			if region == "" {
				return fmt.Errorf("--region is required (or set GCPHCP_REGION)")
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()

			return runCompanion(ctx, project, region, serviceName, os.Stdout, os.Stderr)
		},
	}

	cmd.Flags().StringVar(&serviceName, "service-name", "sre-companion-agent", "Cloud Run service name")
	cmd.Flags().DurationVar(&timeout, "timeout", 10*time.Minute, "Maximum session duration")

	return cmd
}

func runCompanion(ctx context.Context, project, region, serviceName string, stdout, stderr io.Writer) error {
	client := cloudrun.NewClient(ctx, project, region)

	// Discover service URL
	fmt.Fprintf(stderr, "%sDiscovering %s service in %s/%s...%s\n", dim, serviceName, project, region, reset)
	serviceURL, err := client.DiscoverServiceURL(ctx, serviceName)
	if err != nil {
		return fmt.Errorf("discovering service: %w", err)
	}

	// Discover available tools from pam-gated workflows
	fmt.Fprintf(stderr, "%sDiscovering available tools...%s\n", dim, reset)
	tools, err := DiscoverTools(ctx, project, region)
	if err != nil {
		fmt.Fprintf(stderr, "%sWarning: could not discover tools: %v%s\n", yellow, err, reset)
		tools = nil
	}

	if len(tools) > 0 {
		fmt.Fprintf(stderr, "\n%sAvailable local tools:%s\n", bold, reset)
		for _, t := range tools {
			fmt.Fprintf(stderr, "  %s%s%s %s%s%s\n", green, t.Name, reset, dim, t.Description, reset)
		}
	}

	// Start session logger
	sessionLog, err := NewSessionLogger(project)
	if err != nil {
		fmt.Fprintf(stderr, "%sWarning: could not create session log: %v%s\n", yellow, err, reset)
	} else {
		defer sessionLog.Close()
		fmt.Fprintf(stderr, "%sSession log: %s%s\n", dim, sessionLog.Path(), reset)
	}

	var history []cloudrun.ChatMessage
	var maxIterations int

	fmt.Fprintf(stderr, "\n%sSRE Companion ready.%s Type your message, %s/resume%s to resume a session, or %sexit%s to quit.\n", bold, reset, italic, reset, italic, reset)

	// Set up readline with history and colored prompt
	prompt := fmt.Sprintf("\n%s───────────────────────────────────────────%s\n%s%s> %s", dim, reset, bold, project, reset)

	rl, err := readline.NewFromConfig(&readline.Config{
		Prompt:          prompt,
		InterruptPrompt: "^C",
		EOFPrompt:       "exit",
	})
	if err != nil {
		return fmt.Errorf("initializing readline: %w", err)
	}
	defer rl.Close()

	executor := &ToolExecutor{Project: project, Region: region}

	for {
		input, err := rl.ReadLine()
		if err != nil {
			break // EOF or interrupt
		}
		input = strings.TrimSpace(input)
		if input == "" {
			continue
		}
		if input == "exit" || input == "/quit" {
			break
		}

		if input == "/help" {
			fmt.Fprintf(stderr, "\n%sCommands:%s\n", bold, reset)
			fmt.Fprintf(stderr, "  %s/help%s              Show this help\n", green, reset)
			fmt.Fprintf(stderr, "  %s/resume%s            Resume a previous session\n", green, reset)
			fmt.Fprintf(stderr, "  %s/max-iterations N%s  Set max remote tool iterations per turn (current: %d)\n", green, reset, maxIterations)
			fmt.Fprintf(stderr, "  %s/quit%s, %sexit%s       Exit the session\n", green, reset, green, reset)
			fmt.Fprintf(stderr, "\n%sAnything else is sent to the SRE companion agent.%s\n", dim, reset)
			fmt.Fprintf(stderr, "%sLocal tool executions always require your confirmation.%s\n\n", dim, reset)
			continue
		}

		if strings.HasPrefix(input, "/max-iterations") {
			parts := strings.Fields(input)
			if len(parts) != 2 {
				fmt.Fprintf(stderr, "%sUsage: /max-iterations <N> (current: %d)%s\n", yellow, maxIterations, reset)
			} else if n, err := strconv.Atoi(parts[1]); err != nil || n < 1 {
				fmt.Fprintf(stderr, "%sInvalid value: %s%s\n", red, parts[1], reset)
			} else {
				maxIterations = n
				fmt.Fprintf(stderr, "%sMax iterations set to %d%s\n", dim, maxIterations, reset)
			}
			continue
		}

		if input == "/resume" {
			resumed, err := handleResume(project, rl, stderr)
			if err != nil {
				fmt.Fprintf(stderr, "%sError: %v%s\n", red, err, reset)
			} else if resumed != nil {
				history = resumed
				printHistory(history, stdout)
			}
			continue
		}

		history = append(history, cloudrun.ChatMessage{Role: "user", Content: input})
		sessionLog.Log(SessionEvent{Type: "user", Content: input})

		assistantText, err := chatTurn(ctx, client, serviceURL, history, tools, maxIterations, executor, sessionLog, rl, stdout, stderr)
		if err != nil {
			fmt.Fprintf(stderr, "%sError: %v%s\n", red, err, reset)
			sessionLog.Log(SessionEvent{Type: "error", Error: err.Error()})
			// Remove the failed user message from history
			history = history[:len(history)-1]
			continue
		}

		if assistantText != "" {
			history = append(history, cloudrun.ChatMessage{Role: "assistant", Content: assistantText})
			sessionLog.Log(SessionEvent{Type: "assistant", Content: assistantText})
		}

		fmt.Fprintln(stdout)
	}

	fmt.Fprintf(stderr, "\n%sGoodbye!%s\n", dim, reset)
	return nil
}

// chatTurn handles a single conversational turn, including tool-call sub-loops.
// Returns the accumulated assistant text for this turn.
func chatTurn(
	ctx context.Context,
	client *cloudrun.Client,
	serviceURL string,
	history []cloudrun.ChatMessage,
	tools []cloudrun.ToolDef,
	maxIterations int,
	executor *ToolExecutor,
	sessionLog *SessionLogger,
	rl *readline.Instance,
	stdout, stderr io.Writer,
) (string, error) {
	req := cloudrun.ChatRequest{
		History:       history,
		Tools:         tools,
		MaxIterations: maxIterations,
	}

	var assistantText strings.Builder

	for {
		step := 0
		result, err := client.ChatStream(ctx, serviceURL, req, func(event cloudrun.StreamEvent) {
			switch event.Event {
			case "text":
				fmt.Fprint(stdout, event.Content)
				assistantText.WriteString(event.Content)
			case "tool_call":
				step++
				fmt.Fprintf(stderr, "\n  %s[%d]%s %s%s%s\n", cyan, step, reset, dim, formatToolRequest(event.Tool, event.Parameters), reset)
			case "tool_result":
				result := strings.TrimSpace(string(event.Result))
				if len(result) > 80 {
					result = result[:80] + "..."
				}
				fmt.Fprintf(stderr, "      %s-> %s%s\n", dim, result, reset)
			}
		})
		if err != nil {
			return "", err
		}

		if result.PendingToolCall == nil {
			// Turn complete
			return assistantText.String(), nil
		}

		// Agent requested a local tool — prompt user for confirmation
		tc := result.PendingToolCall
		sessionLog.Log(SessionEvent{Type: "tool_call", Tool: tc.Tool, Parameters: tc.Parameters})
		fmt.Fprintf(stderr, "\n  %sExecute %s%s%s? %s[y/N]%s ", yellow, bold, tc.Tool, reset+yellow, dim, reset)

		rl.SetPrompt("")
		answer, err := rl.ReadLine()
		if err != nil {
			answer = "n"
		}
		answer = strings.TrimSpace(strings.ToLower(answer))
		confirmed := answer == "y" || answer == "yes"

		if !confirmed {
			sessionLog.Log(SessionEvent{Type: "tool_confirm", Tool: tc.Tool, Content: "denied"})
			fmt.Fprintf(stderr, "  %sDenied.%s\n", dim, reset)
			req = cloudrun.ChatRequest{
				History:       history,
				Tools:         tools,
				MaxIterations: maxIterations,
				ToolResult: &cloudrun.ChatToolResult{
					CallID:     tc.CallID,
					Tool:       tc.Tool,
					Parameters: tc.Parameters,
					Error:      "user denied execution",
				},
			}
			// Restore main prompt
			rl.SetPrompt(fmt.Sprintf("\n%s───────────────────────────────────────────%s\n%s%s> %s",
				dim, reset, bold, executor.Project, reset))
			continue
		}

		// Execute the tool locally
		fmt.Fprintf(stderr, "  %sExecuting %s...%s\n", dim, tc.Tool, reset)
		toolResult, toolErr := executor.Execute(ctx, tc.Tool, tc.Parameters)

		toolResultMsg := &cloudrun.ChatToolResult{
			CallID:     tc.CallID,
			Tool:       tc.Tool,
			Parameters: tc.Parameters,
		}
		sessionLog.Log(SessionEvent{Type: "tool_confirm", Tool: tc.Tool, Content: "approved"})
		if toolErr != nil {
			toolResultMsg.Error = toolErr.Error()
			sessionLog.Log(SessionEvent{Type: "tool_result", Tool: tc.Tool, Error: toolErr.Error()})
			fmt.Fprintf(stderr, "  %sError: %v%s\n", red, toolErr, reset)
		} else {
			toolResultMsg.Result = toolResult
			sessionLog.Log(SessionEvent{Type: "tool_result", Tool: tc.Tool, Result: toolResult})
			truncated := string(toolResult)
			if len(truncated) > 200 {
				truncated = truncated[:200] + "..."
			}
			fmt.Fprintf(stderr, "  %s-> %s%s\n", dim, truncated, reset)
		}

		req = cloudrun.ChatRequest{
			History:       history,
			Tools:         tools,
			MaxIterations: maxIterations,
			ToolResult:    toolResultMsg,
		}

		// Restore main prompt
		rl.SetPrompt(fmt.Sprintf("\n%s───────────────────────────────────────────%s\n%s%s> %s",
			dim, reset, bold, executor.Project, reset))
	}
}

func printHistory(history []cloudrun.ChatMessage, w io.Writer) {
	fmt.Fprintf(w, "\n%s── Resumed session (%d messages) ──%s\n\n", dim, len(history), reset)
	for _, msg := range history {
		switch msg.Role {
		case "user":
			fmt.Fprintf(w, "%s> %s%s\n", bold, msg.Content, reset)
		case "assistant":
			fmt.Fprintf(w, "%s\n", msg.Content)
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "%s── End of history ──%s\n", dim, reset)
}

func handleResume(project string, rl *readline.Instance, stderr io.Writer) ([]cloudrun.ChatMessage, error) {
	sessions, err := ListSessions(project)
	if err != nil {
		return nil, err
	}
	if len(sessions) == 0 {
		fmt.Fprintf(stderr, "%sNo past sessions found for %s.%s\n", dim, project, reset)
		return nil, nil
	}

	fmt.Fprintf(stderr, "\n%sPast sessions:%s\n", bold, reset)
	for i, s := range sessions {
		lastInput := s.LastInput
		if len(lastInput) > 60 {
			lastInput = lastInput[:60] + "..."
		}
		fmt.Fprintf(stderr, "  %s%d)%s %s%s%s  %s\"%s\"%s\n",
			cyan, i+1, reset,
			dim, s.UpdatedAt.Local().Format("2006-01-02 15:04"), reset,
			italic, lastInput, reset)
	}

	fmt.Fprintf(stderr, "\n%sSelect session [1-%d] or press Enter to cancel:%s ", yellow, len(sessions), reset)
	rl.SetPrompt("")
	answer, err := rl.ReadLine()
	// Restore prompt
	rl.SetPrompt(fmt.Sprintf("\n%s───────────────────────────────────────────%s\n%s%s> %s",
		dim, reset, bold, project, reset))
	if err != nil || strings.TrimSpace(answer) == "" {
		return nil, nil
	}

	idx, err := strconv.Atoi(strings.TrimSpace(answer))
	if err != nil || idx < 1 || idx > len(sessions) {
		return nil, fmt.Errorf("invalid selection: %s", answer)
	}

	return LoadHistory(sessions[idx-1].Path)
}

func formatToolRequest(tool string, params map[string]interface{}) string {
	if len(params) == 0 {
		return tool
	}
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return tool
	}
	return fmt.Sprintf("%s(%s)", tool, string(paramsJSON))
}
