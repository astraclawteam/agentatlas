package workcaseexec

import (
	"context"
	"crypto/ed25519"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"

	nexus "github.com/astraclawteam/agentatlas/sdk/go/nexus"
	sdkworkcase "github.com/astraclawteam/agentatlas/sdk/go/workcase"
	governance "github.com/astraclawteam/agentatlas/services/agentatlas/internal/governance"
	workcase "github.com/astraclawteam/agentatlas/services/agentatlas/internal/workcase"
)

// The defect this guards is the one that produced this package: an
// Orchestrator that is implemented, unit-tested and constructed by nobody. An
// empty Deps must name every seam, so a composition root cannot leave one out
// and still look composed.
func TestMissingRequiredNamesEveryUnwiredSeam(t *testing.T) {
	missing := Deps{}.MissingRequired()
	for _, want := range []string{"Service", "Runs", "Gateway", "Governor", "TrustedKey", "ApprovalPolicy.CurrentOrgVersion"} {
		if !contains(missing, want) {
			t.Errorf("an empty Deps must report %s missing, got %v", want, missing)
		}
	}
}

// An optional seam that starts being reported would make every composition root
// refuse over something that is legitimately absent.
func TestMissingRequiredIgnoresOptionalSeams(t *testing.T) {
	missing := Deps{}.MissingRequired()
	for name := range optionalWorkCaseDeps {
		if contains(missing, name) {
			t.Errorf("%s is declared optional but is reported missing: %v", name, missing)
		}
	}
}

// TestMissingRequiredIsEmptyOnceEverySeamIsWired is the other half: the list has
// to reach empty, or New could never succeed and the refusal would be
// unconditional rather than derived.
func TestMissingRequiredIsEmptyOnceEverySeamIsWired(t *testing.T) {
	if missing := fullDeps(t).MissingRequired(); len(missing) > 0 {
		t.Fatalf("a fully wired Deps still reports %v", missing)
	}
}

// TestOptionalWorkCaseDepsIsExact is what makes this maintainable: a stale entry
// silently downgrades a required seam to optional, which is exactly how the
// wiring gap would come back.
func TestOptionalWorkCaseDepsIsExact(t *testing.T) {
	typ := reflect.TypeOf(Deps{})
	kinds := map[string]reflect.Kind{}
	for i := 0; i < typ.NumField(); i++ {
		if field := typ.Field(i); field.IsExported() {
			kinds[field.Name] = field.Type.Kind()
		}
	}
	var stale []string
	for name := range optionalWorkCaseDeps {
		kind, exists := kinds[name]
		switch {
		case !exists:
			stale = append(stale, name+" (no such field)")
		case kind != reflect.Interface:
			stale = append(stale, name+" (not an interface; MissingRequired never inspects it)")
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Fatalf("optionalWorkCaseDeps has stale entries: %s", strings.Join(stale, ", "))
	}
	// The reverse direction: every interface field is either required (and so
	// reported by an empty Deps) or declared optional here. A field that is
	// neither would be an interface nothing ever decides about.
	missing := Deps{}.MissingRequired()
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if !field.IsExported() || field.Type.Kind() != reflect.Interface {
			continue
		}
		if _, optional := optionalWorkCaseDeps[field.Name]; optional {
			continue
		}
		if !contains(missing, field.Name) {
			t.Errorf("interface field %s is neither declared optional nor reported missing when unset", field.Name)
		}
	}
}

// A refusal has to NAME the seams. "requires Service, Runs, Gateway and
// Governor" -- what workcase.NewOrchestrator says -- does not tell an operator
// which of the four this deployment is actually missing.
func TestNewRefusesAndNamesTheAbsentSeams(t *testing.T) {
	_, err := New(Deps{})
	if err == nil {
		t.Fatal("New must refuse an empty Deps")
	}
	if !errors.Is(err, ErrNotComposed) {
		t.Fatalf("refusal must unwrap to ErrNotComposed, got %v", err)
	}
	var notComposed *NotComposedError
	if !errors.As(err, &notComposed) {
		t.Fatalf("refusal must be a *NotComposedError, got %T", err)
	}
	for _, want := range []string{"Gateway", "Governor", "TrustedKey"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not name %s: %s", want, err)
		}
	}
	// The two seams whose absence is a contract-level fact must say so, or the
	// message reads as a forgotten wire-up that someone will "fix" by injecting
	// a stub.
	if !strings.Contains(err.Error(), "no signing key") {
		t.Errorf("refusal does not explain why Gateway has no source: %s", err)
	}
	if !strings.Contains(err.Error(), "change review, not capabilities") {
		t.Errorf("refusal does not explain why Governor has no source: %s", err)
	}
}

