import { expect, test, type Page } from "@playwright/test";

const session = { authenticated: true, enterprise_id: "ent", enterprise_name: "示例企业", enterprise_user_id: "manager", display_name: "陈经理", org_version: 7, org_unit_ids: ["dept-rd"], org_tree: [{ id: "company", name: "全公司", selectable: false, children: [{ id: "dept-rd", name: "研发一部", selectable: true, children: [] }] }], permissions: ["dream:read", "dream:annotate", "dream:rerun", "edit"], advanced_mode_allowed: true };
const run = { run_id: "run-hidden", status: "succeeded", org_unit_id: "dept-rd", window_start: "2026-07-11T14:00:00Z", window_end: "2026-07-12T14:00:00Z", policy_version: 2, workflow: { id: "wf-hidden", version: 3 }, parent_run_ids: [], input_count: 10, coverage: { expected_children: 3, completed_children: 2, input_count: 10 }, missing_inputs: [{ source_type: "child_dream_summary", source_id: "child-hidden", reason: "not_completed" }], facts: [], themes: [], trends: [{ id: "trend", title: "交付加快", detail: "处理时间下降", severity: "info" }], risks: [{ id: "risk", title: "测试记录待补齐", detail: "一个组织未完成", severity: "warning" }], todos: [{ id: "todo", title: "补齐记录", detail: "由负责人跟进", severity: "warning" }], display_summary: "研发一部交付稳定，一个下级组织输入尚未完成。", evidence_pointer_id: "pointer-hidden", input_snapshot: { source_counts: [], sanitized_input_ids: [] }, visibility_snapshot: { visibility_level: "managers", org_unit_ids: ["dept-rd"], masked_field_count: 2 }, model_route: "hidden", model_version: "企业模型 2.1", attempt: 1, idempotency_key: "hidden" };

async function openDream(page: Page) {
  await page.route("**/api/session", (route) => route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(session) }));
  await page.route("**/api/dream/runs?**", (route) => route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ runs: [run] }) }));
  await page.goto("/dream");
  await expect(page.getByRole("heading", { name: "梦境全景" })).toBeVisible();
}

test("Dream hierarchy remains readable at 1280 and 200 percent zoom equivalent", async ({ page }, testInfo) => {
  await page.setViewportSize({ width: 1280, height: 720 });
  await openDream(page);
  await expect(page.getByRole("list", { name: "组织梦境层级" }).getByText("研发一部", { exact: true })).toBeVisible();
  await expect(page.getByText("覆盖 2/3 个下级组织")).toBeVisible();
  await expect(page.getByText("有 1 项输入未完成")).toBeVisible();
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= document.documentElement.clientWidth)).toBeTruthy();
  await testInfo.attach("dream-overview-1280.png", { body: await page.screenshot(), contentType: "image/png" });

  await page.setViewportSize({ width: 640, height: 360 });
  await expect(page.getByRole("list", { name: "组织梦境层级" }).getByText("研发一部", { exact: true })).toBeVisible();
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= document.documentElement.clientWidth)).toBeTruthy();
  await testInfo.attach("dream-overview-200-percent-equivalent.png", { body: await page.screenshot(), contentType: "image/png" });
});
