import { expect, test, type Page } from "@playwright/test";
import { execFileSync } from "node:child_process";

const token = process.env.CHAINS_WEB_TEST_TOKEN ?? "web-fixture-token";

async function login(page: Page, path = "/ui/state") {
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

test("guided and DSL rules publish live, download, and import with conflicts", async ({ page }) => {
  await login(page, "/ui/rules");
  const catalog = page.getByRole("heading", { name: "Rule catalog" });
  await expect(catalog).toBeVisible();
  await expect(page.getByRole("button", { name: "manual", exact: true })).toBeVisible();
  await expect(page.locator("#rule-action-0-service-choices option[value=sim-fan]")).toHaveCount(0);
  await page.getByRole("button", { name: "New rule" }).click();
  await page.getByLabel("Rule name").fill("Guided fixture");
  await expect(page.locator("#rule-action-0-service-choices option[value=sim-fan]")).toHaveCount(1);
  await page.getByLabel("Event type", { exact: true }).fill("manual.guided");
  await page.getByLabel("Target service").fill("sim-fan");
  await page.getByLabel("Target device").fill("fan");
  await page.getByLabel("Action", { exact: true }).fill("set_power");
  await page.getByRole("combobox", { name: "value *", exact: true }).selectOption("true");
  await page.getByRole("button", { name: "Validate and preview" }).click();
  await expect(page.getByText("Validated source", { exact: true })).toBeVisible();
  await page.getByRole("button", { name: "Publish", exact: true }).click();
  await expect(page.getByRole("button", { name: "Guided fixture", exact: true })).toBeVisible();
  await page.getByRole("button", { name: "Guided fixture", exact: true }).click();
  await expect(page.getByRole("combobox", { name: "Trigger", exact: true })).toBeVisible();
  await page.getByRole("button", { name: "Close editor" }).click();

  const response = await page.evaluate(async () => fetch("/operator/v1/events", {
    method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ name: "guided", data: {} }),
  }).then(r => r.json()));
  await expect.poll(async () => {
    const trace = await page.request.get("/operator/v1/traces?correlation=" + response.id).then(r => r.json());
    return trace.records.map((r: { type: string }) => r.type);
  }).toContain("action.outcome");

  await page.getByRole("button", { name: "New rule" }).click();
  await page.getByRole("button", { name: "DSL editor" }).click();
  await page.getByLabel("Rule name").fill("DSL fixture");
  await page.getByLabel("Rule source").fill("Event(type=)\n");
  await page.getByRole("button", { name: "Validate and preview" }).click();
  await expect(page.getByRole("alert")).toContainText("Line 1");
  await page.getByLabel("Rule source").fill('Event(type="manual.dsl")\nreturn\n');
  await page.getByRole("button", { name: "Validate and preview" }).click();
  await page.getByRole("button", { name: "Publish", exact: true }).click();
  await expect(page.getByRole("button", { name: "DSL fixture", exact: true })).toBeVisible();

  const download = page.waitForEvent("download");
  await page.getByRole("link", { name: "Download All" }).click();
  expect((await download).suggestedFilename()).toBe("chains-rules.zip");
  await page.getByLabel("Import rules file").setInputFiles({
    name: "Guided fixture.chains", mimeType: "text/plain", buffer: Buffer.from('Event(type="manual.imported")\nreturn\n'),
  });
  await expect(page.getByText("Name already in use")).toBeVisible();
  await page.getByRole("combobox", { name: "Import choice", exact: true }).selectOption("rename");
  await page.getByLabel("New name").fill("Imported fixture");
  await page.getByRole("button", { name: "Publish import" }).click();
  await expect(page.getByRole("button", { name: "Imported fixture", exact: true })).toBeVisible();
});

