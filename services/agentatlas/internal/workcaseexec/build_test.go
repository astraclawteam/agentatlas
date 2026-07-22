package workcaseexec

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	workcase "github.com/astraclawteam/agentatlas/services/agentatlas/internal/workcase"
)

// unreachablePool is a real *pgxpool.Pool that never connects. Build performs no
// query (it only hands the pool to the store), so this exercises the genuine
// production type rather than a nil pointer that would slip past the store's own
// constructor check for a different reason than the one under test.
func unreachablePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig("postgres://atlas:atlas@127.0.0.1:1/agentatlas?sslmode=disable")
	if err != nil {
		t.Fatalf("parse pool config: %v", err)
	}
	cfg.MinConns = 0
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestBuildRefusesWithoutAnAuthoritativeDatabase(t *testing.T) {
	_, _, err := Build(BuildInput{})
	if !errors.Is(err, ErrNoPool) {
		t.Fatalf("want ErrNoPool, got %v", err)
	}
}

// This is the assertion that describes the repository's ACTUAL state: with
// everything a composition root can supply today, the orchestrator still cannot
// be built, and the refusal names all three reasons.
//
// It is not a permanent expectation. When a Gateway and Governor exist, this
// test is the one that has to change, which is the point: the gap is asserted,
// not assumed.
func TestBuildRefusesWithTodaysAvailableSeams(t *testing.T) {
	_, report, err := Build(BuildInput{Pool: unreachablePool(t)})
	if err == nil {
		t.Fatal("Build must refuse: no production ActionGateway or Governor exists")
	}
	var notComposed *NotComposedError
	if !errors.As(err, &notComposed) {
		t.Fatalf("want *NotComposedError, got %T: %v", err, err)
	}
	for _, want := range []string{"Gateway", "Governor", "TrustedKey", "ApprovalPolicy.CurrentOrgVersion"} {
		if !contains(notComposed.Missing, want) {
			t.Errorf("refusal does not name %s: %v", want, notComposed.Missing)
		}
	}
	// The seams Build DOES own must not appear: reporting Service or Runs
	// missing would mean this composition root is not actually building them.
	for _, unwanted := range []string{"Service", "Runs"} {
		if contains(notComposed.Missing, unwanted) {
			t.Errorf("Build owns %s but reports it missing: %v", unwanted, notComposed.Missing)
		}
	}
	// A report is returned on the failure path too: the limitations do not stop
	// applying because composition was refused.
	if !hasLimitationAbout(report, "IN-MEMORY") {
		t.Errorf("a volatile run ledger must be reported as a limitation, got %v", report.Limitations)
	}
}

// The Orchestrator's own doc comment claims "a fresh Orchestrator resumes
// exactly where a crashed one left off". With MemoryRunStore that is false, so
// the composition root must say so rather than let the claim stand.
func TestBuildReportsAVolatileRunLedger(t *testing.T) {
	_, report, _ := Build(BuildInput{Pool: unreachablePool(t)})
	if report.DurableRunLedger {
		t.Fatal("Build reported a durable run ledger while defaulting to MemoryRunStore")
	}
	if len(report.Limitations) == 0 {
		t.Fatal("a volatile run ledger must produce an operator-facing limitation")
	}
}

// An injected durable ledger must flip the report, or the flag is decorative.
func TestBuildReportsADurableRunLedgerWhenOneIsSupplied(t *testing.T) {
	_, report, _ := Build(BuildInput{Pool: unreachablePool(t), Runs: &countingRunStore{}})
	if !report.DurableRunLedger {
		t.Fatal("an injected RunStore must be reported as durable")
	}
	if hasLimitationAbout(report, "IN-MEMORY") {
		t.Fatalf("the volatile-ledger limitation must not be reported when a ledger was supplied: %v", report.Limitations)
	}
}

// A misconfigured key path must be explained. Left silent, it surfaces only as
// "TrustedKey missing", which sends an operator looking for an unset variable
// that is in fact set and wrong.
func TestBuildExplainsAnUnloadableTrustKey(t *testing.T) {
	_, report, _ := Build(BuildInput{
		Pool:           unreachablePool(t),
		TrustedKeyID:   "nexus-signing-key-1",
		TrustedKeyFile: filepath.Join(t.TempDir(), "absent.key"),
	})
	if !hasLimitationAbout(report, "receipt-signing key could not be loaded") {
		t.Fatalf("an unloadable key must be explained, got %v", report.Limitations)
	}
}

func TestLoadEd25519PublicKeyAcceptsBase64AndHex(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for name, encoded := range map[string]string{
		"base64": base64.StdEncoding.EncodeToString(pub),
		"hex":    hex.EncodeToString(pub),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "nexus.pub")
			if err := os.WriteFile(path, []byte(encoded+"\n"), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			loaded, err := loadEd25519PublicKey(path)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if !loaded.Equal(pub) {
				t.Fatal("loaded key does not match")
			}
		})
	}
}

// A private key on this path would be a 64-byte value whose first 32 bytes are
// the seed. Accepting it would silently install a non-key as the trust anchor.
func TestLoadEd25519PublicKeyRejectsWrongSizedMaterial(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	path := filepath.Join(t.TempDir(), "nexus.key")
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(priv)), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := loadEd25519PublicKey(path); err == nil {
		t.Fatal("a 64-byte private key must be rejected, not truncated to a public key")
	}
}

func TestLoadEd25519PublicKeyRejectsARelativePath(t *testing.T) {
	if _, err := loadEd25519PublicKey("nexus.pub"); err == nil {
		t.Fatal("a relative key path must be rejected")
	}
}

// countingRunStore stands in for a durable ledger. It REFUSES every operation
// rather than pretending to persist: Build never touches it, and a double that
// quietly succeeded would let a future change start using it here without any
// test noticing.
type countingRunStore struct{}

var errNotADurableStore = errors.New("test double: not a real durable run ledger")

func (*countingRunStore) Load(context.Context, string) (workcase.RunLedger, error) {
	return workcase.RunLedger{}, errNotADurableStore
}

func (*countingRunStore) Save(context.Context, workcase.RunLedger) error {
	return errNotADurableStore
}

func (*countingRunStore) Transform(context.Context, string, func(workcase.RunLedger) (workcase.RunLedger, error)) (workcase.RunLedger, error) {
	return workcase.RunLedger{}, errNotADurableStore
}

func hasLimitationAbout(report Report, fragment string) bool {
	for _, limitation := range report.Limitations {
		if strings.Contains(limitation, fragment) {
			return true
		}
	}
	return false
}
