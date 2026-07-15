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

const resultTenant = "ent-parity-18c"

var resultSubjSeq atomic.Uint64

// resultSubject returns a unique subject per case so cases sharing the
// (append-only) Postgres scratch DB cannot collide on the assessment identity.
func resultSubject(name string) string {
	return fmt.Sprintf("actor:%s.%d.%d", name, time.Now().UnixNano(), resultSubjSeq.Add(1))
}

func fullConfidence(level model.ConfidenceLevel) model.ConfidenceExplanation {
	comps := make([]model.ConfidenceComponent, 0, len(model.ConfidenceKinds()))
	for _, k := range model.ConfidenceKinds() {
		comps = append(comps, model.ConfidenceComponent{Kind: k, Score: 1})
	}
	return model.ConfidenceExplanation{Level: level, Components: comps}
}

// sampleAssessment builds a minimal valid WorkAssessment for the store suite.
func sampleAssessment(tenant, subj string, version int) model.WorkAssessment {
	return model.WorkAssessment{
		Tenant: tenant, Org: org, Subject: subj, Level: model.LevelIndividual,
		PolicyKey: policyKey, PolicyRevision: 1, Version: version, Formal: true,
		OrgVersion: 3, Sources: sources(),
		Period: model.Period{Start: fixedNow().Add(-720 * time.Hour), End: fixedNow().Add(-time.Hour)},
		Graph:  model.GraphSchema{Provider: "apache-age", SchemaVersion: "1", ProtocolVersion: "1", GraphName: "outcomegraph", Watermark: 42},
		Dimensions: []model.DimensionResult{{
			Dimension: model.DimOutcomeCompletion, State: model.StateAssessed, Attribution: model.AttrSharedOnce,
			CountedOutcomeKeys: []string{"outcome-1"}, SatisfiedOutcomes: 1,
			Evidence:   []model.Evidence{{Tier: model.TierVerifiedOutcome, Handle: "outcome-1", Kind: "outcome", Revision: 1, Authority: "mes.official", SignatureKeyID: "sig-key-1", Verified: true}},
			Confidence: fullConfidence(model.ConfidenceHigh),
		}},
		Manager:   model.ManagerConfirmation{Confirmed: true, Manager: "mgr:line-lead", ConfirmedAt: fixedNow()},
		CreatedAt: fixedNow(),
	}
}

// TestResultStoreParity runs one append-only immutability table against BOTH store
// implementations so MemoryResultStore and PostgresResultStore cannot drift. The
// Postgres leg is gated on ATLAS_TEST_POSTGRES_DSN (skipped when unset, exactly
// like the outcome/policy postgres tests) and migrates the scratch DB (0..21) first.
func TestResultStoreParity(t *testing.T) {
	t.Run("Memory", func(t *testing.T) {
		resultStoreCases(t, func() ResultStore { return NewMemoryResultStore(fixedNow) }, tnt)
	})

	t.Run("Postgres", func(t *testing.T) {
		dsn := os.Getenv("ATLAS_TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("set ATLAS_TEST_POSTGRES_DSN to run the Postgres result-store leg")
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
		if _, err := pool.Exec(ctx, `INSERT INTO enterprises(id,name) VALUES($1,'Assessment Result Parity') ON CONFLICT (id) DO NOTHING`, resultTenant); err != nil {
			t.Fatalf("seed enterprise: %v", err)
		}
		resultStoreCases(t, func() ResultStore {
			s, _ := NewPostgresResultStore(pool, fixedNow)
			return s
		}, resultTenant)
	})
}

func resultStoreCases(t *testing.T, newStore func() ResultStore, tenant string) {
	ctx := context.Background()

	t.Run("AppendGetRoundtrip", func(t *testing.T) {
		store := newStore()
		wa := sampleAssessment(tenant, resultSubject("roundtrip"), 1)
		saved, err := store.AppendAssessment(ctx, wa)
		if err != nil {
			t.Fatalf("append: %v", err)
		}
		if saved.ID == "" || saved.ID != saved.DerivedID() {
			t.Fatalf("append must assign the deterministic id, got %q", saved.ID)
		}
		got, err := store.GetAssessment(ctx, saved.Tenant, saved.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.Subject != wa.Subject || got.Digest() != saved.Digest() {
			t.Fatalf("roundtrip mismatch: got %+v", got)
		}
	})

	t.Run("DuplicateAppendRejected", func(t *testing.T) {
		store := newStore()
		wa := sampleAssessment(tenant, resultSubject("dup"), 1)
		if _, err := store.AppendAssessment(ctx, wa); err != nil {
			t.Fatalf("append: %v", err)
		}
		if _, err := store.AppendAssessment(ctx, wa); !errors.Is(err, ErrRevisionExists) {
			t.Fatalf("duplicate append = %v, want ErrRevisionExists (append-only, immutable)", err)
		}
	})

	t.Run("ReassessmentIsANewVersion", func(t *testing.T) {
		store := newStore()
		subj := resultSubject("reassess")
		v1, err := store.AppendAssessment(ctx, sampleAssessment(tenant, subj, 1))
		if err != nil {
			t.Fatalf("append v1: %v", err)
		}
		v2, err := store.AppendAssessment(ctx, sampleAssessment(tenant, subj, 2))
		if err != nil {
			t.Fatalf("append v2 (re-assessment): %v", err)
		}
		if v1.ID == v2.ID {
			t.Fatal("a re-assessment must be a NEW version with a new id")
		}
		latest, err := store.LatestVersion(ctx, tenant, subj, policyKey, 1)
		if err != nil {
			t.Fatalf("latest: %v", err)
		}
		if latest.Version != 2 {
			t.Fatalf("latest version = %d, want 2", latest.Version)
		}
	})

	t.Run("MissingGetIsNotFound", func(t *testing.T) {
		store := newStore()
		if _, err := store.GetAssessment(ctx, tenant, "wa_does_not_exist"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing get = %v, want ErrNotFound", err)
		}
	})
}
