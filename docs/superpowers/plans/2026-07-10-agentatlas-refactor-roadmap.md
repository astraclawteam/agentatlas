# AgentAtlas Family Refactor Roadmap Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Deliver the approved AgentAtlas knowledge-maintenance experience, hierarchical Dream distillation, browser SSO, governed publishing, and Xiaozhi family UI without cross-repository source coupling.

**Architecture:** Four public deliverables ship behind published boundaries: `xiaozhiclaw-runtime` publishes UI, AgentNexus publishes identity/authorization contracts, AgentAtlas publishes knowledge/Dream APIs, and AgentAtlas Console consumes those boundaries. An enterprise-owned external Wave E may consume only released public schemas and images; its private implementation roadmap is intentionally outside this open-core repository. Cutover is contract-first and feature-flagged; no page mixes the old and new design systems.

**Tech Stack:** React, TypeScript, Vite, Vitest, Playwright, pnpm, Go, chi/net/http, PostgreSQL/sqlc, AgentNexus, FlowGram, OpenSearch, object storage

## Global Constraints

- Ordinary employees use Xiaozhi Claw only; AgentAtlas is a maintenance surface opened on demand by managers, operations staff, and service providers.
- Browser login is silent-session first and full-page OIDC Authorization Code + PKCE redirect when absent; no popup, password form, pasted ticket, or browser-stored bearer token.
- Low-risk changes require one authorized confirmation; high-risk changes require a second reviewer found upward through the organization graph; the requester cannot review their own change.
- Higher-level Dream runs consume lower-level already-sanitized Dream Summary by default; raw evidence drill-down requires a short-lived AgentNexus Step Grant.
- Dream run outputs are immutable; users may confirm, reject, annotate, mark incorrect, change future policy, or rerun.
- `@xiaozhiclaw/runtime-ui` is the only shared family UI package; consumers must not deep-import `xiaozhiclaw-runtime/ui-next/src`.
- Warm Paper background is `#F9F8F0`, ink is `#131314`, clay is `#D97757`, and Lucide React supplies product icons; emoji cannot be product icons.
- Basic mode exposes no internal ID, raw JSON, cron expression, index name, provider configuration, or English workflow node type.
- Every page has at most one primary action and states its purpose, current scope, current state, and next step.
- Agent drawer reflows desktop content and never covers it; narrow screens use a full-screen assistant route.
- Sensitive drafts are never persisted in browser `localStorage`; authorization and audit failures fail closed.
- Core tasks work at 1280×720, 200% zoom, keyboard-only operation, and WCAG AA text contrast.

---

## File Map

| Plan | Repository | Deliverable |
|---|---|---|
| `2026-07-10-xiaozhiclaw-runtime-ui-package.md` | `../xiaozhiclaw-runtime` | Publishable family UI package consumed first by Xiaozhi |
| `2026-07-10-agentnexus-browser-session-and-approval.md` | `../agentnexus` | Browser session, scoped permissions, Step Grants, and upward reviewer resolution |
| `2026-07-10-agentatlas-dream-and-governance-runtime.md` | `.` | Hierarchical Dream runtime plus governed drafts, review, and publish contracts |
| `2026-07-10-agentatlas-console-refactor.md` | `.` | Complete Console shell and work-surface refactor |

Wave E is owned outside open-core. This roadmap records only its compatibility gate: released Dream/governance schemas and immutable image versions must exist before any external consumer enables them.

The approved source of truth is `docs/superpowers/specs/2026-07-10-agentatlas-knowledge-maintenance-ux-design.md`.

## Specification Coverage

| Approved requirement | Implemented by |
|---|---|
| Employee/Xiaozhi/AgentNexus/AgentAtlas product boundary | Roadmap Tasks 1–3; Console Task 8 |
| A-based knowledge home, shortcuts, search, low-computer-skill copy | Console Tasks 1–2 |
| Structured knowledge/SOP editing, suggestions, truthful autosave | Console Task 3; Backend Task 8 |
| Low-risk single confirmation and high-risk upward dual review | AgentNexus Tasks 3–4; Backend Task 8; Console Task 3 |
| Silent session, full-page PKCE redirect, no pasted ticket | AgentNexus Tasks 1–2; Backend Task 8; Console Task 1 |
| Publishable Xiaozhi family tokens/primitives/chat package | Runtime UI Tasks 1–5; AgentNexus Task 7 |
| Icon-only top Agent button, horizontal alignment, docked/fullscreen assistant | Runtime UI Tasks 2–3; Console Tasks 1 and 6 |
| Existing workflow migration, dual editors, lossless FlowGram round-trip | Backend Tasks 1 and 8; Console Task 5 |
| Hierarchical Dream, child-summary-first, immutable outputs, trace-down grant | Backend Tasks 1–7; Console Task 4 |
| Answer evidence summary before graph and authorized detail | AgentNexus Task 5; Console Task 7 |
| External enterprise compatibility | Released public schema/image digests; implementation is enterprise-owned |
| Accessibility, novice usability, visual regression, real integrations | Roadmap Task 3; Console Task 8 |

