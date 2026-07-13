# AgentAtlas Console Task 5 WIP Handoff

Date: 2026-07-13
Base: `57b3714732b5de939d34d060d0db0c0d0bfcfe97`
Plan: `docs/superpowers/plans/2026-07-10-agentatlas-console-refactor.md`, Task 5

## Status

This is an intentionally incomplete handoff. Do not mark Console Task 5 complete
or begin Console Task 6 until the remaining contract, routing, integration, and
review gates below have passed.

## Implemented in this WIP

- Canonical `AtlasWorkflow` frontend types now retain workflow variables and
  input/output schemas and tolerate future node types.
- `flowgram-adapter.ts` preserves canonical workflow, node, and edge fields in
  namespaced FlowGram data and reconstructs edited graph nodes and edges.
- Basic-mode classification rejects branches, conditional edges, unknown node
  types, cycles, disconnected nodes, duplicate edges, and multiple incoming
  edges instead of flattening them into a linear SOP.
- A structured SOP editor and an Advanced FlowGram wrapper have been drafted.
- A repository-driven Workflow Studio draft covers same-draft revision updates,
  deriving a draft from a published version, optimistic-conflict messaging,
  Advanced-mode gating, and routing publication into governed review.
- `AtlasWorkflowCanvas` uses the lossless adapter and registers unknown node
  types found in a canonical workflow.

## Verification at handoff

The focused command passed with 18 tests:

```powershell
pnpm --filter @agentatlas/console test -- flowgram-adapter.test.ts WorkflowStudio.test.tsx AtlasWorkflowCanvas.test.tsx
```

This focused result proves only the isolated WIP components. It does not prove
Task 5 integration or production readiness.

## Required next work

1. Create the workflow list page and connect the new studio to the real
   `/workflows/*` route. The current product route still renders the legacy
   adapter/studio.
2. Add browser-session BFF and OpenAPI contracts for bounded workflow list/get,
   draft derivation from an immutable published version, revision-based update,
   diff, and governed review preparation. Do not reuse the legacy ticket client.
3. Use opaque session-bound browser handles. Enforce enterprise and exact
   organization authorization through AgentNexus; Basic mode must not expose raw
   IDs, JSON, node type names, or internal revision metadata.
4. Complete the 409 workflow: show the user's draft beside the latest server
   version and make `比较并重新应用` functional rather than presentational.
5. Ensure Advanced FlowGram requires both `workflow_advanced` permission and an
   explicitly enabled Advanced mode, without broadening organization scope.
6. Wire FlowGram export to the current draft and verify add/delete/edit behavior
   for nodes, edges, conditions, configuration, schemas, and unknown types.
7. Remove the direct publish path from the active workflow surface and prove
   `下一步：检查并发布` creates/opens the shared governed change review.
8. Add browser geometry/keyboard coverage at 1280x720 and 200% zoom, then run
   the full Console suite, production build, Playwright suite, relevant Go tests,
   OpenAPI drift checks, and `git diff --check`.
9. Run independent specification and code-quality reviews. Record final evidence
   in `.superpowers/sdd/progress.md` only after both approve.

## Files in the WIP

- `packages/agentatlas-console/src/types.ts`
- `packages/agentatlas-console/src/AtlasWorkflowCanvas.tsx`
- `packages/agentatlas-console/src/AtlasWorkflowCanvas.test.tsx`
- `packages/agentatlas-console/src/features/workflows/flowgram-adapter.ts`
- `packages/agentatlas-console/src/features/workflows/flowgram-adapter.test.ts`
- `packages/agentatlas-console/src/features/workflows/SOPStructuredEditor.tsx`
- `packages/agentatlas-console/src/features/workflows/AdvancedWorkflowStudio.tsx`
- `packages/agentatlas-console/src/features/workflows/WorkflowStudio.tsx`
- `packages/agentatlas-console/src/features/workflows/WorkflowStudio.test.tsx`
- `packages/agentatlas-console/src/features/workflows/workflows.css`
