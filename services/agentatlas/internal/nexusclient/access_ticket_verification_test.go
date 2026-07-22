package nexusclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/astraclawteam/agentatlas/sdk/go/nexus"
)

// The frozen grammars StepGrantVerifyRequest composes, copied from the pinned
// snapshot's Sha256Ref/Capability/GrantRef schemas. They are duplicated here
// rather than derived so that a snapshot edit that loosened one of them shows
// up as a disagreement instead of being silently adopted.
var (
	frozenGrantRef      = regexp.MustCompile(`^grant_[A-Za-z0-9_-]{16,128}$`)
	frozenCapability    = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)
	frozenParameterHash = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// identityVocabulary is the set of member names nexus.VerifyTicketResponse is
// built from — the answer to "who is this actor and what may they do" that
// ticketGuard consumes. Every one of them is a JSON name, so a contract that
// could answer ticketGuard's question would have to declare at least one.
var identityVocabulary = []string{"enterprise_id", "actor_user_id", "scopes"}

// TestFrozenContractHasNoAccessTicketVerification is the enumeration behind the
// finding, held against the digest-pinned snapshot rather than against memory.
//
// AgentAtlas's ticketGuard (internal/app/agent_server.go) is built on a round
// trip: hand AgentNexus an opaque Access Ticket, get back the actor's identity
// and scopes, then authorize locally from the result. That round trip has no
// counterpart on the frozen contract, and this test pins the three independent
// facts that make it so:
//
//  1. POST /v1/tickets/verify — the path the client calls — is verifyStepGrant.
//     A Step Grant is not an Access Ticket: it authorizes ONE exact operation
//     (grant_ref + capability + parameter_hash) and its response carries no
//     actor at all. Correcting the body and the credential therefore cannot
//     help; it produces a well-formed answer to a different question.
//  2. No schema anywhere in the snapshot declares the identity vocabulary, so
//     no other operation can be quietly serving the same need under another
//     name.
//  3. The one operation that DOES return a full principal identity,
//     getBrowserSession, does not accept the caseTicket scheme — an Access
//     Ticket holder cannot call it.
//
// Together these say the gap is a contract gap, not a client bug. If AgentNexus
// later freezes a real access-ticket introspection operation, assertion 2 fails
// with "revisit" rather than letting ticketGuard stay wired to a step-grant
// check.
func TestFrozenContractHasNoAccessTicketVerification(t *testing.T) {
	document := readOpenAPIMap(t, filepath.Join("testdata", publishedGatewayRuntimeSnapshot))

	// (1) The path AgentAtlas calls verifies a Step Grant, and answers with no actor.
	verify := nestedOpenAPIValue(t, document, "paths", "/v1/tickets/verify", "post")
	operation, _ := verify.(map[string]any)
	if got, _ := operation["operationId"].(string); got != "verifyStepGrant" {
		t.Fatalf("POST /v1/tickets/verify is operationId %q; this test and the finding it pins assume verifyStepGrant", got)
	}
	requestRef := schemaRefName(t, nestedOpenAPIValue(t, document,
		"paths", "/v1/tickets/verify", "post", "requestBody", "content", "application/json", "schema"))
	if requestRef != "StepGrantVerifyRequest" {
		t.Fatalf("verifyStepGrant takes %q, want StepGrantVerifyRequest", requestRef)
	}
	request, _ := nestedOpenAPIValue(t, document, "components", "schemas", "StepGrantVerifyRequest").(map[string]any)
	if additional, ok := request["additionalProperties"].(bool); !ok || additional {
		t.Fatalf("StepGrantVerifyRequest is not additionalProperties:false (%v); the finding assumes a strict body", request["additionalProperties"])
	}
	if want := []string{"capability", "grant_ref", "parameter_hash"}; !sameStrings(stringsOf(request["required"]), want) {
		t.Fatalf("StepGrantVerifyRequest requires %v, want %v", stringsOf(request["required"]), want)
	}

	response, _ := nestedOpenAPIValue(t, document, "components", "schemas", "StepGrantVerifyResponse").(map[string]any)
	responseMembers := propertyNames(response)
	for _, member := range identityVocabulary {
		if contains(responseMembers, member) {
			t.Fatalf("StepGrantVerifyResponse declares %q; if verifyStepGrant now answers with an actor identity, "+
				"ticketGuard's round trip may have a counterpart after all and this finding must be revisited", member)
		}
	}
	// Not a single member of what ticketGuard needs. Named explicitly so the
	// assertion above cannot pass merely because the schema shrank to nothing.
	if want := []string{"capability", "expires_at", "grant_ref", "one_use", "parameter_hash", "valid"}; !sameStrings(responseMembers, want) {
		t.Fatalf("StepGrantVerifyResponse has members %v, want %v", responseMembers, want)
	}

	// (2) The identity vocabulary appears nowhere in the contract.
	declared := allPropertyNames(document)
	for _, member := range identityVocabulary {
		if declared[member] {
			t.Fatalf("the frozen contract now declares %q somewhere; an operation may finally answer "+
				"ticketGuard's question, so the no-access-ticket-verification finding must be revisited", member)
		}
	}

	// (3) The one operation that returns a principal identity refuses the Access Ticket.
	session, _ := nestedOpenAPIValue(t, document, "paths", "/v1/browser-sessions/me", "get").(map[string]any)
	sessionSchema, _ := nestedOpenAPIValue(t, document, "components", "schemas", "BrowserSession").(map[string]any)
	for _, member := range []string{"principal_ref", "tenant_ref", "org_version", "org_unit_ids"} {
		if !contains(propertyNames(sessionSchema), member) {
			t.Fatalf("BrowserSession no longer declares %q; it was the closest frozen shape to VerifyTicketResponse", member)
		}
	}
	if schemes := securitySchemes(session); contains(schemes, "caseTicket") {
		t.Fatalf("getBrowserSession now accepts caseTicket (%v): an Access Ticket holder could resolve its own actor "+
			"through it, which is exactly what ticketGuard needs — revisit the finding", schemes)
	}
}

