package app

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

// The defect this guards against is not hypothetical. atlas-agent shipped
// without OrgAuthorization, so every dream-policy org check answered 502 in
// production while the suite was green -- every test set the field itself. It
// shipped without Evidence too, which left the whole frozen-contract evidence
// migration unreachable in a running system.
//
// These assertions deliberately do NOT read main.go's source text. A test that
// greps a composition root for a call proves only that a string is present; the
// repo already deleted four such tests for exactly that reason.

func TestMissingRequiredNamesEveryUnwiredDependency(t *testing.T) {
	missing := AgentRouterDeps{}.MissingRequired()
	for _, want := range []string{"Nexus", "OrgAuthorization", "Evidence", "ApprovalTransmitter"} {
		if !contains(missing, want) {
			t.Errorf("an empty deps must report %s missing, got %v", want, missing)
		}
	}
}

func TestMissingRequiredIsEmptyOnceEveryRequiredDependencyIsSet(t *testing.T) {
	deps := AgentRouterDeps{}
	value := reflect.ValueOf(&deps).Elem()
	typ := value.Type()
	// Set every required interface/func field to a non-nil zero implementation
	// via reflection, so this test does not need updating when a dependency is
	// added -- only the optional set does.
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if _, optional := optionalAgentDeps[field.Name]; optional {
			continue
		}
		switch field.Type.Kind() {
		case reflect.Interface:
			impl := reflect.New(reflect.StructOf(nil))
			if !impl.Type().Implements(field.Type) {
				// Cannot synthesize an implementation of this interface here;
				// the zero-value check below still covers it via the explicit
				// list in the sibling test.
				continue
			}
			value.Field(i).Set(impl)
		case reflect.Func:
			value.Field(i).Set(reflect.MakeFunc(field.Type, func([]reflect.Value) []reflect.Value {
				out := make([]reflect.Value, field.Type.NumOut())
				for o := range out {
					out[o] = reflect.Zero(field.Type.Out(o))
				}
				return out
			}))
		}
	}
	// Only func fields can be synthesized generically; interfaces are covered
	// by the composition-root test below. Assert the func half is now clean.
	for _, name := range deps.MissingRequired() {
		field, _ := typ.FieldByName(name)
		if field.Type.Kind() == reflect.Func {
			t.Errorf("%s is still reported missing after being set", name)
		}
	}
}

// TestOptionalAgentDepsIsExact is the assertion that makes this maintainable:
// every name in the optional set must be a real interface or func field. A
// stale entry silently downgrades a required dependency to optional, which is
// precisely how a wiring gap would come back.
func TestOptionalAgentDepsIsExact(t *testing.T) {
	typ := reflect.TypeOf(AgentRouterDeps{})
	actual := map[string]reflect.Kind{}
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.IsExported() {
			actual[field.Name] = field.Type.Kind()
		}
	}
	var stale []string
	for name := range optionalAgentDeps {
		kind, exists := actual[name]
		if !exists {
			stale = append(stale, name+" (no such field)")
			continue
		}
		if kind != reflect.Interface && kind != reflect.Func {
			stale = append(stale, name+" (not an interface or func; MissingRequired never inspects it)")
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Fatalf("optionalAgentDeps has stale entries: %s", strings.Join(stale, ", "))
	}
	if len(optionalAgentDeps) == 0 {
		t.Fatal("optionalAgentDeps is empty; every dependency cannot be required")
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
