# AgentAtlas Console Refactor Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the existing ticket-driven five-panel Console with the approved low-computer-skill maintenance workspace, enterprise Dream views, governed editing/review, dual workflow editors, and docked Atlas assistant.

**Architecture:** A route-based Console shell loads one BFF session and organization scope, then renders task-focused pages through typed API hooks. Shared visual/chat primitives come only from `@xiaozhiclaw/runtime-ui`; AgentAtlas owns adapters, domain views, routing, permissions, data fetching, and error state.

**Tech Stack:** React, TypeScript, Vite, Vitest, Testing Library, Playwright, `@xiaozhiclaw/runtime-ui`, Lucide React, FlowGram, React Flow

## Global Constraints

- Console never asks for or stores `X-Nexus-Ticket`; `localStorage` cannot hold identity tokens or sensitive drafts.
- Top navigation is `企业知识`, `企业梦境`, `做事流程`, and `回答依据`.
- Desktop header places icon-only Agent button immediately left of the no-wrap user group; all controls share one height and vertical center.
- Agent drawer docks on the right and reflows content; it never overlays or hides the active page. Narrow screens use `/assistant` full screen.
- Basic mode shows no internal IDs, raw JSON, cron, provider/index names, or English node types.
- Service providers and specifically authorized users may enable Advanced Maintenance mode; it never broadens enterprise/org authorization.
- Every page states purpose, scope, state, and next step; at most one primary button is visually dominant.
- Warm Paper `#F9F8F0`, ink `#131314`, clay `#D97757`, glass materials, family typography, and Lucide icons are mandatory; emoji icons are forbidden.
- Historical Dream outputs and published knowledge/workflow versions are read-only.
- Failed saves are truthful; optimistic conflicts show a diff; publish is idempotent; expired session performs full-page login and restores the route.
- Core tasks remain usable at 1280×720, 200% zoom, keyboard-only operation, and WCAG AA contrast.

---

## File Map

- Replace the monolithic `AgentAtlasDashboard.tsx` with `app/ConsoleShell.tsx`, `app/routes.tsx`, `app/session.tsx`, and page folders.
- Replace `api.ts` ticket logic with `api/client.ts`, typed resource modules, and `credentials: include`.
- Use `features/assistant/AtlasAssistantDrawer.tsx` as an adapter over shared chat primitives; delete `AtlasAgentPanel.tsx` after cutover.
- Split Dream into overview, timeline, workflow, run-detail, and policy wizard files.
- Split workflow into list, structured SOP editor, advanced FlowGram studio, and lossless adapters.

### Task 1: Install the Shared Package and Build the Authenticated Shell

**Files:**
- Modify: `packages/agentatlas-console/package.json`
- Modify: `packages/agentatlas-console/src/main.tsx`
- Create: `packages/agentatlas-console/src/app/ConsoleShell.tsx`
- Create: `packages/agentatlas-console/src/app/session.tsx`
- Create: `packages/agentatlas-console/src/app/routes.tsx`
- Create: `packages/agentatlas-console/src/api/client.ts`
- Test: `packages/agentatlas-console/src/app/ConsoleShell.test.tsx`

**Interfaces:**
- Consumes: `GET /api/session`, `/auth/login`, shared `Button`, `DockedPanel`, and styles.
- Produces: `SessionProvider`, four-surface shell, organization scope navigation, advanced-mode gate, and route restoration.

- [ ] **Step 1: Write failing shell tests**

```tsx
it("redirects the full page when session is absent", async () => {
  server.use(http.get("/api/session", () => new HttpResponse(null, { status: 401 })));
  render(<ConsoleShell initialPath="/knowledge/dept-rd" />);
  await waitFor(() => expect(assignLocation).toHaveBeenCalledWith("/auth/login?return_to=%2Fknowledge%2Fdept-rd"));
});
it("keeps Agent and user controls horizontally aligned", async () => {
  renderWithSession(<ConsoleShell />, managerSession);
  expect(screen.getByTestId("header-user-actions")).toHaveClass("items-center", "whitespace-nowrap");
});
```

- [ ] **Step 2: Verify failure**

Run: `cd . && pnpm --filter @agentatlas/console test -- ConsoleShell.test.tsx`

Expected: FAIL because the new shell/session do not exist.

- [ ] **Step 3: Replace package and style dependencies**

