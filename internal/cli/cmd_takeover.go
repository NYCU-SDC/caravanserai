package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"github.com/spf13/cobra"
)

const operatorTokenEnv = "CARA_OPERATOR_TOKEN"

// takeoverFence finds the assignment a takeover of project is about: the one it
// holds, or the released one whose obligation is blocking it.
func takeoverFence(project v1.Project) (v1.AssignmentIdentity, error) {
	if project.Status.NodeRef != "" {
		return v1.AssignmentIdentity{
			ProjectUID:           project.ObjectMeta.UID,
			NodeName:             project.Status.NodeRef,
			AssignmentGeneration: project.Status.AssignmentGeneration,
		}, nil
	}
	if o := project.Status.Recovery.BlockingObligation(); o != nil {
		return o.Assignment, nil
	}
	return v1.AssignmentIdentity{}, errors.New("nothing is blocking this project")
}

// NewTakeoverCmd returns the "takeover" command.
func NewTakeoverCmd() *cobra.Command {
	var (
		yes       bool
		note      string
		requestID string
	)
	cmd := &cobra.Command{
		Use:   "takeover <project>",
		Short: "Let a project move off a node that will not report back",
		Long: `takeover overrides the release gate for a project stuck on a lost node, so
it can be scheduled on another node and restored from its latest backup.

This is not proof the old node stopped. If that node is still running, the
workload runs on both nodes until it reconnects and cleans up, and writes made
since the last backup are not carried over.

Reads the operator token from ` + operatorTokenEnv + `.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			serverURL, _ := cmd.Root().PersistentFlags().GetString("server")
			client := NewClient(serverURL)
			name := args[0]

			project, err := client.GetProject(cmd.Context(), name)
			if err != nil {
				return fmt.Errorf("get project %q: %w", name, err)
			}
			fence, err := takeoverFence(project)
			if err != nil {
				return fmt.Errorf("take over project %q: %w", name, err)
			}

			if !yes {
				fmt.Fprintf(cmd.OutOrStdout(),
					"Take over project %q from node %s (generation %d)?\n"+
						"If the node is still running, the workload will run on two nodes. [y/N] ",
					name, fence.NodeName, fence.AssignmentGeneration)
				answer, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
				if !strings.EqualFold(strings.TrimSpace(answer), "y") {
					return errors.New("takeover not confirmed")
				}
			}

			if requestID == "" {
				b := make([]byte, 16)
				if _, err := rand.Read(b); err != nil {
					return fmt.Errorf("generate request ID: %w", err)
				}
				requestID = hex.EncodeToString(b)
			}
			record, err := client.takeover(cmd.Context(), name, fence, requestID, note, os.Getenv(operatorTokenEnv))
			if err != nil {
				return fmt.Errorf("take over project %q: %w", name, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "project/%s taken over from %s: %s (request %s)\n",
				name, record.Assignment.NodeName, record.Outcome, record.RequestID)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation")
	cmd.Flags().StringVar(&note, "note", "", "optional note for the audit record")
	cmd.Flags().StringVar(&requestID, "request-id", "", "reuse the ID of an earlier attempt to retry it safely")
	return cmd
}

// NewTakeoverAuditCmd returns the "takeover-audit" command.
func NewTakeoverAuditCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "takeover-audit <project>",
		Short: "List recorded operator takeovers of a project",
		Long:  "takeover-audit lists a project's takeovers, including after it is deleted.\n\nReads the operator token from " + operatorTokenEnv + ".",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			serverURL, _ := cmd.Root().PersistentFlags().GetString("server")
			records, err := NewClient(serverURL).takeoverAudit(cmd.Context(), args[0], os.Getenv(operatorTokenEnv))
			if err != nil {
				return fmt.Errorf("list takeovers of %q: %w", args[0], err)
			}
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(records)
		},
	}
}

func (c *Client) takeover(ctx context.Context, name string, fence v1.AssignmentIdentity, requestID, note, token string) (v1.TakeoverAuditRecord, error) {
	body, err := json.Marshal(map[string]any{
		"uid":                  fence.ProjectUID,
		"nodeRef":              fence.NodeName,
		"assignmentGeneration": fence.AssignmentGeneration,
		"requestID":            requestID,
		"note":                 note,
	})
	if err != nil {
		return v1.TakeoverAuditRecord{}, fmt.Errorf("encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/api/v1/projects/"+name+"/recovery/takeover", bytes.NewReader(body))
	if err != nil {
		return v1.TakeoverAuditRecord{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return v1.TakeoverAuditRecord{}, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	if err := checkStatus(resp); err != nil {
		return v1.TakeoverAuditRecord{}, err
	}

	var out struct {
		Record v1.TakeoverAuditRecord `json:"record"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return v1.TakeoverAuditRecord{}, fmt.Errorf("decode response: %w", err)
	}
	return out.Record, nil
}

func (c *Client) takeoverAudit(ctx context.Context, name, token string) ([]v1.TakeoverAuditRecord, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.BaseURL+"/api/v1/projects/"+name+"/recovery/takeover-audit", nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	if err := checkStatus(resp); err != nil {
		return nil, err
	}

	var records []v1.TakeoverAuditRecord
	if err := json.NewDecoder(resp.Body).Decode(&records); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return records, nil
}