test("State controls follow schemas and wait for device reports", async ({ page }) => {
  const service = "typed-controls-fixture";
  const observed = (id: string, values: Record<string, boolean | number>) => send("observation.submitted", id, {
    event_type: "device.observed", observations: [{ device_id: "lamp", values }],
  }, "1");
  function send(type: string, id: string, data: unknown, schemaVersion?: string) {
    const message = { version: 3, source: service, type, id, observed_at: new Date().toISOString(), data, schema_version: schemaVersion };
    execFileSync("mosquitto_pub", ["-p", process.env.CHAINS_WEB_TEST_MQTT_PORT ?? "18883", "-q", "1",
      "-t", "web-fixture/msg/" + type + "/" + service + "/_", "-m", JSON.stringify(message)]);
  }
  function schema(value: unknown) { return { type: "object", required: ["value"], properties: { value } }; }
  const power = { type: "boolean" }, dimlevel = { type: "integer", minimum: 0, maximum: 100 };
  const setter = (type: unknown, action: string, label: string, unit?: string) =>
    ({ schema: type, label, unit, setter: { action, schema_version: "1", argument: "value" } });
  await login(page, "/ui/state");
  send("service.registered", "typed-reg", {
    uuid: service, name: "Test light", service_type: "fixture", events: {},
    devices: [{ id: "lamp", name: "Lamp", classes: ["dimmer"], actions: ["power", "dim"],
      attributes: { power: setter(power, "power", "Power"), dimlevel: setter(dimlevel, "dim", "Brightness", "%"),
        temperature: { schema: { type: "number" }, unit: "°C" } } }],
    actions: { power: { "1": schema(power) }, dim: { "1": schema(dimlevel) } },
  });
  await expect.poll(async () => {
    const services = await page.request.get("/operator/v1/services").then(r => r.json());
    return services.find((item: { uuid: string }) => item.uuid === service)?.devices?.[0]?.classes;
  }).toEqual(["toggle", "light", "dimmer"]);
  observed("typed-initial", { power: false, dimlevel: 30, temperature: 22 });
  await page.getByRole("button", { name: "Refresh", exact: true }).click();
  await page.getByRole("button", { name: "Power", exact: true }).click();
  const control = page.getByRole("switch", { name: "Power", exact: true });
  await expect(control).toHaveAttribute("aria-checked", "false");
  await control.click();
  await expect(page.getByText("Requested true. Waiting for a device report.")).toBeVisible();
  await expect(control).toHaveAttribute("aria-checked", "false");
  observed("typed-on", { power: true });
  await expect(control).toHaveAttribute("aria-checked", "true");
  await page.getByRole("button", { name: "Brightness", exact: true }).click();
  await expect(page.getByRole("slider")).toBeVisible();
  await page.getByRole("button", { name: "temperature", exact: true }).click();
  await expect(page.getByRole("button", { name: "Set value" })).toHaveCount(0);

  await page.getByRole("link", { name: "Rules", exact: true }).click();
  await page.getByRole("button", { name: "New rule" }).click();
  await page.getByLabel("Rule name").fill("Dimmer setter fixture");
  await page.getByLabel("Event type", { exact: true }).fill("manual.dim_fixture");
  await page.getByRole("combobox", { name: "Step", exact: true }).selectOption("setter");
  await page.getByLabel("Target service").fill(service);
  await page.getByLabel("Target device").fill("lamp");
  await page.getByLabel("Writable attribute").fill("dimlevel");
  await page.getByRole("spinbutton", { name: "Brightness percentage" }).fill("60");
  await page.getByRole("button", { name: "Validate and preview" }).click();
  await expect(page.locator(".preview pre")).toContainText('attribute="dimlevel", value=60');
});

