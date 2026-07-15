package assessment

import (
	"context"
	"errors"
	"os"
	"testing"

	model "github.com/astraclawteam/agentatlas/sdk/go/assessment"
	"github.com/astraclawteam/agentatlas/services/agentatlas/internal/storage"
)

const correctionTenant = "ent-corr-18d"

// TestCorrectionStoreParity runs one append-only + one-shot-resolution
// immutability table against BOTH CorrectionStore implementations so
// MemoryCorrectionStore and PostgresCorrectionStore cannot drift. The Postgres
// leg is gated on ATLAS_TEST_POSTGRES_DSN (skipped when unset, like the
// outcome/policy/result postgres tests) and migrates the scratch DB (0..22,
// including 000022_assessment_corrections.sql) first.
func TestCorrectionStoreParity(t *testing.T) {
	t.Run("Memory", func(t *testing.T) {
		results := NewMemoryResultStore(fixedNow)
		correctionStoreCases(t, func() CorrectionStore { return NewMemoryCorrectionStore(fixedNow) }, results, tnt)
	})

	t.Run("Postgres", func(t *testing.T) {
		dsn := os.Getenv("ATLAS_TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("set ATLAS_TEST_POSTGRES_DSN to run the Postgres correction-store leg")
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
		if _, err := pool.Exec(ctx, `INSERT INTO enterprises(id,name) VALUES($1,'Assessment Correction Parity') ON CONFLICT (id) DO NOTHING`, correctionTenant); err != nil {
			t.Fatalf("seed enterprise: %v", err)
		}
		results, _ := NewPostgresResultStore(pool, fixedNow)
		correctionStoreCases(t, func() CorrectionStore {
			s, _ := NewPostgresCorrectionStore(pool, fixedNow)
			return s
		}, results, correctionTenant)

		// Postgres-only: the migration's triggers are the database backstop. A raw
		// DELETE and a raw core-mutating UPDATE must both be rejected.
		t.Run("TriggersRejectDeleteAndCoreMutation", func(t *testing.T) {
			store, _ := NewPostgresCorrectionStore(pool, fixedNow)
			v1 := mustSeedAssessment(t, results, correctionTenant, resultSubject("trigger"))
			c := pendingCorrection(v1)
			saved, err := store.Append(ctx, c)
			if err != nil {
				t.Fatalf("append: %v", err)
			}
			if _, err := pool.Exec(ctx, `DELETE FROM assessment_corrections WHERE tenant=$1 AND id=$2`, saved.Tenant, saved.ID); err == nil {
				t.Fatal("a raw DELETE of a correction must be rejected by the immutability trigger")
			}
			if _, err := pool.Exec(ctx, `UPDATE assessment_corrections SET kind='add_evidence' WHERE tenant=$1 AND id=$2`, saved.Tenant, saved.ID); err == nil {
				t.Fatal("a raw core-mutating UPDATE must be rejected by the immutability trigger")
			}
		})
	})
}

func mustSeedAssessment(t *testing.T, results ResultStore, tenant, subj string) model.WorkAssessment {
	t.Helper()
	v1, err := results.AppendAssessment(context.Background(), sampleAssessment(tenant, subj, 1))
	if err != nil {
		t.Fatalf("seed target assessment: %v", err)
	}
	return v1
}

func pendingCorrection(target model.WorkAssessment) AssessmentCorrectionCase {
	return AssessmentCorrectionCase{
		Tenant: target.Tenant, Subject: target.Subject,
		TargetAssessmentID: target.ID, TargetDigest: target.Digest(),
		Kind: CorrectionChallengeFact, Dimension: model.DimOutcomeCompletion,
		ChallengedHandle: "outcome-1", Rationale: "the outcome-1 fact is disputed",
		SubmittedAt: fixedNow(), Resolution: CorrectionResolution{State: CorrectionPending},
	}
}

func correctionStoreCases(t *testing.T, newStore func() CorrectionStore, results ResultStore, tenant string) {
	ctx := context.Background()

	t.Run("AppendGetRoundtrip", func(t *testing.T) {
		store := newStore()
		v1 := mustSeedAssessment(t, results, tenant, resultSubject("roundtrip"))
		saved, err := store.Append(ctx, pendingCorrection(v1))
		if err != nil {
			t.Fatalf("append: %v", err)
		}
		if saved.ID == "" || saved.ID != saved.DerivedID() {
			t.Fatalf("append must assign the deterministic id, got %q", saved.ID)
		}
		got, err := store.Get(ctx, saved.Tenant, saved.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.TargetAssessmentID != v1.ID || got.Resolution.State != CorrectionPending {
			t.Fatalf("roundtrip mismatch: got %+v", got)
		}
	})

	t.Run("DuplicateAppendRejected", func(t *testing.T) {
		store := newStore()
		v1 := mustSeedAssessment(t, results, tenant, resultSubject("dup"))
		c := pendingCorrection(v1)
		if _, err := store.Append(ctx, c); err != nil {
			t.Fatalf("append: %v", err)
		}
		if _, err := store.Append(ctx, c); !errors.Is(err, ErrCorrectionExists) {
			t.Fatalf("duplicate append = %v, want ErrCorrectionExists", err)
		}
	})

	t.Run("ResolveIsOneShotAndImmutable", func(t *testing.T) {
		store := newStore()
		v1 := mustSeedAssessment(t, results, tenant, resultSubject("resolve"))
		v2 := mustSeedAssessment2(t, results, tenant, v1.Subject)
		saved, err := store.Append(ctx, pendingCorrection(v1))
		if err != nil {
			t.Fatalf("append: %v", err)
		}
		accepted := saved
		accepted.Resolution = CorrectionResolution{State: CorrectionAccepted, DecidedBy: "u:div-lead", DecidedAt: fixedNow(), NewAssessmentID: v2.ID, NewVersion: v2.Version}
		got, err := store.Resolve(ctx, accepted)
		if err != nil {
			t.Fatalf("resolve accept: %v", err)
		}
		if got.Resolution.State != CorrectionAccepted || got.Resolution.NewAssessmentID != v2.ID {
			t.Fatalf("resolve did not record the accepted resolution: %+v", got.Resolution)
		}
		// One-shot: a second resolve is rejected.
		rej := got
		rej.Resolution = CorrectionResolution{State: CorrectionRejected, DecidedBy: "u:div-lead", DecidedAt: fixedNow()}
		if _, err := store.Resolve(ctx, rej); !errors.Is(err, ErrCorrectionResolved) {
			t.Fatalf("re-resolve = %v, want ErrCorrectionResolved", err)
		}
	})

	t.Run("MissingGetIsNotFound", func(t *testing.T) {
		store := newStore()
		if _, err := store.Get(ctx, tenant, "cor_does_not_exist"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing get = %v, want ErrNotFound", err)
		}
	})
}

func mustSeedAssessment2(t *testing.T, results ResultStore, tenant, subj string) model.WorkAssessment {
	t.Helper()
	v2, err := results.AppendAssessment(context.Background(), sampleAssessment(tenant, subj, 2))
	if err != nil {
		t.Fatalf("seed v2: %v", err)
	}
	return v2
}