// TestAccessTicketVerificationHasNoFrozenCounterpart drives the real
// HTTPClient against a server that behaves the way the frozen contract says
// AgentNexus behaves, and is DELIBERATELY RED.
//
// The two rejection subtests are the mutation check, and they are separate on
// purpose: asserting only that the wrong credential is refused is equally
// satisfied by a server that ignores the body, and asserting only that the
// wrong body is refused is equally satisfied by a server that authenticates
// nobody. Asserting one half and calling it covered is precisely how the same
// defect on /v1/runtime/locate and /v1/runtime/read survived until 406e2f2.
//
// The third subtest states what ticketGuard actually needs and fails, because
// nothing on this contract can supply it. That failure is the finding, not a
// flake: see TestFrozenContractHasNoAccessTicketVerification for the
// enumeration proving no other operation answers it either. Do NOT make this
// green by reshaping the call into a step-grant verification — a well-formed
// StepGrantVerifyRequest earns a response with no actor in it, which is not
// progress over the 400 the current body earns.
func TestAccessTicketVerificationHasNoFrozenCounterpart(t *testing.T) {
	const serviceSecret = "AgentAtlas-Nexus-Service-Q7mV2xK9pR4tY8dF3"
	const accessTicket = "tick_frozen_contract_0001"
	srv := httptest.NewServer(frozenStepGrantVerifyHandler(t))
	t.Cleanup(srv.Close)

	client, err := New(srv.URL, 5*time.Second, "agentatlas", writeServiceSecret(t, serviceSecret), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	t.Run("the service credential is not an accepted scheme", func(t *testing.T) {
		// VerifyTicket routes through doPost, whose allowlist attaches Basic
		// auth to this path. verifyStepGrant declares only browserSession,
		// browserAccessToken and caseTicket, so a real AgentNexus refuses it.
		_, err := client.VerifyTicket(ctx, nexus.VerifyTicketRequest{TicketID: accessTicket})
		if err == nil {
			t.Fatal("VerifyTicket succeeded against a server that refuses the service credential; " +
				"the client is presenting a credential verifyStepGrant does not declare, and this test no longer detects it")
		}
		// Specifically 401, not merely "an error". This call sends BOTH a
		// credential the operation does not declare AND a body it does not
		// accept, so an assertion that only required failure would be
		// satisfied by the 400 the body earns and would stop noticing the
		// credential defect entirely.
		if !strings.Contains(err.Error(), "401") {
			t.Fatalf("VerifyTicket failed with %v, want a 401: the request must be turned away on its credential, "+
				"before the body is ever read", err)
		}
	})

	t.Run("the request body is not a StepGrantVerifyRequest", func(t *testing.T) {
		// Isolated from the credential half: this request carries a perfectly
		// good Access Ticket, so a rejection can only be about the body.
		status, _ := postFrozenVerify(t, srv.URL, "CaseTicket "+accessTicket,
			nexus.VerifyTicketRequest{TicketID: accessTicket})
		if status != http.StatusBadRequest {
			t.Fatalf("a {\"ticket_id\":...} body under a valid Access Ticket got %d, want 400: "+
				"StepGrantVerifyRequest is additionalProperties:false and requires grant_ref, capability and parameter_hash", status)
		}
	})

	t.Run("ticketGuard can resolve the actor behind an Access Ticket", func(t *testing.T) {
		// The requirement, stated against the best case the contract allows:
		// the accepted credential AND the exact frozen body.
		status, body := postFrozenVerify(t, srv.URL, "CaseTicket "+accessTicket, map[string]any{
			"grant_ref":      "grant_frozen0000000000",
			"capability":     "workflow.publish",
			"parameter_hash": "sha256:" + strings.Repeat("0", 64),
		})
		if status != http.StatusOK {
			t.Fatalf("a well-formed verifyStepGrant call got %d; the server double is wrong, fix it before reading the failure below", status)
		}
		var missing []string
		for _, member := range identityVocabulary {
			if _, present := body[member]; !present {
				missing = append(missing, member)
			}
		}
		if len(missing) > 0 {
			t.Fatalf(`ticketGuard cannot be satisfied on this contract.

The only thing POST /v1/tickets/verify answers is verifyStepGrant, and its
response is missing every member ticketGuard needs: %v.
It returned %v — a Step Grant's own binding, no actor.

ticketGuard asks "who is this actor and what may they do". On the frozen
contract that question has no endpoint: an Access Ticket is a CREDENTIAL to
present, not a token to introspect. AgentNexus resolves it at ingress from
"Authorization: CaseTicket <opaque>" and no operation exposes the result.

Fixing HTTPClient.VerifyTicket's body and credential does not close this.
Deciding what replaces the round trip is a contract question, not a client
change — see TestFrozenContractHasNoAccessTicketVerification.`,
				missing, sortedMembers(body))
		}
	})
}

// frozenStepGrantVerifyHandler serves POST /v1/tickets/verify the way the
// pinned snapshot declares it: the three accepted schemes and nothing else,
// then a strictly decoded StepGrantVerifyRequest, then a StepGrantVerifyResponse.
//
// It rejects the credential before looking at the body, matching a real
// AgentNexus — the gateway resolves one immutable trusted context at ingress,
// so an unauthenticated caller must not be able to tell a well-formed body from
// a malformed one.
func frozenStepGrantVerifyHandler(t *testing.T) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/tickets/verify", serveFrozenStepGrantVerify)
	return mux
}

