# AgentAtlas Enterprise Pack Migration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move enterprise Dream/governance packs and private deployment onto the released hierarchical Dream and authorization contracts without importing AgentAtlas internal code.

**Architecture:** The enterprise repository contains declarative packs, commercial extensions, license/deployment automation, and customer templates only. Validation pins public JSON Schema/OpenAPI/OCI versions from open-core releases; imports go through public draft/review APIs and never compile against `services/agentatlas/internal/*`.

**Tech Stack:** JSON Schema, OpenAPI, Node.js validation scripts, OCI images, Helm/private deployment manifests, Markdown runbooks

## Global Constraints

- Never import `agentatlas/services/agentatlas/internal/*`; depend only on public SDKs, OpenAPI/Proto/JSON Schema, released images, and Helm charts.
- Keep customer data, customer names, private endpoints, credentials, keys, exported production data, and commercial roadmap content out of all committed examples.
- Higher-level Dream policy packs default to `child_dream_summary`; raw employee evidence access is never enabled by a generic pack.
- Enterprise pack import creates a draft and follows risk review/publish; it never calls a create-and-publish shortcut.
- Policy packs pin a public schema version and a published Dream workflow reference.
- Private deployment pins exact compatible versions of AgentNexus, AgentAtlas, Console, and `@xiaozhiclaw/runtime-ui` assets.
- Company-level outputs default to sanitized manager/company visibility and require explicit review for visibility/masking/workflow changes.

---

## File Map

- Replace the README-only Dream pack description with versioned example manifests and validation.
- Update knowledge-governance packs to the exact permission vocabulary and risk routes.
- Add a public-contract compatibility matrix and private-deployment preflight.
- Update product/technical docs to describe hierarchical Dream and public SDK ownership.

### Task 1: Validate Enterprise Packs Against Released Public Schemas

**Files:**
- Create: `../agentatlas-enterprise/package.json`
- Create: `../agentatlas-enterprise/scripts/validate-packs.mjs`
- Create: `../agentatlas-enterprise/tests/pack-boundary.test.mjs`
- Create: `../agentatlas-enterprise/vendor/contracts/agentatlas-0.2.0/manifest.json`
- Create: `../agentatlas-enterprise/vendor/contracts/agentatlas-0.2.0/dream.schema.json`
- Create: `../agentatlas-enterprise/vendor/contracts/agentatlas-0.2.0/workflow.schema.json`
- Create: `../agentatlas-enterprise/vendor/contracts/agentatlas-0.2.0/governance.schema.json`
- Modify: `../agentatlas-enterprise/README.md`

**Interfaces:**
- Consumes: copied/released schema artifacts identified by version and SHA-256, never an open-core source path.
- Produces: `npm test` boundary scan and `npm run validate:packs` schema validation.

- [ ] **Step 1: Write the failing internal-import boundary test**

```js
test("enterprise files never depend on open-core internal packages", async () => {
  const matches = await scanRepo({
    roots: ["README.md", "dream-policy-packs", "knowledge-governance-packs", "industry-sop-packs", "advanced-parsers", "customer-templates", "private-deploy"],
    forbidden: [/services\/agentatlas\/internal\//, /internal\/dream\.Policy/],
  });
  assert.deepEqual(matches, []);
});
```

- [ ] **Step 2: Run and verify failure**

Run: `cd ../agentatlas-enterprise && node --test tests/pack-boundary.test.mjs`

Expected: FAIL because the scanner does not exist; after implementation it must also flag the current README reference to `internal/dream.Policy`.

- [ ] **Step 3: Add validation tooling**

```json
{
  "name": "agentatlas-enterprise-packs",
  "private": true,
  "type": "module",
  "scripts": { "test": "node --test tests/*.test.mjs", "validate:packs": "node scripts/validate-packs.mjs" },
  "devDependencies": { "ajv": "^8.17.1", "ajv-formats": "^3.0.1" }
}
```