### Task 1: Freeze Public Contracts Before UI Work

**Files:**
- Modify: `../agentnexus/services/agentnexus/api/openapi/gateway-runtime.yaml`
- Modify: `services/agentatlas/api/openapi/atlas-agent.yaml`
- Test: `services/agentatlas/internal/app/contract_drift_test.go`

**Interfaces:**
- Consumes: Approved product specification and existing `X-Nexus-Ticket` verification semantics.
- Produces: Versioned schemas for `BrowserSession`, `PermissionDecision`, `ApprovalRoute`, `DreamPolicyDefinition`, `DreamRunView`, `DreamSummaryView`, `ChangeDraft`, `RiskAssessment`, and `ReviewRoute` in the existing public OpenAPI documents.

- [ ] **Step 1: Write contract drift assertions**

```go
for _, token := range []string{
    "BrowserSession", "ApprovalRoute", "DreamRunView", "DreamSummaryView",
    "visibility_snapshot", "WorkflowRef", "idempotency_key",
} {
    if !bytes.Contains(openAPI, []byte(token)) { t.Fatalf("contract missing %s", token) }
}
```

- [ ] **Step 2: Run the contract test and verify it fails**

Run: `cd services/agentatlas && go test ./internal/app -run Contract -count=1`

Expected: FAIL because the new schema names are absent.

- [ ] **Step 3: Add the exact public schemas defined by the four focused plans**

The AgentAtlas OpenAPI document uses the canonical DTOs from `2026-07-10-agentatlas-dream-and-governance-runtime.md`: a nested `workflow: WorkflowRef` in public views, plus `org_unit_id`, `resource_type`, `resource_id`, `action`, `risk_level`, `requester_user_id`, `reviewer_user_id`, `policy_version`, `window_start`, `window_end`, typed `input_snapshot`/`visibility_snapshot`, `parent_run_ids`, and `idempotency_key`. Do not expose Go `internal/dream.Policy` or legacy parallel Dream DTOs as public contracts.

- [ ] **Step 4: Generate clients and rerun drift tests**

Run: `cd services/agentatlas && make generate && go test ./internal/app -run Contract -count=1`

Expected: PASS and no uncommitted generated drift after a second `make generate`.

- [ ] **Step 5: Commit the contract baseline in each owning repository**

```bash
git add services/agentnexus/api services/agentnexus/internal/app
git commit -m "feat: define browser authorization contracts"

git add services/agentatlas/api services/agentatlas/internal/app
git commit -m "feat: define Dream and governance contracts"
```

### Task 2: Execute Dependency Waves

**Files:**
- Reference: the four focused plans in this directory
- Create: `docs/operations/agentatlas-refactor-release-checklist.md`

**Interfaces:**
- Consumes: Public contracts from Task 1.
- Produces: An ordered release ledger with immutable package/API versions and rollback flags.

- [ ] **Step 1: Execute Wave A — shared UI**

Run every task in `2026-07-10-xiaozhiclaw-runtime-ui-package.md`. Record the package tarball name and integrity in the release checklist:

```markdown
| Boundary | Version | Artifact | Consumer gate |
| @xiaozhiclaw/runtime-ui | 0.1.0 | xiaozhiclaw-runtime-ui-0.1.0.tgz | Xiaozhi visual + unit suite pass |
```

- [ ] **Step 2: Execute Wave B — AgentNexus session and authorization**

Run every task in `2026-07-10-agentnexus-browser-session-and-approval.md`. Wave B may run after the OpenAPI baseline without waiting for Console.

- [ ] **Step 3: Execute Wave C — AgentAtlas backend**

