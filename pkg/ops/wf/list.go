package wf

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/ckandag/gcp-hcp-cli/pkg/output"
	"github.com/ckandag/gcp-hcp-cli/pkg/gcp/workflows"
	"github.com/spf13/cobra"
)

func newListCmd() *cobra.Command {
	var (
		timeout time.Duration
		limit   int
	)

	cmd := &cobra.Command{
		Use:   "list [workflow]",
		Short: "List workflows or execution history",
		Long: `Without arguments, lists all deployed Cloud Workflows.
With a workflow name, lists recent execution history for that workflow.

Examples:
  # List all deployed workflows
  gcphcp ops wf list

  # List recent executions for the 'get' workflow
  gcphcp ops wf list get

  # List last 5 executions
  gcphcp ops wf list get --limit 5

  # JSON output
  gcphcp ops wf list get -o json`,

		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			project, _ := cmd.Flags().GetString("project")
			region, _ := cmd.Flags().GetString("region")
			outputFormat, _ := cmd.Flags().GetString("output")

			if project == "" {
				return fmt.Errorf("--project is required (or set GCPHCP_PROJECT)")
			}
			if region == "" {
				return fmt.Errorf("--region is required (or set GCPHCP_REGION)")
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()

			client, err := workflows.NewClient(ctx, project, region)
			if err != nil {
				return fmt.Errorf("creating client: %w", err)
			}
			defer client.Close()

			if len(args) == 1 {
				return listExecutions(ctx, client, args[0], limit, outputFormat)
			}
			return listWorkflows(ctx, client, outputFormat)
		},
	}

	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Second, "Maximum time to wait")
	cmd.Flags().IntVar(&limit, "limit", 10, "Maximum number of executions to show")

	return cmd
}

func listWorkflows(ctx context.Context, client *workflows.Client, outputFormat string) error {
	wfs, err := client.List(ctx)
	if err != nil {
		return fmt.Errorf("listing workflows: %w", err)
	}

	format := output.ParseFormat(outputFormat)
	if format == output.FormatJSON {
		return output.PrintJSON(os.Stdout, wfs)
	}

	if len(wfs) == 0 {
		fmt.Fprintln(os.Stdout, "No workflows found.")
		return nil
	}

	t := output.NewTable(os.Stdout, "NAME", "STATE", "PAM_GATED", "REVISION", "UPDATED")
	for _, wf := range wfs {
		updated := wf.UpdateTime.Format(time.RFC3339)
		pamGated := ""
		if wf.PamGated {
			pamGated = "yes"
		}
		t.AddRow(wf.Name, wf.State, pamGated, wf.RevisionID, updated)
	}
	return t.Flush()
}

func listExecutions(ctx context.Context, client *workflows.Client, workflow string, limit int, outputFormat string) error {
	execs, err := client.ListExecutions(ctx, workflow, limit)
	if err != nil {
		return fmt.Errorf("listing executions: %w", err)
	}

	format := output.ParseFormat(outputFormat)
	if format == output.FormatJSON {
		return output.PrintJSON(os.Stdout, execs)
	}

	if len(execs) == 0 {
		fmt.Fprintf(os.Stdout, "No executions found for workflow '%s'.\n", workflow)
		return nil
	}

	t := output.NewTable(os.Stdout, "ID", "STATE", "STARTED", "DURATION")
	for _, e := range execs {
		started := output.Age(e.StartTime.Format(time.RFC3339)) + " ago"
		duration := e.Duration
		if duration == "" {
			duration = "running"
		}
		t.AddRow(e.ID, e.State, started, duration)
	}
	return t.Flush()
}