Remove `@agentatlas/claw-runtime-ui`, add `@xiaozhiclaw/runtime-ui@0.1.0`, `lucide-react@^0.511.0`, and `react-router-dom@^7.6.2`, then import `@xiaozhiclaw/runtime-ui/styles.css` once from `main.tsx`.

- [ ] **Step 4: Implement credentialed API and session state**

```ts
export async function api<T>(path: string, init: RequestInit = {}): Promise<T> {
  const response = await fetch(path, { ...init, credentials: "include", headers: { "Content-Type":"application/json", ...init.headers } });
  if (response.status === 401) { location.assign(`/auth/login?return_to=${encodeURIComponent(location.pathname + location.search)}`); throw new SessionRedirect(); }
  if (!response.ok) throw await ApiError.from(response);
  return response.json() as Promise<T>;
}
```

`Session` contains display name, enterprise name, authorized org tree/scopes, permissions, and `advanced_mode_allowed`; it contains no raw ticket/token.

- [ ] **Step 5: Implement shell and route boundaries**

Use a 48px no-wrap header action group. Default route is `/knowledge`. Routes are `/knowledge/*`, `/dream/*`, `/workflows/*`, `/evidence/*`, and `/assistant`. Advanced mode lives in session memory, resets on reload, and only renders its toggle when allowed.

- [ ] **Step 6: Run tests/build and commit**

Run: `pnpm --filter @agentatlas/console test && pnpm --filter @agentatlas/console build`

Expected: PASS and build has no `@agentatlas/claw-runtime-ui` import.

```bash
git add packages/agentatlas-console package.json pnpm-lock.yaml
git commit -m "refactor: add authenticated AgentAtlas family shell"
```

### Task 2: Build the Organization Knowledge Home

**Files:**
- Create: `packages/agentatlas-console/src/features/knowledge/KnowledgeHome.tsx`
- Create: `packages/agentatlas-console/src/features/knowledge/OrgScopeNav.tsx`
- Create: `packages/agentatlas-console/src/features/knowledge/KnowledgeList.tsx`
- Create: `packages/agentatlas-console/src/features/knowledge/KnowledgeSearch.tsx`
- Modify: `packages/agentatlas-console/src/KnowledgeMap.tsx`
- Test: `packages/agentatlas-console/src/features/knowledge/KnowledgeHome.test.tsx`

**Interfaces:**
- Consumes: authorized org tree, knowledge list/search APIs, counts for recent changes/reviews.
- Produces: A-based home with B shortcuts and C search; `KnowledgeMap` remains an optional read-only view.

- [ ] **Step 1: Write novice-oriented tests**

```tsx
expect(screen.getByRole("heading", { name: "研发一部的企业知识" })).toBeVisible();
expect(screen.getByText("这些知识会通过企业网关提供给获得授权的员工 Agent")).toBeVisible();
expect(screen.getAllByRole("button").filter(isPrimary)).toHaveLength(1);
expect(screen.queryByPlaceholderText(/票据|token|ID/)).not.toBeInTheDocument();
```

- [ ] **Step 2: Verify failure**

Run: `pnpm --filter @agentatlas/console test -- KnowledgeHome.test.tsx`

Expected: FAIL because current dashboard exposes five technical panels and ticket input.

- [ ] **Step 3: Implement the page composition**

Left rail shows only authorized organization nodes, `最近修改`, and `需要我审核`. Main area shows purpose copy, running/freshness status, four task cards (`添加资料`, `新建或修改知识`, `制作 SOP 流程`, `处理建议与审核`), then a searchable content list. Search is a toolbar action, not a permanent header field.

- [ ] **Step 4: Use professional icons and one active color**

Use `Building2`, `Upload`, `FilePenLine`, `Workflow`, `ClipboardCheck`, `Search`, `Clock3`, and `ChevronRight` from Lucide. Do not use emoji or hard-coded blue/green/red.

- [ ] **Step 5: Verify behavior and commit**

Run: `pnpm --filter @agentatlas/console test -- KnowledgeHome.test.tsx && pnpm --filter @agentatlas/console build`

Expected: PASS for scope switch, search expansion, empty/loading/error states, and one-primary-action rule.

```bash
git add packages/agentatlas-console/src/features/knowledge packages/agentatlas-console/src/KnowledgeMap.tsx
git commit -m "feat: build organization knowledge workspace"
```

### Task 3: Build Structured Knowledge Editing and Governed Review

