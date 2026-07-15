import { expect, test, type Page } from "@playwright/test";

// Browser geometry spec for the Task 18D management assessment detail (Visibility
// B / manager scope). It navigates to the CONTEXTUAL Dream route
// /dream/assessments/:id (reached from existing work-memory/Dream context, not a
// new nav surface) and asserts the manager-authorized detail renders the score,
// the level and the manager notes, staying readable at compact viewports. Data
// is supplied via page.route exactly like dream-workspace.geometry.spec.ts (the
// route fail-closes to 503 when unwired, so the spec provides its own payload).

const session = { authenticated: true, enterprise_id: "ent", enterprise_name: "示例企业", enterprise_user_id: "u:div-lead", display_name: "陈经理", org_version: 7, org_unit_ids: ["org:div"], org_tree: [{ id: "company", name: "全公司", selectable: false, children: [{ id: "org:div", name: "研发一部", selectable: true, children: [] }] }], permissions: ["dream:read", "edit"], advanced_mode_allowed: true };

const assessmentID = "wa-assessment-detail-abcdefghijklmnop";
const managerNarrative = "主管备注：整体评级 B+，建议加强跨组协作";
const managerID = "mgr-line-lead";
const assessment = {
  id: assessmentID,
  subject: "actor-op-7",
  level: "individual",
  policy_key: "assess.op",
  period: { start: "2026-06-15T00:00:00Z", end: "2026-07-14T00:00:00Z" },
  dimensions: [
    { dimension: "outcome_completion", state: "assessed", counted_outcome_keys: ["outcome-a", "outcome-b"], satisfied_outcomes: 2, confidence: { level: "high" }, evidence: [{ tier: "verified_outcome", handle: "outcome-a", kind: "outcome" }], blockers: [{ handle: "blk-1", kind: "dependency", confidence: "verified", delay: "external" }] },
    { dimension: "quality", state: "not_assessable", not_assessable_reason: "insufficient_data" },
  ],
  narrative: managerNarrative,
  manager: { confirmed: true, manager: managerID },
};

async function mockAssessment(page: Page) {
  await page.route("**/api/session", (route) => route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(session) }));
  await page.route(`**/api/assessments/${assessmentID}`, (route) => route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ assessment }) }));
}

async function assertNoHorizontalOverflow(page: Page) { expect(await page.evaluate(() => document.documentElement.scrollWidth <= document.documentElement.clientWidth)).toBeTruthy(); }
async function assertCompactViewport(page: Page) { await page.setViewportSize({ width: 640, height: 360 }); await assertNoHorizontalOverflow(page); await expect(page.locator("main").first()).toBeVisible(); }

test.beforeEach(async ({ page }) => { await page.setViewportSize({ width: 1280, height: 720 }); await mockAssessment(page); });

test("assessment detail renders the authorized manager score, level and notes", async ({ page }) => {
  await page.goto(`/dream/assessments/${assessmentID}`);
  await expect(page.getByRole("heading", { name: "员工工作评估详情" })).toBeVisible();
  // The manager-authorized detail exposes the score (satisfied outcomes), the
  // level (confidence) and the manager notes.
  await expect(page.getByText(/完成产出/)).toBeVisible();
  await expect(page.getByText(/置信度等级/)).toBeVisible();
  await expect(page.getByText(managerNarrative)).toBeVisible();
  await expect(page.getByText(new RegExp(managerID))).toBeVisible();
  // Facts and the missing-data dimension are present too.
  await expect(page.getByRole("heading", { name: "outcome_completion" })).toBeVisible();
  await expect(page.getByText(/数据不足/)).toBeVisible();
  await assertCompactViewport(page);
});

test("assessment detail visibility stays readable and shows no raw internals at compact viewports", async ({ page }) => {
  await page.goto(`/dream/assessments/${assessmentID}`);
  await expect(page.getByText("评估结果不可直接修改")).toBeVisible();
  // No connector/credential/raw-internal shapes leak into the rendered detail.
  await expect(page.getByText(/jdbc:|password=|bearer |select .* from/i)).toHaveCount(0);
  await assertCompactViewport(page);
});
