package dream

import (
	"fmt"
	"sort"
	"time"

	sdkdream "github.com/astraclawteam/agentatlas/sdk/go/dream"
)

// HierarchyCandidate is a published policy placed in one versioned org-tree
// snapshot. Depth is measured from the enterprise root.
type HierarchyCandidate struct {
	PolicyID  string
	OrgUnitID string
	Depth     int
	policy    Policy
	version   int32
	children  []string
}

// ChildRunState is the latest immutable attempt for an immediate child and an
// exact parent window.
type ChildRunState struct {
	OrgUnitID string
	Status    string
	Attempt   int32
}

func sortHierarchyCandidates(candidates []HierarchyCandidate) {
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Depth != candidates[j].Depth {
			return candidates[i].Depth > candidates[j].Depth
		}
		if candidates[i].OrgUnitID != candidates[j].OrgUnitID {
			return candidates[i].OrgUnitID < candidates[j].OrgUnitID
		}
		return candidates[i].PolicyID < candidates[j].PolicyID
	})
}

// childReadiness requires one explicit outcome per expected immediate child.
// A failed child is usable only after its retry budget is exhausted and the
// parent policy explicitly permits partial input.
func childReadiness(expected []string, states []ChildRunState, explicit []MissingInput, allowPartial bool, maxAttempts map[string]int32) (bool, Coverage, []MissingInput, error) {
	expectedSet := make(map[string]struct{}, len(expected))
	for _, child := range expected {
		if child == "" {
			return false, Coverage{}, nil, fmt.Errorf("Dream hierarchy contains an empty child identity")
		}
		if _, duplicate := expectedSet[child]; duplicate {
			return false, Coverage{}, nil, fmt.Errorf("Dream hierarchy contains duplicate child %s", child)
		}
		expectedSet[child] = struct{}{}
	}
	latest := make(map[string]ChildRunState, len(states))
	for _, state := range states {
		if _, ok := expectedSet[state.OrgUnitID]; !ok {
			continue
		}
		if current, ok := latest[state.OrgUnitID]; !ok || state.Attempt > current.Attempt {
			latest[state.OrgUnitID] = state
		}
	}
	missingByChild := make(map[string]MissingInput, len(explicit))
	for _, item := range explicit {
		if item.SourceType != sdkdream.SourceChildDreamSummary {
			return false, Coverage{}, nil, fmt.Errorf("explicit hierarchy missing input has source %s", item.SourceType)
		}
		if _, ok := expectedSet[item.SourceID]; !ok {
			return false, Coverage{}, nil, fmt.Errorf("explicit hierarchy missing input names unexpected child %s", item.SourceID)
		}
		missingByChild[item.SourceID] = item
	}

	coverage := Coverage{ExpectedChildren: len(expected)}
	missing := append([]MissingInput(nil), explicit...)
	for _, child := range expected {
		if _, ok := missingByChild[child]; ok {
			continue
		}
		state, ok := latest[child]
		if !ok {
			return false, coverage, normalizeMissing(missing), nil
		}
		switch state.Status {
		case "succeeded":
			coverage.CompletedChildren++
		case "failed":
			max := maxAttempts[child]
			if max < 1 || state.Attempt < max || !allowPartial {
				return false, coverage, normalizeMissing(missing), nil
			}
			item := MissingInput{SourceType: sdkdream.SourceChildDreamSummary, SourceID: child, Reason: sdkdream.MissingFailed}
			missing = append(missing, item)
		default:
			return false, coverage, normalizeMissing(missing), nil
		}
	}
	return true, coverage, normalizeMissing(missing), nil
}

func retryAttempt(status string, attempt, maxAttempts int32) (int32, bool) {
	if status != "failed" || attempt < 1 || maxAttempts < 1 || attempt >= maxAttempts {
		return 0, false
	}
	return attempt + 1, true
}

func validateExplicitWindow(start, end, successfulStart, successfulEnd time.Time, rerun bool) error {
	if start.IsZero() || end.IsZero() || !end.After(start) {
		return fmt.Errorf("Dream backfill requires explicit ordered window bounds")
	}
	if successfulStart.IsZero() || successfulEnd.IsZero() {
		return nil
	}
	if start.Before(successfulEnd) && successfulStart.Before(end) && !rerun {
		return fmt.Errorf("Dream backfill overlaps a successful window; create an explicit rerun")
	}
	return nil
}