**Files:**
- Create: `packages/agentatlas-console/src/features/knowledge/KnowledgeEditor.tsx`
- Create: `packages/agentatlas-console/src/features/knowledge/SOPStepsEditor.tsx`
- Create: `packages/agentatlas-console/src/features/governance/ChangeReviewPage.tsx`
- Create: `packages/agentatlas-console/src/features/governance/ChangeDiff.tsx`
- Create: `packages/agentatlas-console/src/api/changes.ts`
- Test: `packages/agentatlas-console/src/features/governance/ChangeReviewPage.test.tsx`

**Interfaces:**
- Consumes: suggestion/draft/update/diff/assess/submit/decision/publish APIs.
- Produces: truthful autosave, conflict resolution, low-risk single confirm, high-risk upward review, and suggestion-only fallback.

- [ ] **Step 1: Write failing state/action tests**

```tsx
it.each([
  ["low", "确认并发布"],
  ["high", "提交给王主任复核"],
  ["suggestion_only", "提交纠错建议"],
])("shows one action for %s", async (mode, label) => {
  renderReview({ mode });
  expect(screen.getByRole("button", { name: label })).toBeVisible();
  expect(primaryButtons()).toHaveLength(1);
});
```

- [ ] **Step 2: Verify failure**

Run: `pnpm --filter @agentatlas/console test -- ChangeReviewPage.test.tsx`

Expected: FAIL because review/edit pages are absent.

- [ ] **Step 3: Implement the structured editor**

Layout: outline on left; readable/editable content and numbered SOP steps in the center; organization scope, impact, references, and advanced entry on the right. Debounce autosave to 800ms and render only truthful states `正在保存`, `已保存 HH:mm`, `尚未保存`, or `保存失败，重试`.

Do not write drafts to `localStorage`. On unload with failed/pending save, show a native unsaved-change warning.

- [ ] **Step 4: Implement review and concurrency behavior**

The review page presents before/after, deterministic risk reasons, impacted organizations/people/Agent answers/SOPs, and next reviewer path. A 409 response renders the user's draft beside the latest server version and offers `重新应用我的修改`; it never silently overwrites.

- [ ] **Step 5: Run tests and commit**

Run: `pnpm --filter @agentatlas/console test -- KnowledgeEditor ChangeReviewPage && pnpm --filter @agentatlas/console build`

Expected: PASS for autosave failure, concurrent update, suggestion fallback, self-review absence, idempotent publish retry, and accurate state copy.

```bash
git add packages/agentatlas-console/src/features/knowledge packages/agentatlas-console/src/features/governance packages/agentatlas-console/src/api/changes.ts
git commit -m "feat: add governed knowledge editing and review"
```

### Task 4: Build the Four Enterprise Dream Views

**Files:**
- Create: `packages/agentatlas-console/src/features/dream/DreamOverviewPage.tsx`
- Rewrite: `packages/agentatlas-console/src/DreamTimeline.tsx`
- Rewrite: `packages/agentatlas-console/src/DreamPolicyPanel.tsx`
- Create: `packages/agentatlas-console/src/features/dream/DreamRunDetailPage.tsx`
- Create: `packages/agentatlas-console/src/features/dream/DreamPolicyWizard.tsx`
- Create: `packages/agentatlas-console/src/api/dream.ts`
- Test: `packages/agentatlas-console/src/features/dream/DreamPages.test.tsx`

**Interfaces:**
- Consumes: Dream overview/run/policy/annotation/rerun/evidence APIs.
- Produces: 梦境全景, 梦境时间线, 梦境工作流, and 运行与追溯.

- [ ] **Step 1: Write failing hierarchy and immutability tests**

```tsx
render(<DreamOverviewPage data={hierarchyFixture} />);
expect(screen.getByText("公司")).toBeVisible();
expect(screen.getByText("研发事业部")).toBeVisible();
expect(screen.getByText("覆盖 2/3 个下级组织")).toBeVisible();
render(<DreamRunDetailPage run={immutableRun} />);
expect(screen.queryByRole("button", { name: /编辑摘要/ })).not.toBeInTheDocument();
expect(screen.getByRole("button", { name: /标记结果有误/ })).toBeVisible();
```

- [ ] **Step 2: Verify failure**

Run: `pnpm --filter @agentatlas/console test -- DreamPages.test.tsx`

