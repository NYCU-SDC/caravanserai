package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"github.com/spf13/cobra"
)

// NewWaitCmd returns the "wait" subcommand.
func NewWaitCmd() *cobra.Command {
	var (
		forFlag string
		timeout time.Duration
	)
	cmd := &cobra.Command{
		Use:   "wait <project|node> <name> --for=<condition>",
		Short: "Wait until a Project or Node reaches a state",
		Long: `wait reads the resource every 2 seconds until the condition holds, and prints
each new state to stderr so a wait that does not end explains itself.

Conditions:
  phase=<Phase>       Project phase (Pending, Scheduled, Running, Failed,
                      Terminating, Terminated); for a Node, its state
                      (Ready, NotReady, Draining). state=<State> also works.
  condition=<Type>    a condition of that type has status True
  delete              the resource no longer exists
  heartbeat           Node only: a heartbeat newer than the one it had when
                      the wait started (compared on the server's clock)

Exit codes:
  0  the condition holds
  1  any other error, including a Project reaching Failed while waiting for
     another phase, or a refused request (401, 403)
  2  timed out; the message gives the last state and the last error
  3  the resource does not exist (except with --for=delete, where that is
     success)

An unreachable server or a 5xx answer does not end the wait: it is retried
until the timeout.

Examples:
  caractl wait project web --for=phase=Running --timeout 2m
  caractl wait project web --for=delete
  caractl wait node node-a --for=heartbeat
  caractl wait node node-a --for=condition=Ready`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			kind, err := waitKind(args[0])
			if err != nil {
				return err
			}
			cond, err := parseWaitFor(kind, forFlag)
			if err != nil {
				return err
			}
			cmd.SilenceUsage = true
			serverURL, _ := cmd.Root().PersistentFlags().GetString("server")
			client := NewClient(serverURL)
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			return waitFor(ctx, client, kind, args[1], cond, cmd.ErrOrStderr(), waitInterval)
		},
	}
	cmd.Flags().StringVar(&forFlag, "for", "", "condition to wait for: phase=<Phase>, condition=<Type>, delete, heartbeat")
	cmd.Flags().DurationVar(&timeout, "timeout", 5*time.Minute, "how long to wait")
	_ = cmd.MarkFlagRequired("for")
	return cmd
}

// waitInterval is how often a wait reads the resource.
const waitInterval = 2 * time.Second

func waitKind(arg string) (string, error) {
	switch strings.ToLower(arg) {
	case "project", "projects":
		return "project", nil
	case "node", "nodes":
		return "node", nil
	}
	return "", fmt.Errorf("cannot wait for %q: valid kinds are project, node", arg)
}

// waitCondition is a parsed --for.
type waitCondition struct {
	what  string // phase, condition, delete, heartbeat
	value string
	// specRevision, when set on a Project phase condition, also requires the
	// Agent to have reported this metadata.specRevision running. Without it,
	// an update to a Project that is already Running would count as done
	// before its new spec runs (CARA-134).
	specRevision int64
}

func (c waitCondition) String() string {
	s := c.what
	if c.value != "" {
		s += "=" + c.value
	}
	if c.specRevision > 0 {
		s += fmt.Sprintf(" at spec revision %d", c.specRevision)
	}
	return s
}

var nodeStates = []v1.NodeState{v1.NodeStateReady, v1.NodeStateNotReady, v1.NodeStateDraining}

func parseWaitFor(kind, s string) (waitCondition, error) {
	what, value, hasValue := strings.Cut(s, "=")
	switch {
	case what == "delete" && !hasValue:
		return waitCondition{what: "delete"}, nil
	case what == "heartbeat" && !hasValue:
		if kind != "node" {
			return waitCondition{}, errors.New("--for=heartbeat applies only to a node")
		}
		return waitCondition{what: "heartbeat"}, nil
	case what == "condition" && value != "":
		return waitCondition{what: "condition", value: value}, nil
	case (what == "phase" || what == "state") && value != "":
		if kind == "project" {
			if !v1.ProjectPhase(value).IsValid() {
				return waitCondition{}, fmt.Errorf("unknown project phase %q: valid are Pending, Scheduled, Running, Failed, Terminating, Terminated", value)
			}
			return waitCondition{what: "phase", value: value}, nil
		}
		for _, st := range nodeStates {
			if string(st) == value {
				return waitCondition{what: "phase", value: value}, nil
			}
		}
		return waitCondition{}, fmt.Errorf("unknown node state %q: valid are Ready, NotReady, Draining", value)
	}
	return waitCondition{}, fmt.Errorf("invalid --for %q: use phase=<Phase>, condition=<Type>, delete or heartbeat", s)
}

// check reads the resource once. It returns whether the condition holds and a
// one-line state to show. A non-nil error that is not transient ends the wait.
type check func(ctx context.Context) (done bool, state string, err error)

// waitFor waits until cond holds for the named resource.
func waitFor(ctx context.Context, client *Client, kind, name string, cond waitCondition, progress io.Writer, interval time.Duration) error {
	label := kind + "/" + name
	var c check
	if kind == "project" {
		c = projectCheck(client, name, cond)
	} else {
		c = nodeCheck(client, name, cond)
	}
	return poll(ctx, label, cond, c, progress, interval)
}

// waitForProject is apply --wait: wait until the Project is Running and runs
// specRevision, the revision the apply returned. Zero waits for Running only.
func waitForProject(ctx context.Context, client *Client, name string, specRevision int64, progress io.Writer, interval time.Duration) error {
	cond := waitCondition{what: "phase", value: string(v1.ProjectPhaseRunning), specRevision: specRevision}
	return waitFor(ctx, client, "project", name, cond, progress, interval)
}

