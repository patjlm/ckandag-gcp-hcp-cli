package wf

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"time"

	"github.com/ckandag/gcp-hcp-cli/pkg/gcp/workflows"
	"github.com/ckandag/gcp-hcp-cli/pkg/ops/pam"
	"github.com/ckandag/gcp-hcp-cli/pkg/output"
	"github.com/spf13/cobra"
)

func newRunCmd() *cobra.Command {
	var (
		data    string
		async   bool
		timeout time.Duration
	)

	cmd := &cobra.Command{
		Use:   "run <workflow-name>",
		Short: "Execute a Cloud Workflow",
		Long: `Execute a Cloud Workflow by name.

By default, waits for the workflow to complete and prints the result.
Use --async to start the workflow and return immediately.

Examples:
  # Run and wait for result
  gcphcp ops wf run get --data '{"resource_type": "pods", "namespace": "hypershift"}'

  # Run asynchronously (returns immediately)
  gcphcp ops wf run describe --data '{"resource_type": "pods", "name": "etcd-0", "namespace": "hypershift"}' --async

  # Run with a timeout
  gcphcp ops wf run get --data '{"resource_type": "nodes"}' --timeout 60s`,

		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			workflowName := args[0]

			project, _ := cmd.Flags().GetString("project")
			region, _ := cmd.Flags().GetString("region")
			outputFormat, _ := cmd.Flags().GetString("output")

			if project == "" {
				return fmt.Errorf("--project is required (or set GCPHCP_PROJECT)")
			}
			if region == "" {
				return fmt.Errorf("--region is required (or set GCPHCP_REGION)")
			}

			var parsedData map[string]interface{}
			if data != "" {
				if err := json.Unmarshal([]byte(data), &parsedData); err != nil {
					return fmt.Errorf("invalid --data JSON: %w", err)
				}
			} else {
				parsedData = map[string]interface{}{}
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()

			client, err := workflows.NewClient(ctx, project, region)
			if err != nil {
				return fmt.Errorf("creating client: %w", err)
			}
			defer client.Close()

			// Check PAM gate
			pamEntitlement, _ := cmd.Flags().GetString("pam-entitlement")
			pamGated := workflows.CheckPamGatedTag(ctx, project, region, workflowName)
			reason, _ := cmd.Flags().GetString("reason")
			if err := pam.EnsurePAMGrant(ctx, project, pamEntitlement, reason, pamGated, os.Stdin, os.Stderr); err != nil {
				return err
			}

			fmt.Fprintf(os.Stderr, "Executing workflow: %s\n", workflowName)

			execName, err := client.Execute(ctx, workflowName, parsedData)
			if err != nil {
				return fmt.Errorf("executing workflow: %w", err)
			}

			execID := path.Base(execName)
			fmt.Fprintf(os.Stderr, "Execution: %s\n", execID)

			if async {
				fmt.Fprintf(os.Stderr, "Workflow started. Check status with:\n")
				fmt.Fprintf(os.Stderr, "  gcphcp ops wf status %s %s\n", workflowName, execID)
				return nil
			}

			fmt.Fprintf(os.Stderr, "Waiting for completion... (Ctrl+C to detach)\n")

			result, err := client.WaitForCompletion(ctx, execName)
			if err != nil {
				return fmt.Errorf("waiting for workflow: %w\n\nCheck status with: gcphcp ops wf status %s %s", err, workflowName, execID)
			}

			fmt.Fprintf(os.Stderr, "State: %s  Duration: %s\n", result.State, result.Duration.Round(time.Millisecond))

			if result.State == "FAILED" {
				fmt.Fprintf(os.Stderr, "Error: %s\n", result.Error)
				os.Exit(1)
			}

			format := output.ParseFormat(outputFormat)
			return output.PrintResult(os.Stdout, format, result.Result)
		},
	}

	cmd.Flags().StringVar(&data, "data", "", "JSON data to pass as workflow arguments")
	cmd.Flags().BoolVar(&async, "async", false, "Start workflow and return immediately without waiting")
	cmd.Flags().DurationVar(&timeout, "timeout", 5*time.Minute, "Maximum time to wait for workflow completion")

	return cmd
}