Expected: FAIL because current Dream UI is a flat timeline plus raw cron/regex form.

- [ ] **Step 3: Implement Dream overview and timeline**

Overview is a hierarchy/tree, not a node canvas. Each org row shows last run, freshness, coverage, missing input, risk count, and plain status. Timeline filters by org level/window and renders summary, trends, risks, todos, and confirmation state.

- [ ] **Step 4: Implement the policy wizard and advanced bridge**

Basic steps ask: `整理哪个组织`, `多久整理一次` using choices like 每晚/每周, `使用哪些记录`, `谁能看到`, and `是否需要确认`. Convert choices to public policy fields inside the API adapter. Only Advanced mode shows timezone/cron/masking rules, diagnostics, and `AdvancedWorkflowStudio` for the pinned Dream workflow.

- [ ] **Step 5: Implement run detail and evidence access**

Show input child organizations, policy/workflow/model versions as human labels, coverage, missing inputs, structured output, failure step, annotations, lineage, and rerun relationship. `查看原始依据` first requests a Step Grant; denial explains who can authorize it and never displays a raw pointer ID in basic mode.

- [ ] **Step 6: Run tests and commit**

Run: `pnpm --filter @agentatlas/console test -- Dream && pnpm --filter @agentatlas/console build`

Expected: PASS for partial coverage, immutable outputs, annotation/rerun, evidence denial/allow, basic/advanced separation, and no cron in basic mode.

```bash
git add packages/agentatlas-console/src/features/dream packages/agentatlas-console/src/DreamTimeline.tsx packages/agentatlas-console/src/DreamPolicyPanel.tsx packages/agentatlas-console/src/api/dream.ts
git commit -m "feat: build hierarchical enterprise Dream workspace"
```

### Task 5: Split Workflow Editing and Make FlowGram Round-Trip Lossless

**Files:**
- Replace: `packages/agentatlas-console/src/WorkflowStudio.tsx`
- Create: `packages/agentatlas-console/src/features/workflows/WorkflowListPage.tsx`
- Create: `packages/agentatlas-console/src/features/workflows/SOPStructuredEditor.tsx`
- Create: `packages/agentatlas-console/src/features/workflows/AdvancedWorkflowStudio.tsx`
- Create: `packages/agentatlas-console/src/features/workflows/flowgram-adapter.ts`
- Modify: `packages/agentatlas-console/src/AtlasWorkflowCanvas.tsx`
- Test: `packages/agentatlas-console/src/features/workflows/flowgram-adapter.test.ts`

**Interfaces:**
- Consumes: canonical `AtlasWorkflow`, draft/list/get/update/diff/governed-publish APIs.
- Produces: basic linear editor, read-only summary for advanced graphs, permission-gated FlowGram editor, and exact `AtlasWorkflow ↔ WorkflowJSON` conversion.

- [ ] **Step 1: Write the round-trip test before changing conversion**

```ts
const original = advancedWorkflowFixture();
expect(fromWorkflowJSON(toWorkflowJSON(original), original)).toEqual(original);
expect(fromWorkflowJSON(toWorkflowJSON(original), original).edges[0].condition).toBe("risk == 'high'");
```

The fixture includes node config, edge conditions, confirmation, risk, variables, and input/output schemas.

- [ ] **Step 2: Verify failure**

Run: `pnpm --filter @agentatlas/console test -- flowgram-adapter.test.ts`

Expected: FAIL because current conversion drops config/condition/schema and the canvas export is not wired to Studio state.

- [ ] **Step 3: Implement lossless adapters**

Store original Atlas fields in namespaced FlowGram node/edge data and reconstruct them explicitly. Unknown node types survive round-trip. Never infer a linear order from a branched graph for editing.

- [ ] **Step 4: Implement dual editor rules**

Linear SOPs may be reordered/edited in `SOPStructuredEditor`. Workflows with branches, edge conditions, or unsupported nodes show a read-only step summary and `使用高级维护模式打开` only when `workflow_advanced` is allowed. Published versions create a new draft; save updates that draft with revision instead of creating a new workflow every time.

- [ ] **Step 5: Wire FlowGram export and governed publish**

Pass `onExport={(json) => setDraft(fromWorkflowJSON(json, draft))}` to `AtlasWorkflowCanvas`. Remove the direct Publish button; use `下一步：检查并发布`, routing to the shared review page.

