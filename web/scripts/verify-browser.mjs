import assert from "node:assert/strict";
import { mkdir } from "node:fs/promises";
import { chromium } from "playwright";
const origin = process.env.CONSOLE_URL || "http://127.0.0.1:15173";
const out = process.env.ARTIFACT_DIR || "/tmp/argon-console-checks";
await mkdir(out, { recursive: true });
const browser = await chromium.launch({ headless: true });
const context = await browser.newContext({
  viewport: { width: 1440, height: 1000 },
});
await context.addInitScript(() => localStorage.setItem("argon.tour.done", "1"));
const page = await context.newPage();
const errors = [];
const events = [];
const native = [];
page.on("pageerror", (e) => errors.push(e.message));
page.on("request", (r) => {
  if (r.url().endsWith("/api/v1/events")) events.push(r.postDataJSON());
  if (/\/sandboxes|\/checkout/.test(r.url())) native.push(r.url());
});
try {
  await page.goto(origin);
  await page
    .getByRole("button", { name: "run agent session", exact: true })
    .waitFor();
  assert.equal(events.length, 0);
  assert.equal(
    await page
      .getByRole("button", { name: "create sandbox", exact: true })
      .count(),
    0,
  );
  await page.getByRole("checkbox").check();
  const scenarioResponse = page.waitForResponse(
    (r) =>
      r.url().endsWith("/api/v1/demo/scenario") &&
      r.request().method() === "POST",
  );
  await page
    .getByRole("button", { name: "run agent session", exact: true })
    .click();
  const scenario = await (await scenarioResponse).json();
  assert.equal(scenario.branches.length, 2);
  assert.ok(scenario.pin);
  await page
    .getByRole("link", { name: "Executor (review conflict) →" })
    .click();
  await page
    .getByRole("button", { name: "compute diff against parent" })
    .click();
  await page
    .getByRole("button", { name: "create merge plan", exact: true })
    .waitFor();
  assert.match(await page.locator("body").innerText(), /1 conflicts/);
  assert.equal(
    await page.getByRole("button", { name: "checkout", exact: true }).count(),
    0,
  );
  assert.equal(
    await page.locator("code").filter({ hasText: "mongodb://" }).count(),
    0,
  );
  await page.screenshot({
    path: `${out}/executor-desktop.png`,
    fullPage: true,
  });
  await page
    .getByRole("button", { name: "create merge plan", exact: true })
    .click();
  await page
    .getByText("Conflicts · 1 — never resolved silently", { exact: true })
    .waitFor();
  // Keep the already accepted planner price: conflict choice is explicit.
  await page.getByRole("combobox").selectOption("ours");
  await page.getByRole("button", { name: "apply merge", exact: true }).click();
  await page.getByRole("button", { name: "confirm?", exact: true }).click();
  await page.getByText(/^applied \d+ changes/).waitFor();
  await page.screenshot({ path: `${out}/merge-desktop.png`, fullPage: true });
  const project = new URL(page.url()).pathname.split("/")[2];
  await page.goto(`${origin}/p/${project}/b/${scenario.branch}`);
  const undoPanel = page.locator('[data-tour="undo"]');
  const undoResponse = (dry) =>
    page.waitForResponse(
      (r) =>
        r.url().endsWith("/undo") &&
        r.request().postDataJSON()?.dry_run === dry,
    );
  let previewResponse = undoResponse(true);
  await page.getByRole("button", { name: "undo this actor →" }).first().click();
  let preview = await previewResponse;
  const originalScope = preview.request().postDataJSON();
  let reviewed = await preview.json();
  const applyButton = undoPanel.getByRole("button", { name: /^apply undo/ });
  await applyButton.waitFor();
  // Every scope edit invalidates the review, including changing the actor.
  await undoPanel.getByRole("combobox").selectOption("");
  assert.equal(
    await applyButton.count(),
    0,
    "actor edit invalidates undo review",
  );
  await undoPanel.getByRole("combobox").selectOption(originalScope.actor);
  previewResponse = undoResponse(true);
  await undoPanel.getByRole("button", { name: "dry run", exact: true }).click();
  await previewResponse;
  await applyButton.waitFor();
  await undoPanel
    .getByPlaceholder("990")
    .fill(String(originalScope.from_lsn + 1));
  assert.equal(
    await applyButton.count(),
    0,
    "from LSN edit invalidates review",
  );
  await undoPanel.getByPlaceholder("990").fill(String(originalScope.from_lsn));
  previewResponse = undoResponse(true);
  await undoPanel.getByRole("button", { name: "dry run", exact: true }).click();
  await previewResponse;
  await applyButton.waitFor();
  await undoPanel.getByPlaceholder("head").fill(String(reviewed.to_lsn));
  assert.equal(await applyButton.count(), 0, "to LSN edit invalidates review");
  await undoPanel.getByPlaceholder("head").fill("");
  // Delay a real engine response; edits while it is in flight must win.
  let releasePreview;
  const delayPreview = new Promise((resolve) => {
    releasePreview = resolve;
  });
  let sawPreview;
  const previewSent = new Promise((resolve) => {
    sawPreview = resolve;
  });
  await page.route("**/undo", async (route) => {
    const response = await route.fetch();
    sawPreview();
    await delayPreview;
    await route.fulfill({ response });
  });
  await undoPanel.getByRole("button", { name: "dry run", exact: true }).click();
  await previewSent;
  await undoPanel.getByRole("combobox").selectOption("");
  releasePreview();
  await undoPanel
    .getByRole("button", { name: "dry run", exact: true })
    .waitFor();
  await page.waitForFunction(
    () =>
      !Array.from(document.querySelectorAll("button")).find(
        (b) => b.textContent === "dry run",
      )?.disabled,
  );
  assert.equal(
    await applyButton.count(),
    0,
    "late preview cannot revive invalidated scope",
  );
  await page.unroute("**/undo");
  await undoPanel.getByRole("combobox").selectOption(originalScope.actor);
  previewResponse = undoResponse(true);
  await undoPanel.getByRole("button", { name: "dry run", exact: true }).click();
  preview = await previewResponse;
  reviewed = await preview.json();
  await applyButton.waitFor();
  await applyButton.click();
  const applyResponse = undoResponse(false);
  await undoPanel
    .getByRole("button", { name: "confirm?", exact: true })
    .click();
  const applied = await applyResponse;
  assert.deepEqual(
    applied.request().postDataJSON(),
    {
      from_lsn: reviewed.from_lsn,
      to_lsn: reviewed.to_lsn,
      actor: originalScope.actor,
      dry_run: false,
    },
    "execution uses the immutable reviewed actor and bounded LSN range",
  );
  await page.getByText(/— applied:/).waitFor();
  const docs = await context.request.get(
    `${origin}/api/v1/projects/${project}/branches/${scenario.branch}/time-travel`,
  );
  const info = await docs.json();
  const state = await context.request.get(
    `${origin}/api/v1/projects/${project}/branches/${scenario.branch}/time-travel/query?lsn=${info.LatestLSN}&collection=orders`,
  );
  assert.equal(
    (await state.json()).documents.find((d) => d._id === "o1").price,
    49,
    "undo restores original executor input",
  );
  await page.setViewportSize({ width: 390, height: 844 });
  for (const route of [`/p/${project}`, `/p/${project}/b/${scenario.branch}`]) {
    await page.goto(origin + route);
    await page
      .getByText(
        route.endsWith(scenario.branch)
          ? "Branch · " + scenario.branch
          : "02 · Branches",
        { exact: true },
      )
      .waitFor();
    assert.ok(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= innerWidth + 1,
      ),
      route + " mobile overflow",
    );
    await page.screenshot({
      path: `${out}/${route.endsWith(scenario.branch) ? "branch" : "project"}-mobile.png`,
      fullPage: true,
    });
  }
  // Exercise the capture status contract, including a degraded history warning.
  const detail = await (
    await context.request.get(
      `${origin}/api/v1/projects/${project}/branches/${scenario.branch}`,
    )
  ).json();
  await page.route("**/api/v1/status/ingesters", (route) =>
    route.fulfill({
      json: {
        ingesters: [],
        count: 0,
        capture: [
          {
            branch_id: detail.branch.id,
            state: "degraded",
            actor: "agent:executor",
            error: "Unsupported collection rename; history is incomplete",
            updated_at: new Date().toISOString(),
            last_captured_at: new Date().toISOString(),
            head_lsn: 42,
            last_event_lag_ms: 12,
          },
        ],
      },
    }),
  );
  await page.reload();
  await page
    .getByText("Unsupported collection rename; history is incomplete", {
      exact: true,
    })
    .waitFor();
  assert.match(
    await page.locator("body").innerText(),
    /Last observed capture delay: 12 ms/,
  );
  await page.screenshot({
    path: `${out}/capture-degraded-mobile.png`,
    fullPage: true,
  });
  const failing = await browser.newContext();
  const failurePage = await failing.newPage();
  await failurePage.route("**/api/v1/demo/scenario", (route) =>
    route.fulfill({
      status: 503,
      json: { error: "Scenario temporarily unavailable. Try again." },
    }),
  );
  await failurePage.goto(origin);
  await failurePage
    .getByRole("button", { name: "next", exact: true })
    .waitFor();
  await failurePage.getByRole("button", { name: "next", exact: true }).click();
  await failurePage
    .getByRole("button", { name: "run it →", exact: true })
    .click();
  await failurePage
    .getByRole("alert")
    .getByText("Scenario temporarily unavailable. Try again.", { exact: true })
    .waitFor();
  await failing.close();
  // Real cookie loss (expiry or a reset instance) must offer recovery.
  const recovery = await browser.newContext();
  await recovery.addInitScript(() =>
    localStorage.setItem("argon.tour.done", "1"),
  );
  const recoveryPage = await recovery.newPage();
  recoveryPage.on("pageerror", (error) => errors.push(error.message));
  await recoveryPage.goto(origin);
  await recoveryPage
    .getByRole("button", { name: "run agent session", exact: true })
    .waitFor();
  const oldProjectURL = recoveryPage.url();
  const otherProject = new URL(oldProjectURL).pathname.split("/")[2];
  assert.equal(
    (
      await context.request.get(
        `${origin}/api/v1/projects/${otherProject}/branches`,
      )
    ).status(),
    404,
    "separate real sessions cannot read each other's projects",
  );
  assert.equal(
    (
      await context.request.post(
        `${origin}/api/v1/projects/${project}/branches/main/checkout`,
        { data: {} },
      )
    ).status(),
    403,
    "hosted demo refuses native checkout server-side",
  );
  // Keep one genuinely unauthorized request in flight while other requests
  // detect cookie loss and the user renews. Its late 401 must not expire the
  // newly created session after the old query has been cancelled/unmounted.
  let releaseOldRequest;
  const delayedOldRequest = new Promise((resolve) => {
    releaseOldRequest = resolve;
  });
  let sawOldRequest;
  const oldRequestArrived = new Promise((resolve) => {
    sawOldRequest = resolve;
  });
  let heldOldURL = "";
  await recoveryPage.route("**/api/v1/projects/**", async (route) => {
    if (heldOldURL || route.request().method() !== "GET") {
      await route.continue();
      return;
    }
    heldOldURL = route.request().url();
    const response = await route.fetch();
    assert.equal(response.status(), 401);
    sawOldRequest();
    await delayedOldRequest;
    await route.fulfill({ response });
  });
  await recovery.clearCookies();
  await recoveryPage.getByRole("link", { name: "main", exact: true }).click();
  await Promise.race([
    oldRequestArrived,
    new Promise((_, reject) =>
      setTimeout(
        () =>
          reject(
            new Error("Timed out waiting for delayed old-session request"),
          ),
        10000,
      ),
    ),
  ]);
  await recoveryPage
    .getByRole("heading", { name: "Your sample session has ended" })
    .waitFor();
  await recoveryPage
    .getByRole("button", { name: "Start a new sample session" })
    .click();
  await recoveryPage
    .getByRole("button", { name: "run agent session", exact: true })
    .waitFor();
  assert.notEqual(
    recoveryPage.url(),
    oldProjectURL,
    "recovery provisions a fresh real project",
  );
  const lateResponse = recoveryPage.waitForResponse(
    (response) => response.url() === heldOldURL && response.status() === 401,
  );
  releaseOldRequest();
  await lateResponse;
  await recoveryPage.unroute("**/api/v1/projects/**");
  await recoveryPage.waitForLoadState("networkidle");
  assert.equal(
    await recoveryPage
      .getByRole("heading", { name: "Your sample session has ended" })
      .count(),
    0,
    "late old-session 401 cannot expire a renewed session",
  );
  await recoveryPage.goto(oldProjectURL + "/b/main");
  await recoveryPage
    .getByRole("button", { name: "run agent session", exact: true })
    .waitFor();
  assert.notEqual(
    new URL(recoveryPage.url()).pathname,
    new URL(oldProjectURL).pathname + "/b/main",
    "old deep link redirects to current session",
  );
  await recoveryPage
    .getByRole("status")
    .getByText(/That sample session has ended/)
    .waitFor();
  // Exercise the expiry timer without an hour-long wait. The expiry header is
  // shortened in transit; project creation and every recovery request stay real.
  await recoveryPage.route("**/api/v1/demo/session", async (route) => {
    const response = await route.fetch();
    const data = await response.json();
    await route.fulfill({
      response,
      json: { ...data, expires_at: new Date(Date.now() + 1000).toISOString() },
    });
  });
  await recoveryPage.reload();
  await recoveryPage
    .getByRole("heading", { name: "Your sample session has ended" })
    .waitFor();
  await recoveryPage.unroute("**/api/v1/demo/session");
  await recovery.clearCookies();
  await recoveryPage
    .getByRole("button", { name: "Start a new sample session" })
    .click();
  await recoveryPage
    .getByRole("button", { name: "run agent session", exact: true })
    .waitFor();
  await recoveryPage.screenshot({
    path: `${out}/session-recovered.png`,
    fullPage: true,
  });
  await recovery.close();
  assert.deepEqual(native, [], "demo UI never calls native routes");
  for (const name of [
    "demo_entered",
    "first_diff",
    "first_merge",
    "first_undo",
  ])
    assert.ok(
      events.some((e) => e.event === name),
      name,
    );
  assert.ok(
    events.every((e) => Object.keys(e).join() === "event"),
    "no identifiers in event payloads",
  );
  assert.deepEqual(errors, []);
  console.log(
    "PASS: real same-pin scenario, conflict review/explicit merge, immutable bounded undo restores49, delayed-review safety, session expiry/cookie-loss/deeplink recovery, demo native gating, opt-in events, desktop/mobile rendering",
  );
} finally {
  await browser.close();
}
