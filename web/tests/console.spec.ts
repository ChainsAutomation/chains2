import { expect, test, type Page } from "@playwright/test";
import { execFileSync } from "node:child_process";

const token = process.env.CHAINS_WEB_TEST_TOKEN ?? "web-fixture-token";

async function login(page: Page, path = "/ui/") {
  await page.goto(path);
  await page.getByLabel("Operator token").fill(token);
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByRole("navigation", { name: "Main navigation" })).toBeVisible();
}

test("state, profile form, manual draft and correlated rule outcome", async ({ page }) => {
  const errors: string[] = [];
  page.on("pageerror", error => errors.push(error.message));
  await login(page);
  await expect(page.getByRole("heading", { name: "State", exact: true })).toBeVisible();
  await expect(page.locator("#state-service-choices option[value=humidity-sensor]")).toHaveCount(1);
  await page.getByLabel("Service", { exact: true }).fill("humidity-sensor");
  await expect(page.locator("#state-device-choices option[value=bathroom-sensor]")).toHaveCount(1);
  await page.getByLabel("Device", { exact: true }).fill("bathroom-sensor");
  await page.getByLabel("Attribute", { exact: true }).fill("humidity");
  await page.getByRole("button", { name: "humidity", exact: true }).click();
  await expect(page.getByText("Observed", { exact: true })).toBeVisible();
  execFileSync("mosquitto_pub", ["-p", process.env.CHAINS_WEB_TEST_MQTT_PORT ?? "18883", "-q", "1",
    "-t", "sensors/bathroom/humidity", "-m", "61.25"]);
  await expect(page.getByRole("cell", { name: "61.25", exact: true })).toBeVisible({ timeout: 9000 });
  await expect(page.getByText("Observed", { exact: true })).toBeVisible();

  await page.getByRole("link", { name: "Send", exact: true }).click();
  await page.getByLabel("Event name").fill("web_test");
  await page.getByLabel("JSON payload").fill('{"from":"browser"}');
  await page.getByLabel("Profile service").fill("humidity-sensor");
  await page.getByLabel("Event profile").selectOption("humidity");
  await page.getByLabel("Value *").fill("62");
  await page.getByRole("button", { name: "Preview event" }).click();
  await expect(page.locator(".preview pre")).toHaveText("62");
  await page.getByRole("button", { name: "Publish event" }).click();
  await expect(page.getByText("Published", { exact: true })).toBeVisible();
  await page.getByRole("link", { name: "Watch", exact: true }).click();
  await expect(page.getByRole("button", { name: "Inspect humidity.reported" }).first()).toBeVisible();
  await page.getByRole("button", { name: "Pause", exact: true }).click();
  await expect(page.getByRole("button", { name: "Resume live" })).toBeVisible();
  await page.getByRole("button", { name: "Resume live" }).click();

  await page.getByRole("link", { name: "Send", exact: true }).click();
  await expect(page.getByLabel("Value *")).toHaveValue("62");
  await page.getByLabel("Event profile").selectOption("");
  await expect(page.getByLabel("Event name")).toHaveValue("web_test");
  await expect(page.getByLabel("JSON payload")).toHaveValue('{"from":"browser"}');
  await page.getByRole("button", { name: "Preview event" }).click();
  await page.getByRole("button", { name: "Publish event" }).click();
  await page.getByRole("button", { name: "Explore trace" }).click();
  await expect(page.getByRole("heading", { name: "Trace Explorer", exact: true })).toBeVisible();
  await expect.poll(async () => {
    await page.getByRole("button", { name: "Refresh trace" }).click();
    return page.getByRole("button", { name: "Inspect trace step action.outcome" }).count();
  }).toBeGreaterThan(0);
  await expect(page.getByRole("button", { name: "Inspect trace step rule.started" })).toBeVisible();
  await page.getByRole("button", { name: "Inspect trace step action.outcome" }).last().click();
  await expect(page.getByRole("complementary", { name: "Record details" })).toBeVisible();
  await page.reload();
  await expect(page.getByRole("heading", { name: "Trace Explorer", exact: true })).toBeVisible();
  expect(errors).toEqual([]);
});

test("resume cursor, gap, keyboard navigation, narrow layout and revoked session", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  let seenResume = false;
  let calls = 0;
  await page.route("**/operator/v1/activity?*", async route => {
    if (++calls === 1) {
      await route.fulfill({ headers: { "Content-Type": "text/event-stream" }, body: "id: browser-fixture:0\n\n" });
    } else if (route.request().headers()["last-event-id"] === "browser-fixture:0") {
      seenResume = true;
      await route.fulfill({ headers: { "Content-Type": "text/event-stream" }, body:
        'id: browser-fixture:1\ndata: {"type":"watch.gap","time":"2026-01-01T00:00:00Z","summary":"Activity cursor expired"}\n\n' });
    } else {
      await route.continue();
    }
  });
  await login(page, "/ui/watch");
  await expect(page.getByText("Activity cursor expired", { exact: true })).toBeVisible();
  expect(seenResume).toBe(true);
  await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(390);
  const link = page.getByRole("link", { name: "State", exact: true });
  await link.focus();
  await page.keyboard.press("Enter");
  await expect(page.getByRole("heading", { name: "State", exact: true })).toBeVisible();
  const origin = new URL(page.url()).origin;
  expect((await page.request.delete("/operator/v1/session", { headers: { Origin: origin } })).status()).toBe(204);
  await page.getByRole("button", { name: "Refresh", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Operator sign-in" })).toBeVisible();
});
