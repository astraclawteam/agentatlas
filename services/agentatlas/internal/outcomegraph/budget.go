package outcomegraph

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Budget bounds every Outcome-Graph query: a maximum traversal depth, a maximum
// number of returned rows, and a wall-clock time budget. No query ever runs
// unbounded — an unset field falls back to the conservative GA defaults, and a
// request that asks for more than the hard ceilings is refused rather than
// clamped silently, so a caller cannot smuggle an unbounded traversal past the
// budget.
type Budget struct {
	MaxDepth int
	MaxRows  int
	Timeout  time.Duration
}

// GA hard ceilings and defaults. A typed operation may lower these but may never
// exceed the ceilings (Resolve refuses an over-budget request).
const (
	DefaultMaxDepth = 5
	DefaultMaxRows  = 500
	DefaultTimeout  = 5 * time.Second

	CeilingMaxDepth = 12
	CeilingMaxRows  = 5000
	CeilingTimeout  = 30 * time.Second
)

// Sentinel budget errors.
var (
	// ErrBudgetExceeded is returned when a requested depth/rows/time exceeds the
	// hard ceiling (a refusal, never an unbounded run).
	ErrBudgetExceeded = errors.New("outcomegraph: query budget exceeded")
	// ErrQueryDeadline is returned when a query is cancelled by its time budget.
	ErrQueryDeadline = errors.New("outcomegraph: query cancelled by time budget")
)

// DefaultBudget returns the conservative GA default budget.
func DefaultBudget() Budget {
	return Budget{MaxDepth: DefaultMaxDepth, MaxRows: DefaultMaxRows, Timeout: DefaultTimeout}
}

// Resolve validates a requested budget against the hard ceilings and fills
// unset fields with defaults. A request that EXCEEDS a ceiling is refused with
// ErrBudgetExceeded — excessive depth/rows/time is never run.
func (b Budget) Resolve() (Budget, error) {
	out := b
	if out.MaxDepth == 0 {
		out.MaxDepth = DefaultMaxDepth
	}
	if out.MaxRows == 0 {
		out.MaxRows = DefaultMaxRows
	}
	if out.Timeout == 0 {
		out.Timeout = DefaultTimeout
	}
	if out.MaxDepth < 0 || out.MaxRows < 0 || out.Timeout < 0 {
		return Budget{}, fmt.Errorf("%w: negative bound", ErrBudgetExceeded)
	}
	if out.MaxDepth > CeilingMaxDepth {
		return Budget{}, fmt.Errorf("%w: depth %d > ceiling %d", ErrBudgetExceeded, out.MaxDepth, CeilingMaxDepth)
	}
	if out.MaxRows > CeilingMaxRows {
		return Budget{}, fmt.Errorf("%w: rows %d > ceiling %d", ErrBudgetExceeded, out.MaxRows, CeilingMaxRows)
	}
	if out.Timeout > CeilingTimeout {
		return Budget{}, fmt.Errorf("%w: timeout %s > ceiling %s", ErrBudgetExceeded, out.Timeout, CeilingTimeout)
	}
	return out, nil
}

// withDeadline derives a context bounded by the budget's timeout.
func (b Budget) withDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, b.Timeout)
}
