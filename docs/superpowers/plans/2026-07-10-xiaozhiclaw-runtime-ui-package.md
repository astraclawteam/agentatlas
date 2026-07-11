# Xiaozhi Runtime UI Package Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Publish `@xiaozhiclaw/runtime-ui` from the Xiaozhi repository and make Xiaozhi consume it before AgentAtlas or AgentNexus do.

**Architecture:** Move only controlled visual primitives and chat shells behind a package entry point. Keep runtime stores, gateway events, desktop bridges, account/model/voice state, and task business logic in `ui-next`; consumers supply all state and callbacks through typed props.

**Tech Stack:** React 19, TypeScript, Vite library mode, Vitest, Testing Library, Tailwind CSS 4, Radix UI, Lucide React, pnpm workspaces

## Global Constraints

- Package name is exactly `@xiaozhiclaw/runtime-ui` and its initial version is `0.1.0`.
- Warm Paper background is `#F9F8F0`, ink is `#131314`, and default clay is `#D97757` (`217 119 87`).
- Package code cannot import `ui-next/src/stores`, gateway WebSocket hooks, native bridges, account state, model selection, voice state, or goal state.
- Public interactive components are controlled by props and have keyboard-visible focus states.
- Product icons use Lucide React; emoji cannot be product icons.
- Xiaozhi must consume the package through its package name, not a Vite alias to package source.
- The package must emit ESM JavaScript, `.d.ts` declarations, and one explicit `styles.css` export.

---

## File Map

- Create `packages/runtime-ui/package.json`, `tsconfig.json`, `vite.config.ts`, and `src/index.ts` as the publish boundary.
- Create `packages/runtime-ui/src/styles.css` for tokens, fonts, glass utilities, and focus rules.
- Move/adapt primitives into `packages/runtime-ui/src/components/` with one responsibility per file.
- Modify `ui-next/package.json`, `ui-next/src/styles.css`, and imports in existing consumers.
- Add `scripts/check-runtime-ui-boundary.mjs` to prevent forbidden imports.

### Task 1: Scaffold the Publishable Package and Boundary Guard

**Files:**
- Create: `../xiaozhiclaw-runtime/packages/runtime-ui/package.json`
- Create: `../xiaozhiclaw-runtime/packages/runtime-ui/tsconfig.json`
- Create: `../xiaozhiclaw-runtime/packages/runtime-ui/vite.config.ts`
- Create: `../xiaozhiclaw-runtime/packages/runtime-ui/src/index.ts`
- Create: `../xiaozhiclaw-runtime/scripts/check-runtime-ui-boundary.mjs`
- Test: `../xiaozhiclaw-runtime/test/runtime-ui-boundary.test.mjs`

**Interfaces:**
- Consumes: pnpm workspace configuration and React 19 peer dependency.
- Produces: package exports `.` and `./styles.css`; `pnpm --filter @xiaozhiclaw/runtime-ui build`.

- [ ] **Step 1: Write the failing boundary test**

```js
import test from "node:test";
import assert from "node:assert/strict";
import { scanRuntimeUI } from "../scripts/check-runtime-ui-boundary.mjs";

test("runtime-ui has no Xiaozhi business imports", async () => {
  assert.deepEqual(await scanRuntimeUI(), []);
});
```

- [ ] **Step 2: Run it and verify it fails**

Run: `cd ../xiaozhiclaw-runtime && node --test test/runtime-ui-boundary.test.mjs`

Expected: FAIL because the scanner/package does not exist.

- [ ] **Step 3: Add the package manifest and build entry**

```json
{
  "name": "@xiaozhiclaw/runtime-ui",
  "version": "0.1.0",
  "type": "module",
  "files": ["dist"],
  "exports": {
    ".": { "types": "./dist/index.d.ts", "import": "./dist/index.js" },
    "./styles.css": "./dist/styles.css"
  },
  "scripts": { "build": "vite build && tsc --emitDeclarationOnly", "test": "vitest run", "pack:check": "pnpm pack --pack-destination .pack" },
  "peerDependencies": { "react": ">=18.3 <20", "react-dom": ">=18.3 <20" },
  "dependencies": { "@fontsource-variable/hanken-grotesk": "^5.2.8", "@fontsource-variable/noto-sans-sc": "^5.2.10", "@fontsource-variable/source-serif-4": "^5.2.9", "@fontsource/jetbrains-mono": "^5.2.8", "@radix-ui/react-dialog": "^1.1.16", "@radix-ui/react-dropdown-menu": "^2.1.17", "@radix-ui/react-slot": "^1.2.5", "lucide-react": "^0.511.0", "clsx": "^2.1.1", "tailwind-merge": "^3.3.0" },
  "devDependencies": { "@tailwindcss/vite": "^4.1.7", "@testing-library/react": "^16.3.0", "@types/react": "^19.1.4", "@types/react-dom": "^19.1.5", "react": "^19.1.0", "react-dom": "^19.1.0", "tailwindcss": "^4.1.7", "typescript": "^5.8.3", "vite": "^7.3.1", "vitest": "^4.1.5" }
}
```