// A short or unidentified trust key is not a relaxed mode: no receipt could ever
// verify, so every side effect would park at result_unknown. It must be refused
// as missing, not accepted.
func TestNewRefusesAnUnusableTrustKey(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Deps)
	}{
		{"short key", func(d *Deps) { d.TrustedKey = ed25519.PublicKey{1, 2, 3} }},
		{"no key id", func(d *Deps) { d.TrustedKeyID = "  " }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps := fullDeps(t)
			tc.mutate(&deps)
			if _, err := New(deps); err == nil || !strings.Contains(err.Error(), "TrustedKey") {
				t.Fatalf("want a refusal naming TrustedKey, got %v", err)
			}
		})
	}
}

// A zero org version fails every upward review, which on every surface looks
// like a governance decision rather than an unset field.
func TestNewRefusesAnUnsetOrgVersion(t *testing.T) {
	deps := fullDeps(t)
	deps.ApprovalPolicy = governance.UpwardReviewPolicy{}
	if _, err := New(deps); err == nil || !strings.Contains(err.Error(), "CurrentOrgVersion") {
		t.Fatalf("want a refusal naming CurrentOrgVersion, got %v", err)
	}
}

func TestNewComposesOnceEverySeamIsPresent(t *testing.T) {
	orch, err := New(fullDeps(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if orch == nil {
		t.Fatal("New returned no orchestrator and no error")
	}
}

// --- doubles -------------------------------------------------------------
//
// These are at least as strict as production: the gateway refuses to dispatch
// (a test double that silently "succeeded" is how a green-but-broken path gets
// built), and the governor refuses to prepare. Composition is what is under
// test here, not execution.

type refusingGateway struct{}

func (refusingGateway) Dispatch(context.Context, nexus.ActionRequest) (nexus.ActionReceipt, error) {
	return nexus.ActionReceipt{}, errors.New("test double: dispatch is not implemented; no production ActionGateway exists")
}

func (refusingGateway) Reconcile(context.Context, nexus.ActionRequest) (nexus.ActionReceipt, bool, error) {
	return nexus.ActionReceipt{}, false, errors.New("test double: reconcile is not implemented")
}

type refusingGovernor struct{}

func (refusingGovernor) PrepareAction(context.Context, sdkworkcase.WorkCase, uint64, sdkworkcase.Step) (nexus.ActionRequest, governance.ApprovalParties, bool, error) {
	return nexus.ActionRequest{}, governance.ApprovalParties{}, false, errors.New("test double: no production Governor exists")
}

func (refusingGovernor) PrepareCompensation(context.Context, sdkworkcase.WorkCase, uint64, sdkworkcase.Step) (nexus.ActionRequest, error) {
	return nexus.ActionRequest{}, errors.New("test double: no production Governor exists")
}

func fullDeps(t *testing.T) Deps {
	t.Helper()
	svc, err := workcase.NewService(workcase.NewMemoryStore(nil), nil)
	if err != nil {
		t.Fatalf("workcase service: %v", err)
	}
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return Deps{
		Service:        svc,
		Runs:           workcase.NewMemoryRunStore(),
		Gateway:        refusingGateway{},
		Governor:       refusingGovernor{},
		TrustedKeyID:   "nexus-signing-key-1",
		TrustedKey:     pub,
		ApprovalPolicy: governance.UpwardReviewPolicy{CurrentOrgVersion: 7},
	}
}

func contains(haystack []string, needle string) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}
	return false
}