Run every task in `2026-07-10-agentatlas-dream-and-governance-runtime.md`. It consumes the AgentNexus contract and must use a pinned test server, not a permissive mock, for integration acceptance.

- [ ] **Step 4: Execute Wave D — Console**

Run every task in `2026-07-10-agentatlas-console-refactor.md` after the UI package and backend contracts are published.

- [ ] **Step 5: Publish the external Wave E compatibility gate**

Record released Dream/governance schema digests and image versions for the enterprise-owned external Wave E. Open-core does not contain or execute its private implementation plan; external consumers must not import open-core `internal/*` packages.

- [ ] **Step 6: Commit the release checklist**

```bash
git add docs/operations/agentatlas-refactor-release-checklist.md
git commit -m "docs: record AgentAtlas refactor release boundaries"
```

### Task 3: Run Cross-Repository Acceptance and Cutover

**Files:**
- Create: `scripts/acceptance/agentatlas-refactor.ps1`
- Create: `packages/agentatlas-console/e2e/novice-maintenance.spec.ts`
- Create: `packages/agentatlas-console/e2e/dream-hierarchy.spec.ts`
- Modify: `docs/operations/agentatlas-refactor-release-checklist.md`

**Interfaces:**
- Consumes: Published UI package, AgentNexus session APIs, AgentAtlas governance/Dream APIs, refactored Console.
- Produces: One command that proves the family release is safe to expose.

- [ ] **Step 1: Write the failing Playwright cutover scenarios**

```ts
test("unauthenticated maintenance link returns to its original task", async ({ page }) => {
  await page.goto("/changes/change_high_1");
  await expect(page).toHaveURL(/agentnexus.*authorize/);
  await completeTestIdPLogin(page, "manager-a");
  await expect(page).toHaveURL(/changes\/change_high_1/);
  await expect(page.getByRole("button", { name: /提交给.*复核/ })).toBeVisible();
});

test("company Dream consumes sanitized department summaries", async ({ page }) => {
  await seedDreamHierarchy(page.request);
  await page.goto("/dream/runs/company-run-1");
  await expect(page.getByText("输入：3 个部门梦境摘要")).toBeVisible();
  await expect(page.getByText("员工原始简报")).not.toBeVisible();
});
```

- [ ] **Step 2: Verify the scenarios fail before all waves are integrated**

Run: `cd packages/agentatlas-console && pnpm exec playwright test e2e/novice-maintenance.spec.ts e2e/dream-hierarchy.spec.ts`

Expected: FAIL on missing redirect/session and Dream run detail behavior.

- [ ] **Step 3: Add the acceptance script**

```powershell
$ErrorActionPreference = 'Stop'
Push-Location ../xiaozhiclaw-runtime; pnpm --filter @xiaozhiclaw/runtime-ui test; Pop-Location
Push-Location ../agentnexus/services/agentnexus; go test ./...; Pop-Location
Push-Location services/agentatlas; go test ./...; Pop-Location
Push-Location .; pnpm --filter @agentatlas/console test; pnpm --filter @agentatlas/console build; Pop-Location
```

- [ ] **Step 4: Run full acceptance**

Run: `powershell -ExecutionPolicy Bypass -File scripts/acceptance/agentatlas-refactor.ps1`

Expected: all public repository suites exit 0; Playwright proves login return, organization scope, low/high-risk paths, Dream hierarchy, evidence authorization, agent drawer reflow, 1280×720, and 200% zoom. External Wave E compatibility is established by published schema/image digests, not private tests in this repository.

- [ ] **Step 5: Enable cutover flags in order**

Set `ATLAS_BROWSER_SESSION_V1=true`, then `ATLAS_GOVERNED_PUBLISH_V1=true`, then `ATLAS_DREAM_HIERARCHY_V1=true`, and finally `VITE_ATLAS_CONSOLE_V2=true`. Keep the previous Console route available for one release only; do not render old and new shells together.

- [ ] **Step 6: Remove compatibility code after one stable release**

Delete `packages/claw-runtime-ui`, ticket input/localStorage code, the old `AtlasAgentPanel`, direct workflow publish UI, and old Dream cron form only after telemetry shows no fallback traffic and all acceptance tests pass with flags permanently on.

- [ ] **Step 7: Commit the cutover harness**

```bash
git add scripts/acceptance packages/agentatlas-console/e2e docs/operations/agentatlas-refactor-release-checklist.md
git commit -m "test: add AgentAtlas refactor cutover acceptance"
```
