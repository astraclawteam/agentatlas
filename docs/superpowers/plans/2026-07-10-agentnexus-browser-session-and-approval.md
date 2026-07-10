# AgentNexus Browser Session and Approval Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make AgentNexus the enterprise browser identity, scoped authorization, Step Grant, and upward-review authority for AgentAtlas.

**Architecture:** AgentNexus acts as an OIDC relying party to the enterprise IdP and as an authorization server to registered first-party consoles. It issues one-time Authorization Code + PKCE exchanges, stores opaque browser sessions server-side, evaluates scoped permissions, and resolves high-risk reviewers from the versioned organization graph.

**Tech Stack:** Go, net/http ServeMux, PostgreSQL/sqlc, `golang.org/x/oauth2`, `github.com/coreos/go-oidc/v3/oidc`, OpenFGA/policy evaluator, AgentNexus audit chain

## Global Constraints

- Login uses silent session first, then full-page redirect; no popup, AgentAtlas password form, pasted ticket, or JavaScript-persisted bearer token.
- Authorization codes are one-time, expire in 60 seconds, and require S256 PKCE.
- Browser sessions are opaque, server-side, idle-expire after 8 hours, absolute-expire after 24 hours, and are stored only in `HttpOnly; Secure; SameSite=Lax` cookies.
- Login session proves identity only; sensitive reads/writes require a new short-lived Case Ticket or Step Grant.
- Permission names are exactly `suggest`, `edit`, `publish_low_risk`, `approve_high_risk`, `workflow_edit`, `workflow_advanced`, and `service_mode`.
- Permission decisions bind enterprise, organization scope, resource type, resource id, and action.
- High-risk reviewer resolution walks parent organization units; requester is always excluded; no reviewer falls back to the enterprise knowledge-admin queue and never auto-publishes.
- Agent explanations cannot lower deterministic risk; an authorized administrator may raise it.
- Session, grant, approval, logout, and evidence-drill-down events append to the audit hash chain; audit failure fails closed for sensitive operations.

---

## File Map

- Add migration `000002_browser_sessions_and_approvals.sql` and queries in `db/queries/auth.sql` and `approval.sql`.
- Add `internal/browserauth/` for OIDC, authorization codes, and sessions.
- Extend `internal/policy/` with the AgentAtlas permission vocabulary and scoped decisions.
- Add `internal/approval/` for risk and organization-upward routing.
- Wire public endpoints in `internal/app/gateway_api.go` through explicit dependencies instead of globals.

### Task 1: Persist Opaque Sessions and One-Time Authorization Codes

**Files:**
- Create: `../agentnexus/services/agentnexus/db/migrations/000002_browser_sessions_and_approvals.sql`
- Create: `../agentnexus/services/agentnexus/db/queries/auth.sql`
- Create: `../agentnexus/services/agentnexus/internal/browserauth/model.go`
- Create: `../agentnexus/services/agentnexus/internal/browserauth/service.go`
- Test: `../agentnexus/services/agentnexus/internal/browserauth/service_test.go`

**Interfaces:**
- Consumes: `enterprise_users` and `external_identities` from migration 000001.
- Produces: `Service.CreateSession`, `GetSession`, `RevokeSession`, `IssueCode`, and `ExchangeCode`.

- [ ] **Step 1: Write failing expiry and one-time exchange tests**

```go
func TestExchangeCodeIsOneTimeAndPKCEBound(t *testing.T) {
    svc := newTestService(fixedNow)
    code := svc.mustIssueCode(IssueCodeInput{ClientID: "agentatlas", RedirectURI: "https://atlas/auth/callback", CodeChallenge: s256("verifier")})
    if _, err := svc.ExchangeCode(code, "wrong", "agentatlas", "https://atlas/auth/callback"); !errors.Is(err, ErrInvalidGrant) { t.Fatal(err) }
    if _, err := svc.ExchangeCode(code, "verifier", "agentatlas", "https://atlas/auth/callback"); err != nil { t.Fatal(err) }
    if _, err := svc.ExchangeCode(code, "verifier", "agentatlas", "https://atlas/auth/callback"); !errors.Is(err, ErrInvalidGrant) { t.Fatal("code reused") }
}
```

- [ ] **Step 2: Verify failure**

Run: `cd ../agentnexus/services/agentnexus && go test ./internal/browserauth -count=1`

Expected: FAIL because the package is absent.

- [ ] **Step 3: Add the migration**