`validate-packs.mjs` loads the AgentAtlas `0.2.0` release schemas from `vendor/contracts/agentatlas-0.2.0/`, validates every JSON file in pack directories, and verifies each file SHA-256 against `manifest.json` before validation. Copy these artifacts from the signed open-core `agentatlas-contracts-0.2.0` release asset, not from `services/agentatlas/internal`.

- [ ] **Step 4: Replace the internal reference and run tests**

The README must say packs align to the released `dream/dream.schema.json` contract and import through AgentAtlas public APIs.

Run: `npm test && npm run validate:packs`

Expected: boundary test PASS; validation may report no packs until Task 2 creates them, but it exits 0 only for an empty allowed set or valid JSON.

- [ ] **Step 5: Commit**

```bash
git add package.json package-lock.json scripts tests README.md
git commit -m "test: enforce enterprise public-contract boundary"
```

### Task 2: Migrate Hierarchical Dream Policy and Workflow Packs

**Files:**
- Modify: `../agentatlas-enterprise/dream-policy-packs/README.md`
- Create: `../agentatlas-enterprise/dream-policy-packs/baseline/department-nightly.json`
- Create: `../agentatlas-enterprise/dream-policy-packs/baseline/company-weekly.json`
- Create: `../agentatlas-enterprise/dream-policy-packs/workflows/bottom-up-distillation.json`
- Create: `../agentatlas-enterprise/tests/dream-pack-semantics.test.mjs`

**Interfaces:**
- Consumes: public Dream Policy and AtlasWorkflow schemas.
- Produces: safe department/company baselines with pinned workflow refs and child-summary-first semantics.

- [ ] **Step 1: Write semantic tests before pack files**

```js
for (const pack of companyPacks) {
  assert.deepEqual(pack.input_sources, ["child_dream_summary"]);
  assert.equal(pack.visibility_level, "company_sanitized");
  assert.equal(pack.raw_evidence_access, "step_grant_only");
  assert.ok(pack.workflow.id && pack.workflow.version > 0);
}
```

- [ ] **Step 2: Verify failure**

Run: `node --test tests/dream-pack-semantics.test.mjs`

Expected: FAIL because baseline pack files are absent.

- [ ] **Step 3: Add the department and company packs**

Department pack inputs `work_brief`, `project_record`, `sop_update`, `completed_task`, and `risk_event`; company pack inputs only `child_dream_summary`. Both specify `timezone`, human-friendly schedule preset plus compiled schedule, output space selector, masking profile reference, confirmation mode, evidence retention, `max_attempts`, and published workflow `{id,version}`.

- [ ] **Step 4: Add the public Dream workflow pack**

The workflow contains input, sanitize/visibility, `dream.aggregate`, optional `human.confirm`, and `trace.append` nodes. It uses the public `AtlasWorkflow` shape and carries no endpoint, tenant, credential, customer name, or raw evidence.

- [ ] **Step 5: Validate and commit**

Run: `npm test && npm run validate:packs`

Expected: all JSON schema and semantic tests PASS.

```bash
git add dream-policy-packs tests/dream-pack-semantics.test.mjs
git commit -m "feat: add hierarchical Dream policy packs"
```

### Task 3: Align Knowledge Governance Packs to Scoped Permissions

**Files:**
- Modify: `../agentatlas-enterprise/knowledge-governance-packs/README.md`
- Create: `../agentatlas-enterprise/knowledge-governance-packs/baseline-permissions.json`
- Create: `../agentatlas-enterprise/knowledge-governance-packs/baseline-risk-rules.json`
- Test: `../agentatlas-enterprise/tests/governance-pack-semantics.test.mjs`

**Interfaces:**
- Consumes: AgentNexus permission/approval public schema.
- Produces: scoped enterprise roles and deterministic risk minima compatible with AgentAtlas review UI.

- [ ] **Step 1: Write the permission/risk semantic test**