test("Devices groups across services and edits placement without changing control targets", async ({ page }) => {
  await login(page, "/ui/devices");
  const fan = page.getByRole("article", { name: "Bathroom fan", exact: true });
  const sensor = page.getByRole("article", { name: "bathroom-sensor", exact: true });
  await expect(fan).toBeVisible();
  await expect(sensor).toBeVisible();
  for (const card of [fan, sensor]) {
    await card.getByRole("button", { name: "Edit placement" }).click();
    await card.getByLabel("Location segments").fill("House\nBedroom\nShelf.1");
    await card.getByLabel("Tags, one per line").fill("guest\nbattery");
    await card.getByRole("button", { name: "Save placement" }).click();
    await expect(card.getByRole("button", { name: "Save placement" })).toHaveCount(0);
  }
  await page.getByRole("button", { name: "Bedroom", exact: true }).click();
  await expect(page.getByRole("heading", { name: "House › Bedroom › Shelf.1", exact: true })).toBeVisible();
  await expect(fan).toBeVisible();
  await expect(sensor).toBeVisible();
  await page.getByRole("button", { name: "Shelf.1", exact: true }).click();
  await expect(fan).toBeVisible();
  await page.getByLabel("Tags", { exact: true }).fill("guest,battery");
  await page.getByRole("button", { name: "Apply filters" }).click();
  await expect(page.getByRole("article")).toHaveCount(2);

  const power = fan.getByRole("switch", { name: "power" });
  const before = await power.getAttribute("aria-checked");
  await power.click();
  await expect(power).toHaveAttribute("aria-checked", before === "true" ? "false" : "true");
  await fan.getByRole("button", { name: "Edit placement" }).click();
  await fan.getByRole("button", { name: "Reset location" }).click();
  await page.getByRole("button", { name: "All locations", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Unassigned", exact: true })).toBeVisible();
  await expect(fan).toBeVisible();
  await expect(sensor).toBeVisible();
});

test("household can browse and control registered devices", async ({ page, browser }) => {
  await page.goto("/admin");
  await page.getByLabel("Access token").fill(token);
  await page.getByRole("button", { name: "Sign in" }).click();
  const independent = await page.request.get("/operator/v1/session");
  expect(independent.status()).toBe(401);

  await page.getByRole("button", { name: "New dashboard" }).click();
  await page.getByLabel("Dashboard name").fill("Household test");
  await page.getByLabel("Use as home").check();
  await page.getByLabel("Pin in navigation").check();
  await page.getByRole("button", { name: "Add separator" }).click();
  await page.getByLabel("Heading (optional)").fill("Controls");
  await page.getByRole("button", { name: "Add live group" }).click();
  await page.getByLabel("Device class").fill("toggle");
  await page.getByRole("button", { name: "Preview dashboard" }).click();
  await expect(page.getByRole("article", { name: "Bathroom fan" })).toBeVisible();
  await expect(page.getByRole("switch")).toHaveCount(0);
  await page.getByRole("button", { name: "Save dashboards" }).click();
  await expect(page.getByRole("link", { name: "Household test" })).toBeVisible();

  const household = await browser.newPage({ viewport: { width: 390, height: 844 } });
  try {
    await household.goto(new URL("/", page.url()).href);
    await household.getByLabel("Access token").fill("web-fixture-household");
    await household.getByRole("button", { name: "Sign in" }).click();
    await expect(household.getByRole("heading", { name: "Household test" })).toBeVisible();
    await expect(household.getByRole("heading", { name: "Controls" })).toBeVisible();
    await expect(household.getByRole("link", { name: "Admin" })).toHaveCount(0);
    expect((await household.request.get("/operator/v1/rules")).status()).toBe(401);
    const toggle = household.getByRole("switch", { name: "power" });
    const before = await toggle.getAttribute("aria-checked");
    await toggle.focus();
    await household.keyboard.press("Space");
    await expect(toggle).toHaveAttribute("aria-checked", before === "true" ? "false" : "true");
    expect(await household.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(390);

    await household.getByRole("link", { name: "All Devices" }).click();
    await expect(household.getByRole("article", { name: "bathroom-sensor", exact: true })).toBeVisible();
    await household.getByRole("link", { name: "Home", exact: true }).click();
    await household.reload();
    await expect(household.getByRole("heading", { name: "Household test" })).toBeVisible();
  } finally { await household.close(); }
});