// serveFrozenStepGrantVerify is the handler itself, shared with contractServer
// in client_contract_test.go so that there is exactly ONE description of how
// this operation behaves. A second, more permissive copy living next to the
// client under test is how the retired shape kept its endorsement.
func serveFrozenStepGrantVerify(w http.ResponseWriter, r *http.Request) {
	if _, _, isBasic := r.BasicAuth(); isBasic || !acceptedActorCredential(r) {
		http.Error(w, `{"code":"invalid_credential"}`, http.StatusUnauthorized)
		return
	}
	var request struct {
		GrantRef      string `json:"grant_ref"`
		Capability    string `json:"capability"`
		ParameterHash string `json:"parameter_hash"`
	}
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<16))
	decoder.DisallowUnknownFields() // additionalProperties: false
	if err := decoder.Decode(&request); err != nil ||
		!frozenGrantRef.MatchString(request.GrantRef) ||
		!frozenCapability.MatchString(request.Capability) ||
		!frozenParameterHash.MatchString(request.ParameterHash) {
		http.Error(w, `{"code":"invalid_request"}`, http.StatusBadRequest)
		return
	}
	// StepGrantVerifyResponse, member for member. Deliberately nothing more:
	// inventing an actor here would make the double the only reason
	// ticketGuard looked satisfiable.
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"valid": true, "grant_ref": request.GrantRef,
		"capability": request.Capability, "parameter_hash": request.ParameterHash,
		"one_use": true, "expires_at": time.Now().Add(time.Minute).UTC().Format(time.RFC3339),
	})
}