```js
assert.deepEqual(new Set(pack.permissions), new Set(["suggest","edit","publish_low_risk","approve_high_risk","workflow_edit","workflow_advanced","service_mode"]));
assert.equal(rules.high_risk.self_review, "deny");
assert.equal(rules.high_risk.reviewer_route, "organization_upward");
assert.equal(rules.high_risk.no_reviewer, "enterprise_knowledge_admin_queue");
```

- [ ] **Step 2: Verify failure**

Run: `node --test tests/governance-pack-semantics.test.mjs`

Expected: FAIL because the baseline packs are absent.

- [ ] **Step 3: Add organization-scoped permission bindings**

Every binding contains role, permission, organization selector, resource types, and allowed actions. Service-provider bindings require explicit customer org scope and never imply cross-enterprise access.

- [ ] **Step 4: Add deterministic risk rules**

Published workflow/SOP behavior, permission/approval, execution deadline, evidence requirement, external side effect, visibility/masking, and Dream workflow binding changes have minimum high risk. Content-only corrections may be low risk subject to configured impact thresholds. No rule permits user/Agent risk downgrades.

- [ ] **Step 5: Validate and commit**

Run: `npm test && npm run validate:packs`

Expected: PASS for permission vocabulary, upward route, self-review denial, fallback queue, and risk minima.

```bash
git add knowledge-governance-packs tests/governance-pack-semantics.test.mjs
git commit -m "feat: align enterprise governance packs"
```

### Task 4: Add Private-Deployment Compatibility and Upgrade Gates

**Files:**
- Modify: `../agentatlas-enterprise/private-deploy/README.md`
- Create: `../agentatlas-enterprise/private-deploy/compatibility.json`
- Create: `../agentatlas-enterprise/private-deploy/preflight.mjs`
- Modify: `../agentatlas-enterprise/docs/superpowers/specs/2026-07-06-agentatlas-product-design.md`
- Modify: `../agentatlas-enterprise/docs/superpowers/specs/2026-07-06-agentatlas-technical-architecture.md`
- Test: `../agentatlas-enterprise/tests/private-deploy-preflight.test.mjs`

**Interfaces:**
- Consumes: released AgentNexus/AgentAtlas image versions, public schema hashes, Console asset version, and pack version.
- Produces: a preflight that blocks incompatible offline deployment/upgrade before data migration.

- [ ] **Step 1: Write the compatibility failure test**

```js
assert.throws(() => preflight({ agentnexus:"1.4.0", agentatlas:"2.0.0", console:"1.0.0", contract:"dream-v2" }), /incompatible/);
assert.doesNotThrow(() => preflight(knownGoodRelease));
```

- [ ] **Step 2: Verify failure**

Run: `node --test tests/private-deploy-preflight.test.mjs`

Expected: FAIL because compatibility/preflight files are absent.

- [ ] **Step 3: Add exact compatibility data and preflight checks**

`compatibility.json` records exact release/image digests, minimum PostgreSQL/OpenSearch versions from the released deployment contract, public schema SHA-256 values, required feature flags, and migration order. `preflight.mjs` rejects absent digests, schema mismatch, unsupported downgrade, and enabling Console V2 before Browser Session/Governance/Dream APIs.

- [ ] **Step 4: Correct the enterprise design documents**

Document employee → Xiaozhi Claw → AgentNexus → AgentAtlas, direct AgentAtlas maintenance only on demand, hierarchical Dream inputs/outputs, child-summary-first rollup, evidence Step Grants, immutable Dream history, four Dream views, and public-contract-only enterprise extension boundaries.

- [ ] **Step 5: Run all enterprise gates and commit**

Run: `npm test && npm run validate:packs && node private-deploy/preflight.mjs --compat private-deploy/compatibility.json`

Expected: PASS with placeholder/non-customer release data; repository scan finds no secrets, customer identifiers, private endpoints, or open-core internal imports.

```bash
git add private-deploy docs/superpowers/specs tests/private-deploy-preflight.test.mjs
git commit -m "feat: gate enterprise deployment on public contracts"
```
