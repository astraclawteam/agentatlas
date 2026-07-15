package assessment

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	model "github.com/astraclawteam/agentatlas/sdk/go/assessment"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/storage"
)

// parityTenant is the enterprise every store-parity case runs under; the
// Postgres leg seeds it into enterprises (the tenant FK) once.
const parityTenant = "ent-parity-18b"

var parityKeySeq atomic.Uint64

// parityKey returns a unique policy_key per case so cases sharing the (append-only)
// Postgres scratch DB cannot collide on (tenant, policy_key, revision) — the
// timestamp keeps it unique across re-runs too, the atomic counter within a run.
func parityKey(name string) string {
	return fmt.Sprintf("assess.parity.%s.%d.%d", name, time.Now().UnixNano(), parityKeySeq.Add(1))
}

// minimalPolicy is the smallest valid draft policy: one built-in dimension with
// one evidence/attribution/confidence rule each and a valid source binding. It
// is shared by the store-parity suite and the service precondition tests.
func minimalPolicy(tenant, key string, revision int) model.Policy {
	dim := model.Dimension{
		Key:       model.DimOutcomeCompletion,
		Title:     "Outcome completion",
		Formal:    true,
		Rationale: "grounded in confirmed outcome completion over the assessment period",
		Evidence:  []model.EvidenceLink{{Handle: "ol_out1", Kind: "outcome", Summary: "satisfied outcome"}},
	}
	return model.Policy{
		Tenant: tenant, Org: org, PolicyKey: key, Revision: revision, Status: model.StatusDraft,
		Sources: model.SourceBinding{
			OrgVersion:        3,
			JobResponsibility: model.VersionedRef{Key: "resp.assembly.operator", Version: 4},
			OrgGoal:           model.VersionedRef{Key: goalKey, Version: goalVer},
			SOPs:              []model.VersionedRef{{Key: "sop.assembly.safety", Version: 7}},
			AcceptanceReview:  model.VersionedRef{Key: "review.assembly.qc", Version: 1},
		},
		Watermark:        42,
		Dimensions:       []model.Dimension{dim},
		EvidenceRules:    []model.EvidenceRule{{Dimension: dim.Key, Tier: model.TierVerifiedOutcome, Rationale: "grounded in a signed observation"}},
		AttributionRules: []model.AttributionRule{{Dimension: dim.Key, Mode: model.AttrSharedOnce, Rationale: "shared outcomes attributed once"}},
		ConfidenceRules:  []model.ConfidenceRule{{Dimension: dim.Key, Level: model.ConfidenceHigh, Rationale: "coverage is high across the period"}},
		CreatedAt:        fixedNow(), UpdatedAt: fixedNow(),
	}
}

// TestPolicyStoreParity runs one immutability/lifecycle table against BOTH store
// implementations so MemoryPolicyStore and PostgresPolicyStore cannot drift. The
// Postgres leg is gated on ATLAS_TEST_POSTGRES_DSN (skipped when unset, exactly
// like the outcome/outcomelearning postgres tests) and migrates the scratch DB
// first.
//
// This suite deliberately covers only the STRUCTURAL invariants the store/DB
// enforce (immutability of a frozen terminal state and of the rule set within a
// revision, plus create/lookup semantics). The legality of non-terminal lifecycle
// transitions is a SERVICE concern (see the service precondition tests and the
// checkTransition layering note) and is intentionally NOT duplicated here.
func TestPolicyStoreParity(t *testing.T) {
	t.Run("Memory", func(t *testing.T) {
		storeParityCases(t, func() PolicyStore { return NewMemoryPolicyStore(fixedNow) })
	})

	t.Run("Postgres", func(t *testing.T) {
		dsn := os.Getenv("ATLAS_TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("set ATLAS_TEST_POSTGRES_DSN to run the Postgres store-parity leg")
		}
		ctx := context.Background()
		if err := storage.Migrate(ctx, dsn); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		pool, err := storage.NewPool(ctx, dsn, nil)
		if err != nil {
			t.Fatalf("pool: %v", err)
		}
		t.Cleanup(pool.Close)
		if _, err := pool.Exec(ctx, `INSERT INTO enterprises(id,name) VALUES($1,'Assessment Parity') ON CONFLICT (id) DO NOTHING`, parityTenant); err != nil {
			t.Fatalf("seed enterprise: %v", err)
		}
		storeParityCases(t, func() PolicyStore {
			// pool is non-nil, so NewPostgresPolicyStore never errors here.
			s, _ := NewPostgresPolicyStore(pool, fixedNow)
			return s
		})
	})
}

