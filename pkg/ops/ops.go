// Package ops implements the "ops" command subtree for operational debugging
// of GKE-hosted OpenShift clusters via Cloud Workflows.
//
// This package is self-contained so it can be extracted as a standalone plugin
// binary (gcphcp-ops) without depending on the parent pkg/cli package.
package ops

import (
	"github.com/ckandag/gcp-hcp-cli/pkg/ops/companion"
	"github.com/ckandag/gcp-hcp-cli/pkg/ops/pam"
	"github.com/ckandag/gcp-hcp-cli/pkg/ops/wf"

	"github.com/spf13/cobra"
)

// NewOpsCmd creates the ops command tree. It can be registered as a subcommand
// of the root gcphcp command, or used as the root command of a standalone
// gcphcp-ops plugin binary.
func NewOpsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ops",
		Short: "Operational commands for cluster debugging and remediation",
		Long: `Operational commands for debugging, log analysis, and remediation
of GKE-hosted OpenShift control plane clusters.

Convenience commands (get, logs, describe) run Cloud Workflows under the hood.
Use 'ops wf' for direct workflow management.`,
	}

	cmd.AddCommand(newGetCmd())
	cmd.AddCommand(newLogsCmd())
	cmd.AddCommand(newDescribeCmd())
	cmd.AddCommand(newDiagnoseCmd())
	cmd.AddCommand(wf.NewWfCmd())
	cmd.AddCommand(pam.NewPamCmd())
	cmd.AddCommand(companion.NewCompanionCmd())

	return cmd
}