- [ ] **Step 6: Run tests and commit**

Run: `pnpm --filter @agentatlas/console test -- Workflow AtlasWorkflowCanvas flowgram-adapter && pnpm --filter @agentatlas/console build`

Expected: PASS for lossless round-trip, advanced read-only protection, draft update, optimistic conflict, and governed publish.

```bash
git add packages/agentatlas-console/src/WorkflowStudio.tsx packages/agentatlas-console/src/AtlasWorkflowCanvas.tsx packages/agentatlas-console/src/features/workflows
git commit -m "refactor: split basic and advanced workflow editing"
```

### Task 6: Replace the Assistant with a Shared Docked Chat Adapter

**Files:**
- Create: `packages/agentatlas-console/src/features/assistant/AtlasAssistantDrawer.tsx`
- Create: `packages/agentatlas-console/src/features/assistant/useAtlasChat.ts`
- Modify: `packages/agentatlas-console/src/app/ConsoleShell.tsx`
- Delete after cutover: `packages/agentatlas-console/src/AtlasAgentPanel.tsx`
- Test: `packages/agentatlas-console/src/features/assistant/AtlasAssistantDrawer.test.tsx`

**Interfaces:**
- Consumes: shared `DockedPanel`, `AgentChatShell`, `PromptInput`, message/attachment/approval primitives and AgentAtlas chat/upload APIs.
- Produces: top Agent button, contextual docked drawer, and `/assistant` narrow-screen route.

- [ ] **Step 1: Write reflow and controlled-chat tests**

```tsx
await user.click(screen.getByRole("button", { name: "打开 Atlas 助手" }));
expect(screen.getByTestId("console-content")).toHaveAttribute("data-assistant", "open");
expect(screen.getByRole("complementary", { name: "Atlas 助手" })).toBeVisible();
expect(screen.getByRole("main")).not.toHaveAttribute("aria-hidden", "true");
```

- [ ] **Step 2: Verify failure**

Run: `pnpm --filter @agentatlas/console test -- AtlasAssistantDrawer.test.tsx`

Expected: FAIL because current assistant is permanently visible and uses the mirrored package.

- [ ] **Step 3: Implement the AgentAtlas adapter**

`useAtlasChat` owns AgentAtlas run ID/messages/upload state and maps backend confirmations to shared approval-card props. It passes current org, route, and edited object as context, but shared components never import AgentAtlas APIs.

- [ ] **Step 4: Implement responsive layout**

At desktop width, `ConsoleShell` uses a two-column CSS grid (`minmax(0,1fr) 380px`) only while open. Under the selected breakpoint, clicking the header icon navigates to `/assistant?return_to=...`; returning restores the prior route/draft context.

- [ ] **Step 5: Run tests and commit**

Run: `pnpm --filter @agentatlas/console test -- AtlasAssistantDrawer && pnpm --filter @agentatlas/console build`

Expected: PASS for open/close/focus return, horizontal header alignment, reflow, narrow-screen route, upload partial failure, and session expiry.

```bash
git add packages/agentatlas-console/src/features/assistant packages/agentatlas-console/src/app/ConsoleShell.tsx
git rm packages/agentatlas-console/src/AtlasAgentPanel.tsx
git commit -m "refactor: use shared docked Atlas assistant"
```

### Task 7: Refactor Answer Evidence and Operational States

**Files:**
- Modify: `packages/agentatlas-console/src/AnswerTraceGraph.tsx`
- Create: `packages/agentatlas-console/src/features/evidence/AnswerEvidencePage.tsx`
- Create: `packages/agentatlas-console/src/components/AsyncState.tsx`
- Create: `packages/agentatlas-console/src/components/StatusBadge.tsx`
- Test: `packages/agentatlas-console/src/features/evidence/AnswerEvidencePage.test.tsx`

**Interfaces:**
- Consumes: sanitized trace summaries/source lists and granted evidence detail.
- Produces: human summary first, source list second, optional read-only graph third; shared loading/empty/error/access states.

- [ ] **Step 1: Write progressive-disclosure tests**

```tsx
render(<AnswerEvidencePage trace={traceFixture} />);
expect(screen.getByText("这条回答主要依据 3 份研发资料和 1 条已发布 SOP")).toBeVisible();
expect(screen.queryByTestId("answer-trace-graph")).not.toBeInTheDocument();
await user.click(screen.getByRole("button", { name: "查看证据关系图" }));
expect(screen.getByTestId("answer-trace-graph")).toBeVisible();
```

