package ops

import (
	"context"
	"io"
	"os"

	"github.com/ckandag/gcp-hcp-cli/pkg/gcp/workflows"
	"github.com/ckandag/gcp-hcp-cli/pkg/ops/pam"
	"github.com/spf13/cobra"
)

// checkPAMGate checks if a workflow is PAM-gated and ensures the user has an active grant.
func checkPAMGate(ctx context.Context, wfClient *workflows.Client, workflowName string, cmd *cobra.Command, stderr io.Writer) error {
	pamEntitlement, _ := cmd.Flags().GetString("pam-entitlement")

	pamGated := workflows.CheckPamGatedTag(ctx, wfClient.Project, wfClient.Region, workflowName)

	reason, _ := cmd.Flags().GetString("reason")

	return pam.EnsurePAMGrant(ctx, wfClient.Project, pamEntitlement, reason, pamGated, os.Stdin, stderr)
}
