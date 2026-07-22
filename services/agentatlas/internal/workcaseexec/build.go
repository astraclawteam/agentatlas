package workcaseexec

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	governance "github.com/astraclawteam/agentatlas/services/agentatlas/internal/governance"
	workcase "github.com/astraclawteam/agentatlas/services/agentatlas/internal/workcase"
)

// BuildInput is what a composition root supplies to build the WorkCase
// orchestrator. Build owns the seams this repository can construct on its own
// (the WorkCase Service over PostgreSQL, and the run ledger); everything a
// binary must inject is a field here, so an unwired seam is a nil VALUE that
// MissingRequired reports rather than an omission nothing looks for.
type BuildInput struct {
	// Pool is the authoritative WorkCase PostgreSQL. Required.
	Pool *pgxpool.Pool

	// TrustedKeyID / TrustedKeyFile locate AgentNexus's receipt-signing public
	// key (config.AgentNexus.ActionSigningKeyID / ActionSigningKeyFile). Build
	// loads the file; an absent or malformed key leaves TrustedKey unset, which
	// MissingRequired then reports by name.
	TrustedKeyID   string
	TrustedKeyFile string

	// CurrentOrgVersion is the authoritative sealed org version the
	// UpwardReviewPolicy evaluates every party against. Zero fails every upward
	// review, so it is required rather than defaulted.
	CurrentOrgVersion int64

	// Gateway and Governor have NO production implementation today; see gap.go.
	// They are parameters rather than something Build constructs so that the
	// refusal below is derived from what was actually supplied, not hard-coded:
	// the day either one exists, passing it here composes a working
	// Orchestrator with no change to this function.
	Gateway  workcase.ActionGateway
	Governor workcase.Governor

	// Runs overrides the run ledger. Nil selects workcase.MemoryRunStore, and
	// Report.DurableRunLedger then reports false -- see Report.
	Runs workcase.RunStore

	Planner  workcase.Planner
	Audit    workcase.AuditSink
	Observer workcase.ObservationGateway
	Outcome  *workcase.OutcomeConfig

	Now         func() time.Time
	MaxAttempts int
}

// Report describes the orchestrator Build produced (or refused to produce). It
// is returned on BOTH paths, because the limitations a caller must publish do
// not disappear when composition fails.
type Report struct {
	// DurableRunLedger is false when the run ledger is in-memory. The
	// Orchestrator's own contract says "a fresh Orchestrator resumes exactly
	// where a crashed one left off"; with a volatile ledger that is FALSE, and a
	// crash mid-dispatch loses the record of a side effect that may already have
	// executed. Callers must surface Limitations rather than let that ship
	// silently.
	DurableRunLedger bool
	// Limitations are operator-facing statements about what this orchestrator
	// does NOT guarantee. Empty means no known limitation, not "verified safe".
	Limitations []string
}

// ErrNoPool reports a Build with no authoritative WorkCase database.
var ErrNoPool = errors.New("workcaseexec: a PostgreSQL pool is required (the authoritative WorkCase database)")

// Build composes the WorkCase orchestrator, or refuses with a *NotComposedError
// naming every seam that has no source.
//
// It deliberately performs NO irreversible work before it knows it can finish:
// it resolves and validates every seam first, and only constructs after
// MissingRequired is empty. A caller that gets an error has an orchestrator that
// does not exist, which is an honest state; it never gets one that exists and
// escalates everything to a human.
func Build(in BuildInput) (*workcase.Orchestrator, Report, error) {
	report := Report{}
	if in.Pool == nil {
		return nil, report, ErrNoPool
	}

	store, err := workcase.NewPostgresStore(in.Pool, in.Now)
	if err != nil {
		return nil, report, fmt.Errorf("workcaseexec: workcase store: %w", err)
	}
	svc, err := workcase.NewService(store, nil)
	if err != nil {
		return nil, report, fmt.Errorf("workcaseexec: workcase service: %w", err)
	}

	runs := in.Runs
	report.DurableRunLedger = runs != nil
	if runs == nil {
		runs = workcase.NewMemoryRunStore()
		report.Limitations = append(report.Limitations,
			"the WorkCase run ledger is IN-MEMORY: a restart loses every in-flight action's dispatch record, so a side effect dispatched before the restart is neither resumed nor reconciled")
	}

	// A key that fails to load is reported as a limitation AND leaves TrustedKey
	// unset, so the refusal below names it. Swallowing the load error would turn
	// a misconfigured key path into an unexplained missing seam.
	trustedKey, keyErr := loadEd25519PublicKey(in.TrustedKeyFile)
	if keyErr != nil && strings.TrimSpace(in.TrustedKeyFile) != "" {
		report.Limitations = append(report.Limitations,
			"AgentNexus receipt-signing key could not be loaded: "+keyErr.Error())
	}

	deps := Deps{
		Service:        svc,
		Runs:           runs,
		Gateway:        in.Gateway,
		Governor:       in.Governor,
		Planner:        in.Planner,
		Audit:          in.Audit,
		Observer:       in.Observer,
		TrustedKeyID:   in.TrustedKeyID,
		TrustedKey:     trustedKey,
		ApprovalPolicy: governance.UpwardReviewPolicy{CurrentOrgVersion: in.CurrentOrgVersion},
		Outcome:        in.Outcome,
		Now:            in.Now,
		MaxAttempts:    in.MaxAttempts,
	}
	orch, err := New(deps)
	if err != nil {
		return nil, report, err
	}
	return orch, report, nil
}

// loadEd25519PublicKey reads an ed25519 PUBLIC key from path. It accepts
// standard base64 or hex, trimmed -- the two forms the deployment material in
// this repository already uses for key bytes.
//
// It is a PUBLIC key on purpose: AgentAtlas verifies AgentNexus's receipt
// signatures and never produces one. If a private key ever turns up on this
// path, the length check below rejects it rather than quietly accepting the
// first 32 bytes of a seed.
func loadEd25519PublicKey(path string) (ed25519.PublicKey, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return nil, errors.New("no key file configured")
	}
	if !filepath.IsAbs(trimmed) || filepath.Clean(trimmed) != trimmed {
		return nil, errors.New("key path must be canonical and absolute")
	}
	raw, err := os.ReadFile(trimmed)
	if err != nil {
		// Deliberately not %w: the wrapped *PathError repeats the path, and this
		// message is logged by a composition root.
		return nil, errors.New("read key file: " + err.Error())
	}
	encoded := strings.TrimSpace(string(raw))
	for _, decode := range []func(string) ([]byte, error){
		base64.StdEncoding.DecodeString,
		hex.DecodeString,
	} {
		if decoded, derr := decode(encoded); derr == nil && len(decoded) == ed25519.PublicKeySize {
			return ed25519.PublicKey(decoded), nil
		}
	}
	return nil, fmt.Errorf("key file does not contain a %d-byte ed25519 public key in base64 or hex", ed25519.PublicKeySize)
}
