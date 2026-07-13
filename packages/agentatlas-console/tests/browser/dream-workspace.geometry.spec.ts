import { expect, test, type Page } from "@playwright/test";

const session = { authenticated: true, enterprise_id: "ent", enterprise_name: "示例企业", enterprise_user_id: "manager", display_name: "陈经理", org_version: 7, org_unit_ids: ["dept-rd"], org_tree: [{ id: "company", name: "全公司", selectable: false, children: [{ id: "dept-rd", name: "研发一部", selectable: true, children: [] }] }], permissions: ["dream:read", "dream:annotate", "dream:rerun", "edit"], advanced_mode_allowed: true };
const handle = "opaque-run-handle-abcdefghijklmnopqrstuvwxyz";
const run = { handle, status: "succeeded", window_start: "2026-07-11T14:00:00Z", window_end: "2026-07-12T14:00:00Z", input_count: 10, coverage: { expected_children: 3, completed_children: 2, input_count: 10 }, missing_input_reasons: ["一个下级组织尚未完成"], facts: [], themes: [], trends: [{ title: "交付加快", detail: "处理时间下降", severity: "info" }], risks: [{ title: "测试记录待补齐", detail: "一个组织未完成", severity: "warning" }], todos: [{ title: "补齐记录", detail: "由负责人跟进", severity: "warning" }], display_summary: "研发一部交付稳定，一个下级组织输入尚未完成。", rerun: false };
const detail = { ...run, annotations: [{ action: "comment", comment: "已安排负责人跟进", actor_name: "陈经理" }], input_organizations: [{ organization_name: "研发二组", relation: "下级组织汇总" }], downstream_organizations: [] };
const binding = { handle: "opaque-binding-handle-abcdefghijklmnopqrstuvwxyz", name: "企业梦境整理", version_label: "第 2 版", output_name: "研发知识" };

async function mockDream(page: Page) {
  await page.route("**/api/session", (route) => route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(session) }));
  await page.route("**/api/dream/runs?**", (route) => route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ runs: [run] }) }));
  await page.route(`**/api/dream/runs/${handle}`, (route) => route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(detail) }));
  await page.route("**/api/dream/policies?**", (route) => route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ dream_policies: [] }) }));
  await page.route("**/api/dream/workflow-bindings?**", (route) => route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ bindings: [binding] }) }));
}

async function assertNoHorizontalOverflow(page: Page) { expect(await page.evaluate(() => document.documentElement.scrollWidth <= document.documentElement.clientWidth)).toBeTruthy(); }

test.beforeEach(async ({ page }) => { await page.setViewportSize({ width: 1280, height: 720 }); await mockDream(page); });

test("Dream overview is readable and navigates by opaque handle", async ({ page }) => {
  await page.goto("/dream");
  await expect(page.getByRole("heading", { name: "梦境全景" })).toBeVisible();
  await expect(page.getByText("覆盖 2/3 个下级组织")).toBeVisible();
  await page.getByRole("link", { name: "查看运行详情" }).click();
  await expect(page).toHaveURL(new RegExp(`/dream/runs/${handle}$`));
  await assertNoHorizontalOverflow(page);
});

test("Dream timeline exposes structured results and detail navigation", async ({ page }) => {
  await page.goto("/dream/timeline");
  await expect(page.getByRole("heading", { name: "梦境时间线" })).toBeVisible();
  await expect(page.getByText("交付加快")).toBeVisible();
  await expect(page.getByText("测试记录待补齐")).toBeVisible();
  await expect(page.getByRole("link", { name: "查看运行详情" })).toHaveAttribute("href", `/dream/runs/${handle}`);
  await assertNoHorizontalOverflow(page);
});

test("Dream workflow bootstraps from human published binding without advanced internals", async ({ page }) => {
  await page.goto("/dream/workflow");
  await expect(page.getByRole("heading", { name: "梦境工作流" })).toBeVisible();
  await expect(page.getByLabel("已发布梦境工作流")).toContainText("企业梦境整理");
  await expect(page.getByText(/cron|timezone|workflow_id|output_space_id/i)).toHaveCount(0);
  await expect(page.getByRole("button", { name: "保存梦境工作流" })).toBeEnabled();
  await assertNoHorizontalOverflow(page);
});

test("Dream detail renders immutable annotations and human lineage", async ({ page }) => {
  await page.goto(`/dream/runs/${handle}`);
  await expect(page.getByRole("heading", { name: "运行与追溯" })).toBeVisible();
  await expect(page.getByText("这是历史运行结果，不能直接修改")).toBeVisible();
  await expect(page.getByText("研发二组")).toBeVisible();
  await expect(page.getByText("已安排负责人跟进")).toBeVisible();
  await expect(page.getByText(/run-hidden|pointer-hidden|workflow-hidden/i)).toHaveCount(0);
  await assertNoHorizontalOverflow(page);
});