func poll(ctx context.Context, label string, cond waitCondition, c check, progress io.Writer, interval time.Duration) error {
	var lastState string
	var lastErr error
	timedOut := func() error {
		msg := fmt.Sprintf("timed out waiting for %s %s", label, cond)
		if lastState != "" {
			msg += ": last state: " + lastState
		}
		if lastErr != nil {
			msg += "; last error: " + lastErr.Error()
		}
		return withExitCode(ExitTimeout, errors.New(msg))
	}
	for {
		done, state, err := c(ctx)
		switch {
		case err == nil:
			lastErr = nil
			if state != lastState {
				fmt.Fprintf(progress, "%s %s\n", label, state)
				lastState = state
			}
			if done {
				return nil
			}
		case ctx.Err() != nil:
			// The deadline can land during the read as easily as between reads.
			return timedOut()
		case isNotFound(err):
			return withExitCode(ExitNotFound, fmt.Errorf("%s not found", label))
		case isTransient(err):
			if lastErr == nil || lastErr.Error() != err.Error() {
				fmt.Fprintf(progress, "%s: %v (retrying)\n", label, err)
			}
			lastErr = err
		default:
			return err
		}
		select {
		case <-ctx.Done():
			return timedOut()
		case <-time.After(interval):
		}
	}
}

func isNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

// isTransient reports whether err may go away by itself: the server could not
// be reached, or answered 5xx or 429.
func isTransient(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode >= 500 || apiErr.StatusCode == http.StatusTooManyRequests
	}
	var urlErr *url.Error
	return errors.As(err, &urlErr)
}

// conditionTrue reports whether conditions has one of type t with status True.
func conditionTrue(conditions []v1.Condition, t string) bool {
	for _, c := range conditions {
		if string(c.Type) == t && c.Status == v1.ConditionTrue {
			return true
		}
	}
	return false
}

// trueConditions lists the True conditions other than Phase, with their
// messages, which is what tells a stuck wait apart from a slow one.
func trueConditions(conditions []v1.Condition) string {
	var s string
	for _, c := range conditions {
		if c.Type == v1.ConditionTypePhase || c.Status != v1.ConditionTrue {
			continue
		}
		s += "; " + string(c.Type)
		if c.Message != "" {
			s += ": " + c.Message
		}
	}
	return s
}

func projectCheck(client *Client, name string, cond waitCondition) check {
	return func(ctx context.Context) (bool, string, error) {
		p, err := client.GetProject(ctx, name)
		if cond.what == "delete" && isNotFound(err) {
			return true, "deleted", nil
		}
		if err != nil {
			return false, "", err
		}
		state := string(p.Status.Phase) + trueConditions(p.Status.Conditions)
		switch cond.what {
		case "phase":
			if string(p.Status.Phase) == cond.value {
				if p.Status.ObservedSpecRevision < cond.specRevision {
					// Running the previous revision. The SpecApplied reason,
					// when the Agent gave one, says why the new one is not.
					return false, state + "; " + specBehind(p, cond.specRevision), nil
				}
				return true, state, nil
			}
			if p.Status.Phase == v1.ProjectPhaseFailed {
				// Why it failed is in the Phase condition.
				why := "no reason given"
				for _, c := range p.Status.Conditions {
					if c.Type == v1.ConditionTypePhase {
						why = strings.TrimPrefix(c.Reason+": "+c.Message, ": ")
					}
				}
				return false, state, fmt.Errorf("project %q is Failed: %s", name, why)
			}
			return false, state, nil
		case "condition":
			return conditionTrue(p.Status.Conditions, cond.value), state, nil
		}
		return false, state, nil
	}
}

// specBehind describes a Project that does not run revision yet.
func specBehind(p v1.Project, revision int64) string {
	s := fmt.Sprintf("spec revision %d not running yet (running %d)", revision, p.Status.ObservedSpecRevision)
	for _, c := range p.Status.Conditions {
		if c.Type == v1.ConditionTypeSpecApplied && c.Status == v1.ConditionFalse &&
			c.Reason != "" && c.Reason != v1.ReasonSpecUpdatePending {
			s += ": " + strings.TrimSuffix(c.Reason+": "+c.Message, ": ")
		}
	}
	return s
}

func nodeCheck(client *Client, name string, cond waitCondition) check {
	var baseline *time.Time
	return func(ctx context.Context) (bool, string, error) {
		n, err := client.GetNode(ctx, name)
		if cond.what == "delete" && isNotFound(err) {
			return true, "deleted", nil
		}
		if err != nil {
			return false, "", err
		}
		state := string(n.Status.State) + trueConditions(n.Status.Conditions)
		switch cond.what {
		case "phase":
			return string(n.Status.State) == cond.value, state, nil
		case "condition":
			return conditionTrue(n.Status.Conditions, cond.value), state, nil
		case "heartbeat":
			hb := n.Status.LastHeartbeat
			state += "; lastHeartbeat " + formatHeartbeat(hb)
			if baseline == nil {
				// The first read is the reference: comparing with this
				// machine's clock would depend on the two clocks agreeing.
				baseline = &hb
				return false, state, nil
			}
			return hb.After(*baseline), state, nil
		}
		return false, state, nil
	}
}

func formatHeartbeat(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.UTC().Format(time.RFC3339)
}