`vite.config.ts` must build `src/index.ts` as ESM with React/React DOM external, run Tailwind 4 over package-owned source, and emit the compiled family CSS as `dist/styles.css`. `tsconfig.json` emits declarations to `dist`. `src/index.ts` initially exports no components so scaffolding can build independently.

- [ ] **Step 4: Add the forbidden-import scanner**

```js
export async function scanRuntimeUI() {
  const forbidden = [/ui-next\/src\/stores/, /use-gateway-ws/, /native-browser/, /ui-event-bus/, /input-chat-bridge/];
  return await findSourceMatches("packages/runtime-ui/src", forbidden);
}
```

Implement `findSourceMatches` using `node:fs/promises.readdir/readFile`, recursively scanning `.ts`, `.tsx`, and `.css`, and return strings formatted `path:pattern`.

- [ ] **Step 5: Build and test**

Run: `pnpm --filter @xiaozhiclaw/runtime-ui build && node --test test/runtime-ui-boundary.test.mjs`

Expected: package build succeeds and boundary test PASSes.

- [ ] **Step 6: Commit**

```bash
git add packages/runtime-ui scripts/check-runtime-ui-boundary.mjs test/runtime-ui-boundary.test.mjs pnpm-lock.yaml
git commit -m "feat: scaffold publishable runtime UI package"
```

### Task 2: Extract Family Tokens and Visual Primitives

**Files:**
- Create: `../xiaozhiclaw-runtime/packages/runtime-ui/src/styles.css`
- Create: `../xiaozhiclaw-runtime/packages/runtime-ui/src/lib/cn.ts`
- Create: `../xiaozhiclaw-runtime/packages/runtime-ui/src/components/button.tsx`
- Create: `../xiaozhiclaw-runtime/packages/runtime-ui/src/components/input.tsx`
- Create: `../xiaozhiclaw-runtime/packages/runtime-ui/src/components/menu.tsx`
- Create: `../xiaozhiclaw-runtime/packages/runtime-ui/src/components/dialog.tsx`
- Create: `../xiaozhiclaw-runtime/packages/runtime-ui/src/components/sheet.tsx`
- Create: `../xiaozhiclaw-runtime/packages/runtime-ui/src/components/design-provider.tsx`
- Create: `../xiaozhiclaw-runtime/packages/runtime-ui/src/components/docked-panel.tsx`
- Test: `../xiaozhiclaw-runtime/packages/runtime-ui/src/components/primitives.test.tsx`

**Interfaces:**
- Consumes: `className`, standard React DOM props, and `open/onOpenChange` for panels.
- Produces: `DesignProvider`, `Button`, `Input`, `Menu`, `Dialog`, `Sheet`, `DockedPanel`, `cn`, and stable CSS variables/utilities.

- [ ] **Step 1: Write failing primitive behavior tests**

```tsx
it("docked panel participates in layout and returns focus", async () => {
  const opener = createRef<HTMLButtonElement>();
  render(<><button ref={opener}>open</button><DockedPanel open title="Atlas 助手" onOpenChange={() => {}}>chat</DockedPanel></>);
  expect(screen.getByRole("complementary", { name: "Atlas 助手" })).toHaveAttribute("data-layout", "docked");
  await userEvent.keyboard("{Escape}");
  expect(screen.getByRole("button", { name: "open" })).toHaveFocus();
});
```

- [ ] **Step 2: Verify failure**

Run: `pnpm --filter @xiaozhiclaw/runtime-ui test -- primitives.test.tsx`

Expected: FAIL because exports are missing.

- [ ] **Step 3: Copy canonical tokens, not a visual approximation**

`styles.css` must define the canonical variables from `ui-next/src/styles.css`, including:

```css
:root {
  --color-background: #F9F8F0;
  --color-foreground: #131314;
  --clay-rgb: 217 119 87;
  --radius-card: 1.125rem;
  --radius-field: 1.25rem;
  --glass-border: rgba(255,255,255,.82);
}
.glass-surface { background: var(--glass-bg); border: 1px solid var(--glass-border); backdrop-filter: blur(24px); }
:focus-visible { outline: 2px solid rgb(var(--clay-rgb)); outline-offset: 2px; }
```