// acceptedActorCredential reports whether r carries one of the three schemes
// verifyStepGrant declares. It mirrors assertAcceptedActorCredential, but
// returns a decision instead of failing the test: this one drives a server.
func acceptedActorCredential(r *http.Request) bool {
	scheme, credential, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	if strings.EqualFold(scheme, "CaseTicket") || strings.EqualFold(scheme, "Bearer") {
		return strings.TrimSpace(credential) != ""
	}
	cookie, err := r.Cookie(browserSessionCookie)
	return err == nil && cookie.Value != ""
}

// postFrozenVerify sends body to /v1/tickets/verify under one exact
// Authorization value, bypassing HTTPClient so the credential and the body can
// be varied independently.
func postFrozenVerify(t *testing.T, baseURL, authorization string, body any) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/tickets/verify", strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", authorization)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// --- OpenAPI helpers -------------------------------------------------------

// schemaRefName returns the schema name a {$ref: '#/components/schemas/X'}
// node points at.
func schemaRefName(t *testing.T, node any) string {
	t.Helper()
	object, ok := node.(map[string]any)
	if !ok {
		t.Fatalf("schema node is not an object: %v", node)
	}
	ref, _ := object["$ref"].(string)
	return strings.TrimPrefix(ref, "#/components/schemas/")
}

// propertyNames returns the sorted property names one schema object declares.
func propertyNames(schema map[string]any) []string {
	properties, _ := schema["properties"].(map[string]any)
	names := make([]string, 0, len(properties))
	for name := range properties {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// allPropertyNames collects every property name declared anywhere in the
// document, at any nesting depth. A contract that answered ticketGuard's
// question would have to name the actor somewhere, whatever it called the
// operation, so this is the check that keeps the finding from being scoped to
// the paths that happened to be inspected by hand.
func allPropertyNames(node any) map[string]bool {
	found := map[string]bool{}
	var walk func(any)
	walk = func(value any) {
		switch typed := value.(type) {
		case map[string]any:
			if properties, ok := typed["properties"].(map[string]any); ok {
				for name := range properties {
					found[name] = true
				}
			}
			for _, child := range typed {
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(node)
	return found
}

// securitySchemes flattens one operation's security requirement list into the
// scheme names it accepts.
func securitySchemes(operation map[string]any) []string {
	requirements, _ := operation["security"].([]any)
	var schemes []string
	for _, requirement := range requirements {
		object, ok := requirement.(map[string]any)
		if !ok {
			continue
		}
		for name := range object {
			schemes = append(schemes, name)
		}
	}
	sort.Strings(schemes)
	return schemes
}

func stringsOf(node any) []string {
	items, _ := node.([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, fmt.Sprint(item))
	}
	sort.Strings(out)
	return out
}

func sameStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func sortedMembers(body map[string]any) []string {
	names := make([]string, 0, len(body))
	for name := range body {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
