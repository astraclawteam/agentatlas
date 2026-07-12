import { expect, test, type Page } from "@playwright/test";

const session = {
  authenticated: true,
  enterprise_id: "ent-visual",
  enterprise_name: "示例企业",
  enterprise_user_id: "manager",
  display_name: "陈经理",
  org_version: 7,
  org_unit_ids: ["dept-rd"],
  org_tree: [
    {
      id: "company:root",
      name: "全公司",
      selectable: false,
      children: [{ id: "dept-rd", name: "研发一部", selectable: true, children: [] }],
    },
  ],
  permissions: ["knowledge:read"],
  advanced_mode_allowed: true,
};

async function openConsole(page: Page, path = "/knowledge/dept-rd") {
  await page.route("**/api/session", (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(session) }),
  );
  await page.goto(path);
  await expect(page.getByRole("heading", { name: "企业知识" })).toBeVisible();
}

test("header controls are exactly aligned and the dock reflows without overlap at 1280x720", async ({ page }, testInfo) => {
  await page.setViewportSize({ width: 1280, height: 720 });
  await openConsole(page);
  const agent = page.getByRole("button", { name: "打开 Atlas 助手" });
  const user = page.locator(".console-user-group");
  const actions = page.getByTestId("header-user-actions");
  const [agentBox, userBox, actionsBox] = await Promise.all([agent.boundingBox(), user.boundingBox(), actions.boundingBox()]);
  expect(agentBox).not.toBeNull();
  expect(userBox).not.toBeNull();
  expect(actionsBox).not.toBeNull();
  expect(agentBox!.height).toBe(48);
  expect(userBox!.height).toBe(48);
  expect(agentBox!.y).toBe(userBox!.y);
  expect(agentBox!.x + agentBox!.width).toBeLessThanOrEqual(userBox!.x);
  expect(await actions.evaluate((element) => element.children[0]?.getAttribute("aria-label"))).toBe("打开 Atlas 助手");

  const main = page.locator("main.console-main");
  const before = await main.boundingBox();
  await agent.click();
  const panel = page.getByRole("complementary", { name: "Atlas 助手" });
  await expect(panel).toBeVisible();
  const [after, panelBox] = await Promise.all([main.boundingBox(), panel.boundingBox()]);
  expect(after!.width).toBeLessThan(before!.width);
  expect(after!.x + after!.width).toBeLessThanOrEqual(panelBox!.x);
  await testInfo.attach("console-1280-docked.png", { body: await page.screenshot(), contentType: "image/png" });

  await page.keyboard.press("Escape");
  await expect(panel).toBeHidden();
  await expect(agent).toBeFocused();
});

test("200 percent zoom equivalent and narrow screens stay operable through the assistant route", async ({ page }, testInfo) => {
  await page.setViewportSize({ width: 640, height: 360 });
  await openConsole(page);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= document.documentElement.clientWidth)).toBeTruthy();
  await expect(page.getByRole("button", { name: "打开 Atlas 助手" })).toBeVisible();
  await testInfo.attach("console-200-percent-equivalent.png", { body: await page.screenshot(), contentType: "image/png" });

  await page.setViewportSize({ width: 800, height: 720 });
  await page.getByRole("button", { name: "打开 Atlas 助手" }).click();
  await expect(page).toHaveURL(/\/assistant$/);
  await expect(page.getByRole("dialog", { name: "Atlas 助手" })).toBeVisible();
});
