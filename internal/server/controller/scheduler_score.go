package controller

import (
	v1 "NYCU-SDC/caravanserai/api/v1"
)

// TierPrimaryBonus is added to the score of a primary-tier Node. A score from
// leastAllocated is at most 100, so any bonus above that makes the preference
// strict: a primary Node that passed Filter always beats a backup one, however
// much emptier the backup Node is. That is the policy of
// docs/scheduler-strategy.md §3.1 — run on primary, fall back to backup only
// when no primary Node can take the Project — and fail-back relies on it to
// land a Project back on primary.
//
// Lowering it below 100 turns the preference into a soft one, where a nearly
// full primary Node can lose to an empty backup Node. It is a constant so it
// is easy to tune.
const TierPrimaryBonus = 1000.0

// scoreCandidate is the Score stage of the scheduler (docs/scheduler-strategy.md
// §5.1): the higher the score, the better the Node for a Project of the given
// size. It is leastAllocated, plus TierPrimaryBonus for a primary Node.
func scoreCandidate(c candidate, size v1.ProjectSize) float64 {
	score := leastAllocated(c, size)
	if v1.NodeTierFromLabels(c.Labels) == v1.NodeTierPrimary {
		score += TierPrimaryBonus
	}
	return score
}

// leastAllocated scores a Node by how much of it stays free once the Project
// is placed, 0 to 100, so that among Nodes of one tier the emptiest wins and
// Projects spread out. CPU and memory count equally, as in Kubernetes'
// LeastAllocated.
func leastAllocated(c candidate, size v1.ProjectSize) float64 {
	req := size.Requests()
	cpu := freePercent(c.Allocatable.CPUMilli, c.Used.CPUMilli+req.CPUMilli)
	mem := freePercent(c.Allocatable.MemoryBytes, c.Used.MemoryBytes+req.MemoryBytes)
	return (cpu + mem) / 2
}

// freePercent is the share of allocatable left once booked is taken, 0 to 100.
func freePercent(allocatable, booked int64) float64 {
	if allocatable <= 0 || booked >= allocatable {
		return 0
	}
	return 100 * float64(allocatable-booked) / float64(allocatable)
}

// pickBest returns the highest-scoring candidate and its score. Equal scores
// go to the Node whose name sorts first, so the choice is the same on every
// run instead of following the order the store happened to list Nodes in.
// candidates must not be empty.
func pickBest(candidates []candidate, size v1.ProjectSize) (best candidate, bestScore float64) {
	for i, c := range candidates {
		score := scoreCandidate(c, size)
		if i == 0 || score > bestScore || (score == bestScore && c.Name < best.Name) {
			best, bestScore = c, score
		}
	}
	return best, bestScore
}