func storeParityCases(t *testing.T, newStore func() PolicyStore) {
	ctx := context.Background()

	t.Run("RuleEditInPlaceRejected", func(t *testing.T) {
		store := newStore()
		key := parityKey("ruleedit")
		if _, err := store.CreateRevision(ctx, minimalPolicy(parityTenant, key, 1)); err != nil {
			t.Fatalf("create: %v", err)
		}
		got, err := store.GetRevision(ctx, parityTenant, key, 1)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		// Edit a rule in place (same tenant/key/revision) so the digest differs.
		got.EvidenceRules = append([]model.EvidenceRule(nil), got.EvidenceRules...)
		got.EvidenceRules[0].Tier = model.TierHumanReport
		if _, err := store.UpdateRevision(ctx, got); !errors.Is(err, ErrRulesImmutable) {
			t.Fatalf("rule edit in place = %v, want ErrRulesImmutable", err)
		}
	})

	t.Run("PublishedFrozenExceptRetire", func(t *testing.T) {
		store := newStore()
		key := parityKey("pubfrozen")
		seedPublished(t, store, key)
		got, err := store.GetRevision(ctx, parityTenant, key, 1)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		// A change other than -> retired (here: staying published while appending a
		// shadow cycle) is frozen.
		got.ShadowCycles = append(got.ShadowCycles, model.ShadowCycle{Cycle: 1, Revision: 1})
		got.UpdatedAt = fixedNow().Add(2 * time.Hour)
		if _, err := store.UpdateRevision(ctx, got); !errors.Is(err, ErrFrozenRevision) {
			t.Fatalf("published mutation = %v, want ErrFrozenRevision", err)
		}
	})

	t.Run("PublishedToRetiredStatusOnly", func(t *testing.T) {
		store := newStore()
		key := parityKey("pub2ret")
		seedPublished(t, store, key)
		got, err := store.GetRevision(ctx, parityTenant, key, 1)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		got.Status = model.StatusRetired
		got.UpdatedAt = fixedNow().Add(2 * time.Hour)
		retired, err := store.UpdateRevision(ctx, got)
		if err != nil {
			t.Fatalf("published -> retired (status only) must succeed, got %v", err)
		}
		if retired.Status != model.StatusRetired {
			t.Fatalf("status = %q, want retired", retired.Status)
		}
	})

	t.Run("RetiredFullyFrozen", func(t *testing.T) {
		store := newStore()
		key := parityKey("retfrozen")
		seedPublished(t, store, key)
		got, err := store.GetRevision(ctx, parityTenant, key, 1)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		got.Status = model.StatusRetired
		got.UpdatedAt = fixedNow().Add(2 * time.Hour)
		if _, err := store.UpdateRevision(ctx, got); err != nil {
			t.Fatalf("seed retire: %v", err)
		}
		// Any update to a retired revision is frozen.
		ret, err := store.GetRevision(ctx, parityTenant, key, 1)
		if err != nil {
			t.Fatalf("get retired: %v", err)
		}
		ret.GovernanceChangeID = "chg-parity-2"
		ret.UpdatedAt = fixedNow().Add(3 * time.Hour)
		if _, err := store.UpdateRevision(ctx, ret); !errors.Is(err, ErrFrozenRevision) {
			t.Fatalf("retired mutation = %v, want ErrFrozenRevision", err)
		}
	})

	t.Run("DuplicateCreateRejected", func(t *testing.T) {
		store := newStore()
		key := parityKey("dup")
		p := minimalPolicy(parityTenant, key, 1)
		if _, err := store.CreateRevision(ctx, p); err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := store.CreateRevision(ctx, p); !errors.Is(err, ErrRevisionExists) {
			t.Fatalf("duplicate create = %v, want ErrRevisionExists", err)
		}
	})

	t.Run("UpdateMissingRejected", func(t *testing.T) {
		store := newStore()
		key := parityKey("missing")
		p := minimalPolicy(parityTenant, key, 1)
		p.Status = model.StatusShadow
		if _, err := store.UpdateRevision(ctx, p); !errors.Is(err, ErrNotFound) {
			t.Fatalf("update missing = %v, want ErrNotFound", err)
		}
	})
}

// seedPublished creates revision 1 and advances it draft -> published through the
// store, re-reading via GetRevision so the value carries store-native timestamps
// (mirroring how the service loads a policy before transitioning it — the frozen
// published -> retired comparison in checkTransition then compares like for like).
func seedPublished(t *testing.T, store PolicyStore, key string) {
	t.Helper()
	ctx := context.Background()
	if _, err := store.CreateRevision(ctx, minimalPolicy(parityTenant, key, 1)); err != nil {
		t.Fatalf("seed create: %v", err)
	}
	got, err := store.GetRevision(ctx, parityTenant, key, 1)
	if err != nil {
		t.Fatalf("seed get: %v", err)
	}
	got.Status = model.StatusPublished
	got.GovernanceChangeID = "chg-parity"
	got.UpdatedAt = fixedNow().Add(time.Hour)
	if _, err := store.UpdateRevision(ctx, got); err != nil {
		t.Fatalf("seed publish: %v", err)
	}
}