```sql
CREATE TABLE browser_sessions (
  id_hash text PRIMARY KEY, enterprise_id text NOT NULL REFERENCES enterprises(id),
  enterprise_user_id text NOT NULL REFERENCES enterprise_users(id),
  created_at timestamptz NOT NULL, last_seen_at timestamptz NOT NULL,
  idle_expires_at timestamptz NOT NULL, absolute_expires_at timestamptz NOT NULL,
  revoked_at timestamptz, user_agent_hash text NOT NULL DEFAULT ''
);
CREATE TABLE oauth_authorization_codes (
  code_hash text PRIMARY KEY, client_id text NOT NULL, redirect_uri text NOT NULL,
  enterprise_id text NOT NULL, enterprise_user_id text NOT NULL,
  code_challenge text NOT NULL, nonce text NOT NULL,
  expires_at timestamptz NOT NULL, consumed_at timestamptz
);
CREATE TABLE approval_queue_items (
  id text PRIMARY KEY, enterprise_id text NOT NULL, requester_user_id text NOT NULL,
  resource_type text NOT NULL, resource_id text NOT NULL, action text NOT NULL,
  risk_level text NOT NULL, org_unit_id text NOT NULL, reviewer_user_id text,
  status text NOT NULL, created_at timestamptz NOT NULL DEFAULT now()
);
```

Store only SHA-256 hashes of opaque session IDs and authorization codes.

- [ ] **Step 4: Implement the service contract**

```go
type BrowserSession struct { EnterpriseID, UserID string; IdleExpiresAt, AbsoluteExpiresAt time.Time }
type ExchangeResult struct { EnterpriseID, UserID string }
func (s *Service) ExchangeCode(code, verifier, clientID, redirectURI string) (ExchangeResult, error)
```

Use `subtle.ConstantTimeCompare` for S256 comparison, consume the code in the same transaction as validation, and reject redirect/client mismatches.

- [ ] **Step 5: Generate DB code and run tests**

Run: `make generate && go test ./internal/browserauth -count=1`

Expected: PASS; reuse, expiry, bad verifier, wrong redirect, idle expiry, absolute expiry, and revoked session cases all pass.

- [ ] **Step 6: Commit**

```bash
git add services/agentnexus/db services/agentnexus/internal/browserauth
git commit -m "feat: persist browser sessions and authorization codes"
```

### Task 2: Implement Enterprise IdP Login and Console OIDC Endpoints

**Files:**
- Create: `../agentnexus/services/agentnexus/internal/browserauth/oidc.go`
- Create: `../agentnexus/services/agentnexus/internal/app/browser_auth.go`
- Modify: `../agentnexus/services/agentnexus/internal/app/gateway_api.go`
- Modify: `../agentnexus/services/agentnexus/cmd/gateway-api/main.go`
- Modify: `../agentnexus/services/agentnexus/go.mod`
- Test: `../agentnexus/services/agentnexus/internal/app/browser_auth_test.go`

**Interfaces:**
- Consumes: `browserauth.Service` and enterprise IdP discovery configuration.
- Produces: `GET /oauth2/authorize`, `GET /oauth2/idp/callback`, `POST /oauth2/token`, `GET /.well-known/openid-configuration`, `GET /oauth2/jwks`, `GET /v1/browser-sessions/me`, and `POST /v1/browser-sessions/logout`.

- [ ] **Step 1: Write full-page redirect tests**

```go
func TestAuthorizeWithoutSessionRedirectsToEnterpriseIdP(t *testing.T) {
    rr := request(router, "GET", "/oauth2/authorize?client_id=agentatlas&redirect_uri=https%3A%2F%2Fatlas%2Fauth%2Fcallback&state=s1&code_challenge=c&code_challenge_method=S256")
    if rr.Code != http.StatusFound { t.Fatalf("status=%d", rr.Code) }
    if !strings.Contains(rr.Header().Get("Location"), fakeIDP.URL+"/authorize") { t.Fatal(rr.Header()) }
}
```

- [ ] **Step 2: Verify failure**

Run: `go test ./internal/app -run BrowserAuth -count=1`

Expected: FAIL on missing routes.

- [ ] **Step 3: Add typed configuration and OIDC verification**

```go
type OIDCConfig struct {
  EnterpriseIssuerURL, PublicIssuerURL, ClientID, ClientSecret, CallbackURL string
  ConsoleClients map[string][]string // client id -> exact redirect URIs
  SigningKeyID string
  SigningPrivateKey crypto.Signer
}
```

Use discovery and ID token verification from `coreos/go-oidc`; map `(issuer, sub)` through `external_identities`. Reject unknown enterprise identities instead of auto-provisioning. Load the signing key from the deployment secret provider, expose only its public JWK, and support current plus previous key IDs during rotation; never store a private key in the database or repository.

