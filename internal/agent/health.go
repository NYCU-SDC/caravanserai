package agent

import (
	"fmt"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/agent/docker"
)

// serviceHealth is one service's container reduced to the decision recovery has
// to make about it. The categories are named for that decision rather than for
// the Docker status, because several statuses call for the same action and one
// status ("dead") calls for a different action than its name suggests.
type serviceHealth int

const (
	// healthRunning is the only state that needs nothing done.
	healthRunning serviceHealth = iota

	// healthStartable covers "exited" and "created": the container exists and
	// its filesystem is intact, so starting it is the narrowest operation that
	// fixes it. Both exit codes land here — see classifyService.
	healthStartable

	// healthNeedsRecreate covers a missing container and Docker's "dead"
	// status. A dead container is one Docker failed to remove and cannot
	// restart, so it has to be replaced rather than started.
	healthNeedsRecreate

	// healthPaused is resumable, but nothing in cara ever pauses a container,
	// so reaching it means something outside cara acted on a container cara
	// owns. It is a fault, not a healthy state.
	healthPaused

	// healthTransient covers "restarting": Docker is mid-action and the next
	// poll will see a settled state. cara sets no RestartPolicy, so this
	// should not occur for a cara-managed container, which is exactly why it
	// must not be silently treated as healthy.
	healthTransient

	// healthUnknown is a status string this code does not recognise. Treating
	// an unrecognised state as healthy is how a container stays broken without
	// anyone noticing, so it is reported instead.
	healthUnknown

	// healthNotOwned is a container found under the service's name that does
	// not belong to the current assignment — another lifetime's UID, an
	// earlier grant's generation, or another Project's labels. It outranks
	// every Docker status: a stale container that is running says nothing
	// about whether this assignment is healthy, and one that has exited is not
	// this assignment's to start.
	healthNotOwned
)

func (h serviceHealth) String() string {
	switch h {
	case healthRunning:
		return "running"
	case healthStartable:
		return "startable"
	case healthNeedsRecreate:
		return "needs-recreate"
	case healthPaused:
		return "paused"
	case healthTransient:
		return "transient"
	case healthNotOwned:
		return "not-owned"
	default:
		return "unknown"
	}
}

// serviceState pairs a service with its classification and the raw Docker
// facts behind it. The raw values are kept because the caller distinguishes a
// crash from a clean exit for reporting, and both are healthStartable.
type serviceState struct {
	Service  string
	Health   serviceHealth
	Status   string // raw Docker status; empty when no container exists
	ExitCode int

	// NotOwned is the ownership mismatch behind healthNotOwned, and nil
	// otherwise.
	NotOwned error
}

// Crashed reports whether this container exited with a non-zero status.
func (s serviceState) Crashed() bool {
	return s.Status == "exited" && s.ExitCode != 0
}

// Detail renders the service and the reason it is not running, for a status
// message a human will read.
func (s serviceState) Detail() string {
	switch {
	case s.Health == healthNotOwned:
		return fmt.Sprintf("%s(not owned)", s.Service)
	case s.Status == "":
		return s.Service
	case s.Status == "exited":
		return fmt.Sprintf("%s(exit=%d)", s.Service, s.ExitCode)
	default:
		return fmt.Sprintf("%s(%s)", s.Service, s.Status)
	}
}

// classifyProject classifies every service declared in the spec, including
// those with no container at all.
//
// It iterates the spec rather than the inspected states so a missing container
// is a classification rather than an absence the caller has to infer by
// comparing lengths — the shape the previous code used, which could not tell
// which service was missing without a second pass.
func classifyProject(p *v1.Project, states []docker.ContainerState) []serviceState {
	byService := make(map[string]docker.ContainerState, len(states))
	for _, s := range states {
		byService[s.ServiceName] = s
	}

	out := make([]serviceState, 0, len(p.Spec.Services))
	for _, svc := range p.Spec.Services {
		state, found := byService[svc.Name]
		if !found {
			out = append(out, serviceState{Service: svc.Name, Health: healthNeedsRecreate})
			continue
		}
		out = append(out, serviceState{
			Service:  svc.Name,
			Health:   classifyService(state),
			Status:   state.Status,
			ExitCode: state.ExitCode,
			NotOwned: state.NotOwned,
		})
	}
	return out
}

// classifyService maps one Docker status to the action it calls for.
//
// A clean exit is treated exactly like a crash. cara has no notion of a
// service that legitimately finishes — every ServiceDef is expected to keep
// running — so a container that exited 0 is as broken as one that exited 1.
// `docker stop` produces exactly that case, which makes it the most common
// way an operator will see this path.
func classifyService(s docker.ContainerState) serviceHealth {
	if s.NotOwned != nil {
		return healthNotOwned
	}
	switch s.Status {
	case "running":
		return healthRunning
	case "exited", "created":
		return healthStartable
	case "dead":
		return healthNeedsRecreate
	case "paused":
		return healthPaused
	case "restarting":
		return healthTransient
	default:
		return healthUnknown
	}
}

// unhealthy returns the services that are not running, preserving spec order.
func unhealthy(states []serviceState) []serviceState {
	var out []serviceState
	for _, s := range states {
		if s.Health != healthRunning {
			out = append(out, s)
		}
	}
	return out
}
