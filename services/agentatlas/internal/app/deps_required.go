package app

import (
	"reflect"
	"sort"
)

// optionalAgentDeps names every interface-typed AgentRouterDeps field a binary
// may legitimately leave unset. Everything else is required.
//
// This exists because of a defect class that hit this repo repeatedly: a
// dependency is implemented, unit-tested, and never constructed by any
// composition root. Nothing crashes, because these deps fail closed -- the
// feature simply does not exist, while the suite stays green because every test
// sets the field itself. OrgAuthorization answered 502 in production that way,
// and Evidence made a whole migration unreachable.
//
// The set is asserted EXACT in both directions by TestOptionalAgentDepsIsExact,
// so adding an interface field forces a decision: wire it in every composition
// root, or declare it optional here and say why. Silence is no longer an
// option, which is the whole point.
var optionalAgentDeps = map[string]string{
	"BrowserOrgStore":       "defaults to Store when unset",
	"BrowserKnowledgeStore": "defaults to Store when unset",
	"BrowserAuthorizer":     "only the advanced legacy BFF routes need it",
	"Outlines":              "defaults to Store when unset",
	"DreamRuns":             "not every binary serves dream runs",
	"DreamRerun":            "not every binary re-runs dreams",
	"WorkCaseContextFor":    "defaults to governance.WorkCaseContextFor when unset",
}

// MissingRequired reports the required interface-typed dependencies this value
// leaves nil, sorted. An empty result means every dependency the router needs
// is present; it does NOT mean each one works.
//
// Only interface and func fields are considered. Concrete pointers (the browser
// session service, the governance change service, the assessment service) are
// left alone deliberately: several are genuinely optional and their own code
// already nil-checks them, so including them would force a second, noisier
// list.
func (d AgentRouterDeps) MissingRequired() []string {
	value := reflect.ValueOf(d)
	typ := value.Type()
	var missing []string
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if !field.IsExported() {
			continue
		}
		switch field.Type.Kind() {
		case reflect.Interface, reflect.Func:
		default:
			continue
		}
		if _, optional := optionalAgentDeps[field.Name]; optional {
			continue
		}
		if value.Field(i).IsNil() {
			missing = append(missing, field.Name)
		}
	}
	// Dependencies is a slice, so the reflective sweep above cannot see it. It
	// is checked by hand because an empty one is precisely the false-green this
	// list exists to catch: the health surface publishes the names of the
	// probes registered here, so a composition root that registers none serves
	// an empty dependency list and an unconditional ready:true.
	if len(d.Dependencies) == 0 {
		missing = append(missing, "Dependencies")
	}
	sort.Strings(missing)
	return missing
}