- [ ] **Step 4: Implement cookie and endpoint behavior**

The IdP callback creates a server session and sets `nexus_browser_session` with `HttpOnly`, `Secure`, `SameSite=Lax`, `Path=/`. `/oauth2/authorize` silently issues a one-time code when the cookie is valid. `/oauth2/token` accepts only `grant_type=authorization_code` and returns a signed ID token with `iss`, `sub`, `aud`, `exp` (maximum 5 minutes), `iat`, `nonce`, `enterprise_id`, and `enterprise_user_id` to the confidential AgentAtlas BFF; it does not return a long-lived browser token. Discovery and JWKS endpoints let AgentAtlas verify signature, issuer, audience, expiry, and nonce.

- [ ] **Step 5: Run route and security tests**

Run: `go test ./internal/app ./internal/browserauth -count=1`

Expected: PASS for state/nonce mismatch, redirect allow-list, PKCE S256, unknown subject, silent session, callback, signed ID token claims/JWKS rotation, exchange, logout, and post-logout session lookup.

- [ ] **Step 6: Commit**

```bash
git add services/agentnexus/internal/browserauth services/agentnexus/internal/app services/agentnexus/cmd/gateway-api services/agentnexus/go.mod services/agentnexus/go.sum
git commit -m "feat: add AgentNexus browser OIDC flow"
```

### Task 3: Add Scoped AgentAtlas Permission Decisions

**Files:**
- Create: `../agentnexus/services/agentnexus/internal/policy/agentatlas.go`
- Create: `../agentnexus/services/agentnexus/internal/app/authorization.go`
- Modify: `../agentnexus/services/agentnexus/internal/app/gateway_api.go`
- Test: `../agentnexus/services/agentnexus/internal/policy/agentatlas_test.go`
- Test: `../agentnexus/services/agentnexus/internal/app/authorization_test.go`

**Interfaces:**
- Consumes: authenticated session/ticket actor and organization memberships.
- Produces: `POST /v1/authorization/decisions` returning allow/deny, scopes, masks, deterministic risk, and permitted fallback action.

- [ ] **Step 1: Write the permission matrix test**

```go
cases := []struct{ permission, org, action string; want bool }{
  {"suggest", "dept-rd", "knowledge.suggest", true},
  {"edit", "dept-rd", "knowledge.update", true},
  {"edit", "dept-sales", "knowledge.update", false},
  {"workflow_advanced", "dept-rd", "workflow.edit_advanced", false},
}
```

- [ ] **Step 2: Verify failure**

Run: `go test ./internal/policy -run AgentAtlas -count=1`

Expected: FAIL because the vocabulary/evaluator is missing.

- [ ] **Step 3: Implement typed requests and fail-closed decisions**

```go
type AtlasPermission string
const (PermissionSuggest AtlasPermission = "suggest"; PermissionEdit AtlasPermission = "edit"; PermissionPublishLowRisk AtlasPermission = "publish_low_risk"; PermissionApproveHighRisk AtlasPermission = "approve_high_risk"; PermissionWorkflowEdit AtlasPermission = "workflow_edit"; PermissionWorkflowAdvanced AtlasPermission = "workflow_advanced"; PermissionServiceMode AtlasPermission = "service_mode")
type ScopedRequest struct { EnterpriseID, UserID, OrgUnitID, ResourceType, ResourceID, Action string }
```

Unknown action, missing scope, stale org version, or unavailable policy service returns deny. A denied `knowledge.update` may return `suggest` only when that permission is explicitly allowed.

- [ ] **Step 4: Expose the endpoint behind session-or-ticket actor middleware**

The response is:

```json
{"decision":"allow","permissions":["edit"],"org_unit_ids":["dept-rd"],"risk_level":"low","mask_fields":[],"org_version":42}
```

- [ ] **Step 5: Run tests**

Run: `go test ./internal/policy ./internal/app -run 'AgentAtlas|Authorization' -count=1`

Expected: PASS for every permission, cross-enterprise denial, parent/child scope, stale org graph, and fallback suggestion.

- [ ] **Step 6: Commit**

```bash
git add services/agentnexus/internal/policy services/agentnexus/internal/app
git commit -m "feat: authorize scoped AgentAtlas operations"
```

### Task 4: Resolve Low- and High-Risk Approval Routes Upward

**Files:**
- Create: `../agentnexus/services/agentnexus/db/queries/approval.sql`
- Create: `../agentnexus/services/agentnexus/internal/approval/risk.go`
- Create: `../agentnexus/services/agentnexus/internal/approval/resolver.go`
- Create: `../agentnexus/services/agentnexus/internal/app/approval.go`
- Test: `../agentnexus/services/agentnexus/internal/approval/resolver_test.go`