Import the existing Hanken Grotesk, Noto Sans SC, Source Serif 4, and JetBrains Mono assets through package dependencies; do not fetch fonts from a CDN.

- [ ] **Step 4: Implement primitives with stable public props**

```ts
export interface DockedPanelProps {
  open: boolean;
  title: string;
  onOpenChange(open: boolean): void;
  mode?: "docked" | "fullscreen";
  returnFocusRef?: React.RefObject<HTMLElement | null>;
  children: React.ReactNode;
}
```

`DesignProvider` accepts controlled `theme`, `accent`, and `children` props and owns only family data attributes; it never reads a Xiaozhi store. `Menu`, `Dialog`, and `Sheet` wrap Radix focus/keyboard behavior with family classes. `DockedPanel` renders `<aside data-layout="docked">` on desktop and a dialog route container in fullscreen mode. Escape calls `onOpenChange(false)` and focuses `returnFocusRef.current`.

- [ ] **Step 5: Export and verify**

Run: `pnpm --filter @xiaozhiclaw/runtime-ui test && pnpm --filter @xiaozhiclaw/runtime-ui build`

Expected: all package tests PASS and `dist/styles.css` contains `--clay-rgb: 217 119 87`.

- [ ] **Step 6: Commit**

```bash
git add packages/runtime-ui pnpm-lock.yaml
git commit -m "feat: extract Xiaozhi family UI primitives"
```

### Task 3: Extract Controlled Chat Components

**Files:**
- Create: `../xiaozhiclaw-runtime/packages/runtime-ui/src/components/prompt-input.tsx`
- Create: `../xiaozhiclaw-runtime/packages/runtime-ui/src/components/message-list.tsx`
- Create: `../xiaozhiclaw-runtime/packages/runtime-ui/src/components/approval-card.tsx`
- Create: `../xiaozhiclaw-runtime/packages/runtime-ui/src/components/attachment-card.tsx`
- Create: `../xiaozhiclaw-runtime/packages/runtime-ui/src/components/agent-chat-shell.tsx`
- Test: `../xiaozhiclaw-runtime/packages/runtime-ui/src/components/agent-chat-shell.test.tsx`

**Interfaces:**
- Consumes: `messages`, `value`, `attachments`, `busy`, `onChange`, `onSend`, and `onApproval` callbacks.
- Produces: `PromptInput`, `MessageList`, `ApprovalCard`, `AttachmentCard`, and `AgentChatShell` without business stores.

- [ ] **Step 1: Write the failing controlled-input test**

```tsx
it("submits through caller callback and never owns session state", async () => {
  const onSend = vi.fn();
  render(<AgentChatShell messages={[]} value="修正 MES SOP" attachments={[]} onChange={() => {}} onSend={onSend} />);
  await userEvent.keyboard("{Enter}");
  expect(onSend).toHaveBeenCalledWith({ text: "修正 MES SOP", attachments: [] });
});
```

- [ ] **Step 2: Verify failure**

Run: `pnpm --filter @xiaozhiclaw/runtime-ui test -- agent-chat-shell.test.tsx`

Expected: FAIL because `AgentChatShell` is not exported.

- [ ] **Step 3: Port the pure `PromptInput` implementation**

Move the behavior from `ui-next/src/components/ui/prompt-input.tsx`, replacing `@/lib/utils` with package-local `cn`. Keep `value/onChange/onSubmit`, paste/drop callbacks, attachment slot, footer slot, and imperative focus handle unchanged.

- [ ] **Step 4: Define package-neutral chat types and shell**

```ts
export interface RuntimeMessage { id: string; role: "user" | "assistant" | "system"; content: React.ReactNode }
export interface RuntimeAttachment { id: string; name: string; size?: number; state?: "ready" | "uploading" | "failed" }
export interface AgentChatShellProps {
  messages: RuntimeMessage[]; value: string; attachments: RuntimeAttachment[]; busy?: boolean;
  onChange(value: string): void;
  onSend(input: { text: string; attachments: RuntimeAttachment[] }): void | Promise<void>;
}
```

Approval actions are data-driven (`id`, `label`, `kind`) and call the consumer; they never call Xiaozhi or AgentAtlas APIs directly.

- [ ] **Step 5: Run unit, accessibility, and package-boundary tests**

