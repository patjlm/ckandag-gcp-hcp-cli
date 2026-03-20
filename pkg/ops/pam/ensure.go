package pam

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	pamclient "github.com/ckandag/gcp-hcp-cli/pkg/gcp/pam"
)

// EnsurePAMGrant checks if a workflow requires PAM and ensures the user has an active grant.
// It is a no-op if pamGated is false and no explicit entitlement is provided.
func EnsurePAMGrant(ctx context.Context, project, pamEntitlement, reason string, pamGated bool, stdin io.Reader, stderr io.Writer) error {
	if !pamGated && pamEntitlement == "" {
		return nil
	}

	client, err := pamclient.NewClient(ctx, project)
	if err != nil {
		return fmt.Errorf("creating PAM client: %w", err)
	}
	defer client.Close()

	// Resolve entitlement
	entitlementName := pamEntitlement
	if entitlementName != "" {
		entitlementName = resolveEntitlementName(project, entitlementName)
	} else {
		entitlementName, err = discoverEntitlement(ctx, client)
		if err != nil {
			return err
		}
	}

	// Check for existing active grant
	grants, err := client.SearchGrants(ctx, entitlementName, pamclient.RelationshipCreated)
	if err != nil {
		return fmt.Errorf("searching grants: %w", err)
	}
	for _, g := range grants {
		if g.State == "ACTIVE" || g.State == "ACTIVATED" {
			fmt.Fprintf(stderr, "Active PAM grant found: %s\n", g.ShortName())
			return nil
		}
	}

	// No active grant — prompt user
	fmt.Fprintf(stderr, "This workflow requires PAM access (entitlement: %s)\n", pamclient.ShortEntitlementName(entitlementName))

	if reason == "" {
		return fmt.Errorf("PAM grant required but no --reason provided\n\n" +
			"  Add --reason \"your justification\" to request a grant automatically")
	}

	fmt.Fprintf(stderr, "Request a PAM grant? [Y/n]: ")
	scanner := bufio.NewScanner(stdin)
	if scanner.Scan() {
		answer := strings.TrimSpace(scanner.Text())
		if answer != "" && !strings.EqualFold(answer, "y") && !strings.EqualFold(answer, "yes") {
			return fmt.Errorf("PAM grant required; aborting\n\n" +
				"  Request a grant manually: gcphcp ops pam request --reason \"...\"")
		}
	}

	fmt.Fprintf(stderr, "Requesting PAM grant...\n")
	grant, err := client.CreateGrant(ctx, entitlementName, time.Hour, reason)
	if err != nil {
		return fmt.Errorf("requesting grant: %w", err)
	}

	fmt.Fprintf(stderr, "Grant created: %s (state: %s)\n", grant.ShortName(), grant.State)

	if grant.State == "APPROVAL_AWAITED" {
		fmt.Fprintf(stderr, "Waiting for approval... (Ctrl+C to cancel)\n")
		grant, err = client.WaitForGrant(ctx, grant.Name)
		if err != nil {
			return err
		}
	}

	switch grant.State {
	case "ACTIVE", "ACTIVATED":
		fmt.Fprintf(stderr, "Grant approved and active!\n")
		return nil
	case "DENIED":
		return fmt.Errorf("PAM grant was denied\n\n" +
			"  Contact your approver or try again with a different justification")
	default:
		return fmt.Errorf("PAM grant in unexpected state: %s", grant.State)
	}
}