**Interfaces:**
- Consumes: requester, organization unit, proposed action, changed fields, impacted people/orgs, and permission evaluator.
- Produces: `POST /v1/approvals/resolve` and an `ApprovalRoute` with risk reasons and next reviewer/queue.

- [ ] **Step 1: Write upward-resolution tests**

```go
func TestHighRiskWalksUpAndExcludesRequester(t *testing.T) {
  route := resolver.Resolve(Request{RequesterUserID:"u-manager", OrgUnitID:"team-a", RiskLevel:"high"})
  if route.ReviewerUserID != "u-dept-head" { t.Fatalf("route=%+v", route) }
  if route.ReviewerUserID == route.RequesterUserID { t.Fatal("self review") }
}
func TestNoReviewerFallsBackToKnowledgeAdminQueue(t *testing.T) {
  route := resolver.Resolve(Request{RequesterUserID:"u1", OrgUnitID:"company", RiskLevel:"high"})
  if route.Queue != "enterprise_knowledge_admin" || route.AutoPublish { t.Fatalf("route=%+v", route) }
}
```

- [ ] **Step 2: Verify failure**

Run: `go test ./internal/approval -count=1`

Expected: FAIL because resolver is missing.

- [ ] **Step 3: Implement deterministic risk reasons**

At minimum, high risk is forced by published workflow/SOP behavior changes, permission/approval changes, evidence requirements, execution deadlines, external side effects, or enterprise-configured minimum risk. Scope size and impacted users may raise low to high. The request may supply a higher risk override but never a lower one.

- [ ] **Step 4: Implement reviewer walk**

```go
for unit := start; unit != nil; unit = graph.Parent(unit.ID) {
  for _, candidate := range graph.ResponsibleUsers(unit.ID) {
    if candidate.ID != req.RequesterUserID && auth.Allows(candidate.ID, "approve_high_risk", unit.ID) { return reviewerRoute(candidate, unit) }
  }
}
return adminQueueRoute("enterprise_knowledge_admin")
```

Low risk returns `single_confirmation` only when requester has `publish_low_risk`; otherwise it returns an authorized reviewer rather than publishing.

- [ ] **Step 5: Persist and expose route resolution**

Create `approval_queue_items` only for high-risk or unauthorized low-risk changes. Store the org version and risk reasons in an added JSONB column so later changes to the org graph do not rewrite historical routing evidence.

- [ ] **Step 6: Run tests and commit**

Run: `go test ./internal/approval ./internal/app -run Approval -count=1`

Expected: PASS for self-review exclusion, skipped unauthorized managers, multi-level ascent, no-reviewer queue, low-risk direct confirmation, and cross-enterprise denial.

```bash
git add services/agentnexus/db services/agentnexus/internal/approval services/agentnexus/internal/app
git commit -m "feat: route governed changes through organization hierarchy"
```

### Task 5: Issue Step Grants for Sensitive Actions and Dream Drill-Down

**Files:**
- Modify: `../agentnexus/services/agentnexus/internal/tickets/case_ticket.go`
- Modify: `../agentnexus/services/agentnexus/internal/tickets/step_grant.go`
- Create: `../agentnexus/services/agentnexus/internal/app/grants.go`
- Modify: `../agentnexus/services/agentnexus/internal/app/gateway_api.go`
- Test: `../agentnexus/services/agentnexus/internal/tickets/grant_authorization_test.go`

**Interfaces:**
- Consumes: authenticated actor plus an allowed scoped authorization decision.
- Produces: `POST /v1/step-grants`, `POST /v1/tickets/verify`, and grants constrained to resource/action/scope with a maximum TTL of 5 minutes.

- [ ] **Step 1: Write the sensitive-grant test**

```go
grant, err := svc.AuthorizeAndCreateGrant(ctx, actor, CreateStepGrantInput{ResourceType:"dream_evidence", ResourceID:"ev-1", Action:"read", TTL:10*time.Minute})
if err != nil { t.Fatal(err) }
if grant.ExpiresAt.Sub(now) != 5*time.Minute { t.Fatalf("ttl=%s", grant.ExpiresAt.Sub(now)) }
if !slices.Contains(grant.Scopes, "dream:evidence:read") { t.Fatal(grant.Scopes) }
```

- [ ] **Step 2: Verify failure**

Run: `go test ./internal/tickets -run GrantAuthorization -count=1`

Expected: FAIL because authorization is not integrated.