Run: `pnpm --filter @xiaozhiclaw/runtime-ui test && node --test test/runtime-ui-boundary.test.mjs`

Expected: PASS; scanning finds zero forbidden imports.

- [ ] **Step 6: Commit**

```bash
git add packages/runtime-ui test/runtime-ui-boundary.test.mjs
git commit -m "feat: publish controlled chat UI components"
```

### Task 4: Make Xiaozhi Consume the Package

**Files:**
- Modify: `../xiaozhiclaw-runtime/ui-next/package.json`
- Modify: `../xiaozhiclaw-runtime/ui-next/src/components/ui/prompt-input.tsx`
- Modify: `../xiaozhiclaw-runtime/ui-next/src/components/messages/approval-card.tsx`
- Modify: `../xiaozhiclaw-runtime/ui-next/src/components/chat/input-box.tsx`
- Modify: `../xiaozhiclaw-runtime/ui-next/src/styles.css`
- Test: `../xiaozhiclaw-runtime/ui-next/src/lib/runtime-ui-consumer.test.tsx`

**Interfaces:**
- Consumes: package exports from Tasks 2–3.
- Produces: Xiaozhi adapter components preserving current runtime behavior while shared visuals come from the package.

- [ ] **Step 1: Add a failing import-boundary test**

```ts
expect(source("src/components/chat/input-box.tsx")).toContain("@xiaozhiclaw/runtime-ui");
expect(source("src/components/chat/input-box.tsx")).not.toContain("./ui/prompt-input");
```

- [ ] **Step 2: Verify failure**

Run: `cd ../xiaozhiclaw-runtime/ui-next && pnpm test -- src/lib/runtime-ui-consumer.test.tsx`

Expected: FAIL on current local imports.

- [ ] **Step 3: Add `@xiaozhiclaw/runtime-ui: workspace:*` and migrate imports**

Keep `InputBox` as the Xiaozhi business adapter. It may read stores and gateway events, but it renders package `PromptInput`, attachment cards, and approval cards through props. Remove duplicated component bodies only after their existing tests pass.

- [ ] **Step 4: Make package styles the token source**

Import `@xiaozhiclaw/runtime-ui/styles.css` before Xiaozhi application-specific CSS. Delete duplicated canonical token declarations from `ui-next/src/styles.css`; leave only Xiaozhi-specific layout/animation rules and accent overrides.

- [ ] **Step 5: Run Xiaozhi regression suites**

Run: `cd ../xiaozhiclaw-runtime && pnpm --filter xiaozhiclaw-runtime-control-ui-next test && pnpm --filter xiaozhiclaw-runtime-control-ui-next build`

Expected: PASS with no visual or controlled-input regressions.

- [ ] **Step 6: Commit**

```bash
git add ui-next packages/runtime-ui pnpm-lock.yaml
git commit -m "refactor: consume shared runtime UI in Xiaozhi"
```

### Task 5: Verify the Packed Artifact

**Files:**
- Create: `../xiaozhiclaw-runtime/packages/runtime-ui/test/packed-consumer.test.mjs`
- Modify: `../xiaozhiclaw-runtime/package.json`

**Interfaces:**
- Consumes: built package from Task 4.
- Produces: a tarball proven usable without workspace source aliases.

- [ ] **Step 1: Write the packed-consumer test**

```js
const pkg = JSON.parse(await readFile(".pack/package/package.json", "utf8"));
assert.equal(pkg.exports["."].import, "./dist/index.js");
assert.equal(pkg.exports["./styles.css"], "./dist/styles.css");
assert.match(await readFile(".pack/package/dist/index.d.ts", "utf8"), /PromptInput/);
```

- [ ] **Step 2: Pack and verify**

Run: `pnpm --filter @xiaozhiclaw/runtime-ui build && pnpm --filter @xiaozhiclaw/runtime-ui pack:check && node packages/runtime-ui/test/packed-consumer.test.mjs`

Expected: PASS and a `xiaozhiclaw-runtime-ui-0.1.0.tgz` artifact.

- [ ] **Step 3: Add root scripts and run the full gate**

Add `ui:package:test` as `pnpm --filter @xiaozhiclaw/runtime-ui test && pnpm --filter @xiaozhiclaw/runtime-ui build`.

Run: `pnpm ui:package:test && pnpm --filter xiaozhiclaw-runtime-control-ui-next test`

Expected: all tests PASS.

- [ ] **Step 4: Commit**

```bash
git add package.json packages/runtime-ui pnpm-lock.yaml
git commit -m "test: verify distributable runtime UI package"
```