- [ ] **Step 2: Verify failure**

Run: `pnpm --filter @agentatlas/console test -- AnswerEvidencePage.test.tsx`

Expected: FAIL because the graph is currently the primary presentation.

- [ ] **Step 3: Implement shared operational states**

`AsyncState` has explicit `loading`, `empty`, `error`, and `forbidden` variants. Error copy states what happened, whether work was saved, and a valid next action. Empty state contains one next-step link, not only `暂无数据`.

- [ ] **Step 4: Implement evidence disclosure**

Render names, source types, timestamps, and plain-language use first. Hide pointer/grant/model-route IDs in basic mode. Advanced mode may reveal diagnostic metadata. Evidence detail requests a Step Grant and handles denial without losing the summary page.

- [ ] **Step 5: Run tests and commit**

Run: `pnpm --filter @agentatlas/console test -- AnswerEvidence AsyncState && pnpm --filter @agentatlas/console build`

Expected: PASS for summary-first presentation, optional graph, access denial, no raw IDs in basic mode, and keyboard graph disclosure.

```bash
git add packages/agentatlas-console/src/AnswerTraceGraph.tsx packages/agentatlas-console/src/features/evidence packages/agentatlas-console/src/components
git commit -m "refactor: explain answer evidence before graph detail"
```

### Task 8: Visual, Accessibility, Novice-Use, and Legacy Cutover Gate

**Files:**
- Create: `packages/agentatlas-console/playwright.config.ts`
- Create: `packages/agentatlas-console/e2e/novice-maintenance.spec.ts`
- Create: `packages/agentatlas-console/e2e/dream-hierarchy.spec.ts`
- Create: `packages/agentatlas-console/e2e/accessibility-layout.spec.ts`
- Modify: `packages/agentatlas-console/package.json`
- Delete after pass: `packages/claw-runtime-ui/`
- Delete/replace after pass: `packages/agentatlas-console/src/AgentAtlasDashboard.tsx`

**Interfaces:**
- Consumes: all refactored pages and real local AgentNexus/AgentAtlas stack.
- Produces: cutover proof and removal of duplicated UI/ticket code.

- [ ] **Step 1: Encode the six zero-training journeys**

Playwright covers: find 研发一部 knowledge, upload a document, correct knowledge, reorder one SOP step, submit high-risk change upward, and submit a suggestion without edit permission. Each test asserts visible scope, state, and next action.

- [ ] **Step 2: Add Dream and layout journeys**

Dream tests verify child-to-parent summaries, partial coverage, immutable history, rerun lineage, and granted evidence drill-down. Layout tests run at 1280×720 and `page.evaluate(() => document.body.style.zoom='200%')`, with the assistant open and keyboard-only navigation.

- [ ] **Step 3: Run tests before cutover and verify failures**

Run: `pnpm --filter @agentatlas/console exec playwright test`

Expected: FAIL until all new routes and real backend fixtures are wired.

- [ ] **Step 4: Add scripts and visual snapshots**

Add `test:e2e`, `test:visual`, and `test:a11y`. Store baseline screenshots for knowledge home, editor, review, Dream overview/timeline/workflow/run, basic/advanced workflow, evidence page, assistant closed/open, loading, empty, error, and forbidden states.

- [ ] **Step 5: Run the full cutover gate**

Run: `pnpm --filter @agentatlas/console test && pnpm --filter @agentatlas/console build && pnpm --filter @agentatlas/console exec playwright test`

Expected: PASS; no horizontal header wrap, content overlap, inaccessible focus, old token import, ticket input, or emoji product icon.

- [ ] **Step 6: Remove legacy code and prove no duplicate dependency remains**

Run: `rg -n "@agentatlas/claw-runtime-ui|X-Nexus-Ticket|atlas_console_ticket|粘贴管理员票据|var\(--claw-" packages/agentatlas-console packages/claw-runtime-ui`

Expected: no matches in Console source; the deleted mirror directory does not exist. Domain copy may use the word “票据” only in historical documentation, not the UI.

- [ ] **Step 7: Commit**

```bash
git add packages/agentatlas-console package.json pnpm-lock.yaml
git rm -r packages/claw-runtime-ui
git commit -m "feat: cut over AgentAtlas family maintenance console"
```