- [ ] **Step 3: Add authorize-before-issue behavior**

Grant issuance checks current organization scope and resource ownership, caps TTL, stores immutable scopes, and appends an audit event before returning the opaque grant. Dream evidence uses `resource_type=dream_evidence`, `action=read`, and never inherits broad session scope.

- [ ] **Step 4: Add route and audit failure tests**

Run: `go test ./internal/tickets ./internal/app -run 'Grant|Ticket' -count=1`

Expected: PASS; policy unavailable, audit unavailable, expired session, wrong enterprise, wrong action, and expired grant all deny.

- [ ] **Step 5: Commit**

```bash
git add services/agentnexus/internal/tickets services/agentnexus/internal/app
git commit -m "feat: issue scoped Step Grants for Atlas operations"
```

### Task 6: Publish and Integration-Test the Gateway Contract

**Files:**
- Modify: `../agentnexus/services/agentnexus/api/openapi/gateway-runtime.yaml`
- Create: `../agentnexus/services/agentnexus/tests/e2e/browser_session_and_approval_test.go`
- Modify: `../agentnexus/services/agentnexus/Makefile`

**Interfaces:**
- Consumes: all services above.
- Produces: stable public contract and one real-PostgreSQL acceptance suite.

- [ ] **Step 1: Add an end-to-end test**

The test must execute: unknown browser → IdP redirect → callback → silent authorize → PKCE code exchange → `/me` → low-risk decision → high-risk upward route → Dream evidence Step Grant → logout → `/me` returns 401.

- [ ] **Step 2: Run it before wiring and verify failure**

Run: `go test ./tests/e2e -run BrowserSessionAndApproval -count=1`

Expected: FAIL until all routers and PostgreSQL stores are wired in `cmd/gateway-api`.

- [ ] **Step 3: Complete OpenAPI and command wiring**

Add `test-auth` to the Makefile:

```make
test-auth:
	go test ./internal/browserauth ./internal/policy ./internal/approval ./internal/tickets ./internal/app ./tests/e2e -count=1
```

- [ ] **Step 4: Run full AgentNexus verification**

Run: `make test-auth && make test && make build`

Expected: all packages PASS and all four binaries build.

- [ ] **Step 5: Commit**

```bash
git add services/agentnexus/api services/agentnexus/tests/e2e services/agentnexus/Makefile services/agentnexus/cmd
git commit -m "test: verify browser identity and governed approval flow"
```

### Task 7: Migrate the AgentNexus Console to the Family Package

**Files:**
- Modify: `../agentnexus/packages/enterprise-gateway-console/package.json`
- Modify: `../agentnexus/packages/enterprise-gateway-console/src/App.tsx`
- Modify: `../agentnexus/packages/enterprise-gateway-console/src/AgentNexusDashboard.tsx`
- Test: `../agentnexus/packages/enterprise-gateway-console/src/AgentNexusDashboard.test.tsx`
- Delete after verification: `../agentnexus/packages/claw-runtime-ui/`

**Interfaces:**
- Consumes: published `@xiaozhiclaw/runtime-ui` package.
- Produces: the existing gateway Console on the same canonical tokens/primitives, without a mirrored UI package.

- [ ] **Step 1: Add a failing dependency-boundary assertion**

```ts
expect(source("src/AgentNexusDashboard.tsx")).toContain("@xiaozhiclaw/runtime-ui");
expect(source("src/AgentNexusDashboard.tsx")).not.toContain("@agentnexus/claw-runtime-ui");
```

- [ ] **Step 2: Verify failure**

Run: `cd ../agentnexus && pnpm --filter @agentnexus/enterprise-gateway-console test`

Expected: FAIL on the old package import assertion.

- [ ] **Step 3: Migrate imports and styles**

Replace `@agentnexus/claw-runtime-ui` with the pinned `@xiaozhiclaw/runtime-ui` version, import its `styles.css` once, and map existing gateway business callbacks into shared controlled components. Do not move gateway API calls or session state into the shared package.

- [ ] **Step 4: Run the Console gate and remove the mirror**

Run: `pnpm --filter @agentnexus/enterprise-gateway-console test && pnpm --filter @agentnexus/enterprise-gateway-console build`

Expected: PASS. Then run `rg -n "@agentnexus/claw-runtime-ui" packages`; expected no matches before deleting `packages/claw-runtime-ui`.

- [ ] **Step 5: Commit**

```bash
git add packages/enterprise-gateway-console package.json pnpm-lock.yaml
git rm -r packages/claw-runtime-ui
git commit -m "refactor: adopt Xiaozhi family UI in AgentNexus"
```
