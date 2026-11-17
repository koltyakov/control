// Optional browser verification. All state, credentials, and processes belong to
// this fixture. No saved Control login, real fleet, or desktop profile is used.
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { createHash } from "node:crypto";
import { mkdtemp, writeFile, mkdir, readFile } from "node:fs/promises";
import { createServer } from "node:net";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { chromium } from "playwright";

const root = fileURLToPath(new URL("../..", import.meta.url));
const binary =
  process.env.CONTROL_CONSOLE_TEST_BINARY ||
  path.join(
    root,
    "bin",
    process.platform === "win32" ? "control.exe" : "control",
  );
const dir = await mkdtemp(path.join(tmpdir(), "control-console-smoke-"));
const superkey = "isolated-console-test-superuser-credential";
const password = "isolated browser password";
const processes = [];
const logs = [];
let browser;

async function freePort() {
  const server = createServer();
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  const port = server.address().port;
  await new Promise((resolve) => server.close(resolve));
  return port;
}
function start(args, env = {}) {
  const child = spawn(binary, args, {
    cwd: dir,
    env: {
      ...process.env,
      CONTROL_TOKEN: "",
      CONTROL_USER_KEY: "",
      CONTROL_SUPERUSER_KEY: "",
      CONTROL_HOME: path.join(dir, "home"),
      CONTROL_CONFIG: path.join(dir, "unused.json"),
      CONTROL_RELEASE_TOKEN: "",
      CONTROL_RELEASE_REPO: "",
      ...env,
    },
    stdio: ["ignore", "pipe", "pipe"],
  });
  const exited = new Promise((resolve) => {
    child.once("exit", resolve);
    child.once("error", (error) => {
      logs.push(error.message);
      resolve();
    });
  });
  processes.push({ child, exited });
  child.stdout.on("data", (data) => logs.push(data.toString()));
  child.stderr.on("data", (data) => logs.push(data.toString()));
}
async function until(check, message) {
  const deadline = Date.now() + 30000;
  while (Date.now() < deadline) {
    if (await check()) return;
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
  throw new Error(message);
}

async function assertGatewayProxySetup(page) {
  let setup = {
    supported: true, enabled: false,
    interfaces: [
      { name: "Ethernet <test>", address: "192.168.10.5", prefix: "192.168.10.0/24" },
      { name: "VPN", address: "fd00::5", prefix: "fd00::/64" },
    ],
    dependents: { invitations: 0, children: 0, retiring: 0, machines: 0 },
  };
  const writes = [];
  let loseReply = false;
  const endpoint = "**/console/api/fleet/nodes/*/proxy";
  await page.route(endpoint, async (route) => {
    const request = route.request();
    if (request.method() === "PUT") {
      const value = request.postDataJSON();
      writes.push(value);
      setup = { ...setup, enabled: value.enabled, settings: value.enabled ? value : setup.settings };
      if (loseReply) {
        loseReply = false;
        await route.abort("failed");
        return;
      }
    }
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(setup) });
  });
  const open = async () => {
    await page.getByRole("button", { name: "Actions for browser-worker", exact: true }).click();
    await page.locator('#machine-menu [data-action="worker-proxy"]').click();
    await page.locator('#modal-form input[name="enabled"]').waitFor();
  };
  try {
    await open();
    assert.equal(writes.length, 0, "Opening proxy settings must not enable a listener");
    assert.equal(await page.locator('#modal-form input[name="enabled"]').isChecked(), false);
    assert.ok(await page.locator("#proxy-settings-fields").isHidden());
    await page.locator('#modal-form input[name="enabled"]').check();
    await page.locator('#modal-form select[name="interfaceAddress"]').selectOption("0");
    assert.equal(await page.locator('#modal-form textarea[name="sources"]').inputValue(), "192.168.10.0/24");
    assert.equal(await page.locator("#modal-form img").count(), 0, "Interface labels must not become markup");
    await page.locator('#modal-form input[name="port"]').fill("4321");
    await page.locator('#modal-form textarea[name="sources"]').fill("192.168.10.20\n192.168.10.32/28");
    await page.locator("#modal-form button[type=submit]").click();
    await page.locator("#dialog").waitFor({ state: "hidden" });
    assert.deepEqual(writes, [{
      enabled: true, interface: "Ethernet <test>", address: "192.168.10.5",
      port: 4321, sources: ["192.168.10.20/32", "192.168.10.32/28"],
    }], "Enabling must submit the exact chosen interface, address and sources");

    setup.dependents.children = 2;
    setup.dependents.machines = 1;
    setup.dependents.retiring = 1;
    await open();
    assert.ok(await page.locator('#modal-form input[name="enabled"]').isDisabled());
    assert.ok(await page.locator("#modal-form button[type=submit]").isDisabled());
    await page.getByText("Move or remove these dependencies", { exact: false }).waitFor();
    await page.getByText("In use by 1 machine.", { exact: false }).waitFor();
    await page.getByText("1 removed context has not yet acknowledged removal.", { exact: false }).waitFor();
    assert.ok((await page.locator("#modal-form").textContent()).includes("control machines proxies release CHILD_ID"));
    assert.ok(!(await page.locator("#modal-form").textContent()).includes("retiring"));
    await page.locator('#modal-form [data-action="close"]').click();
    assert.equal(writes.length, 1, "A dependent child must block disabling or reconfiguring the proxy");

    setup.dependents.children = 0;
    setup.dependents.machines = 0;
    setup.dependents.retiring = 0;
    await open();
    assert.ok(await page.locator('#modal-form input[name="enabled"]').isChecked(), "Zero dependents must not disable the explicit setting");
    await page.locator('#modal-form input[name="enabled"]').uncheck();
    await page.locator("#modal-form button[type=submit]").click();
    await page.locator("#dialog").waitFor({ state: "hidden" });
    assert.deepEqual(writes.at(-1), { enabled: false }, "Disabling must be an explicit mutation");

    setup.enabled = true;
    await open();
    await page.locator('#modal-form input[name="enabled"]').uncheck();
    loseReply = true;
    await page.locator("#modal-form button[type=submit]").click();
    await until(async () => (await page.locator("#modal-form .form-error").textContent()).length > 0, "Uncertain proxy write was not reported");
    const attempted = writes.length;
    await page.locator("#modal-form button[type=submit]").click();
    await page.getByText("Reload settings to check whether the previous change", { exact: false }).waitFor();
    assert.equal(writes.length, attempted, "An uncertain proxy write must not be replayed");
    await page.getByRole("button", { name: "Reload settings", exact: true }).click();
    await until(async () => !(await page.locator('#modal-form input[name="enabled"]').isChecked()), "Reload did not inspect the applied setting");
    await page.locator('#modal-form [data-action="close"]').click();

    setup.supported = false;
    await page.getByRole("button", { name: "Actions for browser-worker", exact: true }).click();
    await page.locator('#machine-menu [data-action="worker-proxy"]').click();
    await page.getByText("This machine does not allow gateway proxy management", { exact: false }).waitFor();
    assert.equal(await page.locator('#modal-form input[name="enabled"]').count(), 0);
    await page.getByRole("button", { name: "Close", exact: true }).click();

    let releaseRead, completeRead;
    const gate = new Promise((resolve) => { releaseRead = resolve; }),
      completed = new Promise((resolve) => { completeRead = resolve; });
    await page.route(endpoint, async (route) => {
      await gate;
      await route.fulfill({ json: { ...setup, supported: true } });
      completeRead();
    });
    try {
      await page.getByRole("button", { name: "Actions for browser-worker", exact: true }).click();
      await page.locator('#machine-menu [data-action="worker-proxy"]').click();
      await page.locator("#proxy-settings-loading").waitFor();
      await page.keyboard.press("Escape");
      releaseRead();
      await completed;
      await page.evaluate(() => new Promise(requestAnimationFrame));
      assert.equal(await page.locator("#dialog").isVisible(), false, "A cancelled settings read must not reopen the dialog");
    } finally {
      releaseRead();
    }
  } finally {
    await page.unroute(endpoint);
  }
}
const gatewayPort = await freePort();
const nodePort = await freePort();
const base = "http://127.0.0.1:" + gatewayPort;
async function api(route, method = "GET", body, key = superkey) {
  const response = await fetch(base + route, {
    method,
    headers: {
      Authorization: "Bearer " + key,
      "Content-Type": "application/json",
    },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  assert.ok(
    response.ok,
    `${method} ${route}: ${response.status} ${await response.clone().text()}`,
  );
  return response.status === 204 ? null : response.json();
}
async function publishInstallers() {
  const software = (await api("/v1/admin/updates")).gateway;
  const data = Buffer.from("isolated browser installation fixture");
  const sha256 = createHash("sha256").update(data).digest("hex");
  const uploaded = await fetch(base + "/v1/admin/updates/blobs/" + sha256, {
    method: "PUT",
    headers: {
      Authorization: "Bearer " + superkey,
      "Content-Type": "application/octet-stream",
    },
    body: data,
  });
  assert.equal(uploaded.status, 204);
  await api("/v1/admin/updates", "POST", {
    // Matching running versions skip staging/restarts. These pinned test bytes
    // are only used to render installation commands, never executed.
    version: software.version,
    createdAt: new Date().toISOString(),
    assets: ["amd64", "arm64"].map((arch) => ({
      os: software.os,
      arch,
      file: `control_${software.os}_${arch}${software.os === "windows" ? ".exe" : ""}`,
      size: data.length,
      sha256,
    })),
  });
  return software.os;
}
async function login(page, username, pw = password) {
  await page.goto(base);
  await page.locator("#login-form input[name=username]").fill(username);
  await page.locator("#login-form input[name=password]").fill(pw);
  await page.getByRole("button", { name: "Sign in", exact: false }).click();
  await page.locator("#app").waitFor({ state: "visible" });
}

// Hold a real browser request until the test chooses its response order.
async function holdRequest(page, pattern, method) {
  let received;
  const pending = new Promise((resolve) => { received = resolve; });
  const handler = (route) => {
    if (route.request().method() === method) received(route);
    else return route.fallback();
  };
  await page.route(pattern, handler);
  return {
    pending,
    remove: () => page.unroute(pattern, handler),
  };
}

async function assertAsyncConsoleOwnership(browser) {
  const context = await browser.newContext();
  const page = await context.newPage();
  const errors = [];
  page.on("pageerror", (error) => errors.push(error.message));
  const keys = [{ id: "async-key", name: "Async fixture key", role: "common", createdAt: new Date().toISOString() }];
  await page.route("**/console/api/fleet/keys", (route) =>
    route.request().method() === "GET" ? route.fulfill({ json: keys }) : route.fallback());
  try {
    await login(page, "browser-owner");
    await pageLink(page, "keys");
    await page.getByText("Async fixture key", { exact: true }).waitFor();
    for (const status of [204, 409]) {
      const held = await holdRequest(page, "**/console/api/fleet/keys/async-key", "PATCH");
      await page.getByRole("button", { name: "Rename", exact: true }).click();
      const submitted = await page.locator("#modal-form button[type=submit]").elementHandle();
      await submitted.click();
      const request = await held.pending;
      await page.locator("#modal-form [data-action=close]").click();
      await page.getByRole("button", { name: "Create key", exact: false }).click();
      await request.fulfill({ status, body: status === 409 ? "name has an existing invitation" : undefined });
      await page.waitForFunction((button) => !button.disabled, submitted);
      assert.equal(await page.locator("#dialog-title").textContent(), "Create fleet key");
      assert.equal(await page.locator("#modal-form .form-error").textContent(), "");
      assert.equal(await page.locator("#force-confirm").count(), 0);
      await page.locator("#modal-form [data-action=close]").click();
      await held.remove();
    }

    for (const outcome of ["success", "expiry", "logout", "uncertain"]) {
      if (outcome === "logout" || outcome === "uncertain") {
        if (outcome === "uncertain") await login(page, "browser-owner");
        await pageLink(page, "keys");
      }
      const held = await holdRequest(page, "**/console/api/fleet/keys", "POST");
      await page.getByRole("button", { name: "Create key", exact: false }).click();
      await page.locator("#modal-form input[name=name]").fill("Private pending name");
      const submitted = await page.locator("#modal-form button[type=submit]").elementHandle();
      await submitted.click();
      const request = await held.pending;
      assert.equal(await page.locator("#modal-form [data-action=close]").isDisabled(), true);
      await page.keyboard.press("Escape");
      await page.getByRole("button", { name: "Close dialog", exact: true }).click();
      assert.equal(await page.locator("#dialog").isVisible(), true, "Pending issuance must resist dismissal");
      if (outcome === "expiry") {
        await page.waitForFunction(() => !state.busy);
        await page.route("**/console/api/status", (route) => route.fulfill({ status: 401, body: "session expired" }));
        await page.evaluate(() => refresh(true));
        await page.locator("#login").waitFor({ state: "visible" });
        await page.unroute("**/console/api/status");
        // Sign in without navigation so the old response still reaches its callback.
        await page.locator("#login-form input[name=username]").fill("browser-owner");
        await page.locator("#login-form input[name=password]").fill(password);
        await page.getByRole("button", { name: "Sign in", exact: false }).click();
        await page.locator("#app").waitFor({ state: "visible" });
      } else if (outcome === "logout") {
        // Another session transition may occur while a credential reply is in flight.
        await page.evaluate(async () => { await api("logout", "POST"); showLogin(); });
      }
      if (outcome === "uncertain") await request.abort("failed");
      else await request.fulfill({ json: { token: "ctl_async_private_token" } });
      await page.waitForFunction((button) => !button.disabled, submitted);
      if (outcome === "success") {
        assert.equal(await page.locator("#secret-value").textContent(), "ctl_async_private_token");
        await page.getByRole("button", { name: "Done", exact: true }).click();
      } else if (outcome === "uncertain") {
        assert.match(await page.locator("#modal-form .form-error").textContent(), /may have completed/);
        assert.equal(await page.locator("#secret-value").count(), 0);
        await page.locator("#modal-form [data-action=close]").click();
      } else {
        assert.equal(await page.locator("#dialog").isVisible(), false);
        assert.equal(await page.locator("#secret-value").count(), 0);
        assert.ok(!(await page.locator("body").textContent()).includes("Private pending name"));
      }
      await held.remove();
    }
    assert.deepEqual(errors, []);
  } finally {
    await context.close();
  }
}

async function assertAuditRequestOwnership(browser) {
  const context = await browser.newContext();
  const page = await context.newPage();
  let newest = 100;
  const event = (id, category = "key") => ({
    id, at: new Date().toISOString(), userId: "fixture", actor: "fixture",
    via: "browser", action: `${category}.create`, target: `${category}-${id}`,
  });
  await page.route("**/console/api/audit?*", (route) => {
    const query = new URL(route.request().url()).searchParams;
    const category = query.get("category") || "key";
    const top = query.has("before") ? Number(query.get("before")) - 1 : newest;
    const count = Math.min(100, top);
    return route.fulfill({ json: {
      events: Array.from({ length: count }, (_, i) => event(top - i, category)),
      next: top > 100 ? top - 99 : 0,
    } });
  });
  try {
    await login(page, "browser-owner");
    await pageLink(page, "activity");
    await page.getByText("key-100", { exact: true }).waitFor();
    newest = 250;
    await page.evaluate(() => refresh(true));
    assert.equal(await page.locator("tbody tr").count(), 100);
    await page.getByRole("button", { name: "Load older", exact: true }).click();
    await page.getByText("key-150", { exact: true }).waitFor();
    await page.getByRole("button", { name: "Load older", exact: true }).click();
    await page.getByText("key-1", { exact: true }).waitFor();
    assert.equal(await page.locator("tbody tr").count(), 250, "Burst reset must leave every event reachable");

    const held = await holdRequest(page, "**/console/api/audit?*", "GET");
    await page.evaluate(() => { void refresh(true); });
    const stale = await held.pending;
    await page.locator("#audit-category").selectOption("machine");
    await held.remove();
    await stale.fulfill({ json: { events: [event(999)], next: 0 } });
    await page.getByText("machine-250", { exact: true }).waitFor();
    assert.equal(await page.getByText("key-999", { exact: true }).count(), 0);

    const olderHeld = await holdRequest(page, "**/console/api/audit?*", "GET");
    const olderButton = await page.getByRole("button", { name: "Load older", exact: true }).elementHandle();
    await olderButton.click();
    const older = await olderHeld.pending;
    await olderHeld.remove();
    await page.locator("#audit-category").selectOption("key");
    await page.getByText("key-250", { exact: true }).waitFor();
    await older.fulfill({ json: { events: [event(149, "machine")], next: 149 } });
    await page.waitForFunction((button) => !button.disabled, olderButton);
    assert.equal(await page.getByText("machine-149", { exact: true }).count(), 0);
  } finally {
    await context.close();
  }
}
async function pageLink(page, hash) {
  await page.locator(`nav a[href="#${hash}"]`).click();
  await page.waitForFunction(
    (expected) =>
      document.querySelector("#page-title").textContent === expected,
    {
      machines: "Machines",
      keys: "Access keys",
      activity: "Activity",
      invitations: "Invitations",
      users: "Accounts",
      updates: "Updates",
    }[hash],
  );
}

async function refreshPage(page) {
  await page.evaluate(async () => {
    // Wait and request in the same browser turn so the polling timer cannot
    // start between a separate idle check and a manual refresh.
    const deadline = Date.now() + 10000;
    while (state.busy) {
      if (Date.now() >= deadline) throw new Error("Console refresh did not finish");
      await new Promise((resolve) => setTimeout(resolve, 10));
    }
    await refresh(true);
  });
}

async function assertOverviewFullFleet(page, snapshot) {
  const online = Array.from({ length: 12 }, (_, i) => ({
    ...snapshot.nodes[0],
    id: `full-fleet-${i}`,
    name: `online-host-${String(i + 1).padStart(2, "0")}`,
    online: true,
    disabled: false,
    status: "summary",
    runAs: "user",
    hybridId: i === 11 ? "full-fleet-pair" : undefined,
    hybridRole: i === 11 ? "user" : undefined,
  }));
  const nodes = [
    ...online,
    { ...online[11], id: "full-fleet-system", name: "paired-system", hybridRole: "system", runAs: "system" },
    { ...online[0], id: "full-fleet-offline", name: "offline-host", online: false, status: "offline" },
    { ...online[0], id: "full-fleet-disabled", name: "disabled-host", disabled: true },
  ];
  await page.route("**/console/api/status", (route) => route.fulfill({ json: { ...snapshot, nodes } }));
  try {
    await pageLink(page, "machines");
    await refreshPage(page);
    await page.locator("#machine-search").fill("no-matches");
    await page.locator('[data-action="filter-state"][data-filter="offline"]').click();
    await page.locator('.brand[href="#overview"]').click();
    await page.getByRole("button", { name: "online-host-12", exact: true }).waitFor();
    const names = page.locator(".machine-table .machine-name");
    const expected = online.map((node) => node.name);
    assert.deepEqual((await names.allTextContents()).sort(), expected, "Overview must show every online enabled host, even beyond eight and after Machines filters");
    assert.equal(await page.locator("#nav-count").textContent(), "14", "Hybrid contexts must count as one host");
    await page.locator('[data-action="sort"][data-sort="name"]').click();
    assert.deepEqual((await names.allTextContents()).sort(), expected, "Sorting must not hide hosts");
    await page.setViewportSize({ width: 390, height: 844 });
    await page.getByRole("button", { name: "online-host-12", exact: true }).scrollIntoViewIfNeeded();
    assert.equal(await names.count(), 12, "Narrow layouts must retain every online enabled host");
    await page.locator('.panel-heading [data-action="filter-machines"][data-filter="all"]').click();
    await page.locator("#machine-search").waitFor();
    assert.equal(await page.locator("#machine-search").inputValue(), "");
    assert.equal(await names.count(), 14, "All machines must retain offline and disabled hosts without duplicating Hybrid contexts");
    assert.equal(await page.locator('[data-action="filter-state"][aria-pressed="true"]').getAttribute("data-filter"), "all");
  } finally {
    await page.setViewportSize({ width: 1440, height: 1000 });
    await page.unroute("**/console/api/status");
    await page.locator('.brand[href="#overview"]').click();
    await refreshPage(page);
  }
}

async function assertCustomMachineOrder(page, snapshot) {
  await page.evaluate(() => {
    state.sort = "name";
    state.descending = false;
  });
  const baseNode = { ...snapshot.nodes[0], online: true, disabled: false, status: "summary" },
    alpha = { ...baseNode, id: "order-alpha", name: "alpha" },
    pair = { ...baseNode, id: "order-user", name: "beta-pair", hybridId: "order-pair", hybridRole: "user" },
    companion = { ...pair, id: "order-system", name: "beta-pair-system", hybridRole: "system" },
    offline = { ...baseNode, id: "order-offline", name: "offline", online: false, status: "offline" },
    disabled = { ...baseNode, id: "order-disabled", name: "disabled", disabled: true };
  let nodes = [offline, companion, alpha, disabled, pair];
  await page.route("**/console/api/status", (route) => route.fulfill({ json: { ...snapshot, nodes } }));
  const names = page.locator(".machine-table .machine-name"),
    dialog = page.locator("#dialog");
  const drag = async (name, target, after = false, cancel = false, duringDrag) => {
    const handle = page.getByRole("button", { name: "Reorder " + name, exact: true }),
      targetRow = page.locator(".machine-table tbody tr").filter({
        has: page.getByRole("button", { name: target, exact: true }),
      });
    await handle.scrollIntoViewIfNeeded();
    const start = await handle.boundingBox(),
      end = await targetRow.boundingBox(),
      source = await handle.locator("xpath=ancestor::tr").boundingBox(),
      keys = await page.locator(".machine-table tbody tr").evaluateAll((rows) =>
        rows.map((row) => row.dataset.orderKey)),
      sourceKey = await handle.getAttribute("data-order-key"),
      targetKey = await targetRow.getAttribute("data-order-key"),
      remaining = keys.filter((key) => key !== sourceKey),
      position = remaining.indexOf(targetKey) + Number(after) + 1;
    assert.ok(start && end);
    await page.mouse.move(start.x + start.width / 2, start.y + start.height / 2);
    await page.mouse.down();
    await page.mouse.move(start.x + start.width / 2, end.y + end.height * (after ? 0.8 : 0.2), { steps: 8 });
    assert.equal(await page.locator(".machine-drag-preview").count(), 1);
    const placeholder = page.locator(".machine-drop-placeholder"),
      gap = await placeholder.boundingBox(),
      shiftedTarget = await targetRow.boundingBox();
    assert.ok(gap && source && shiftedTarget, "Dragging must open a visible insertion slot");
    assert.ok(Math.abs(gap.height - source.height) < 2, "The gap must reserve the dragged row's full height");
    assert.ok(gap.width >= shiftedTarget.width - 2, "The drop slot must span the table width");
    assert.equal(await placeholder.getAttribute("data-position"), String(position));
    assert.equal(await placeholder.locator("strong").textContent(), "Drop " + name + " here");
    assert.equal(await placeholder.locator("small").textContent(), `Position ${position} of ${keys.length}`);
    assert.ok(Math.abs(after ? gap.y - shiftedTarget.y - shiftedTarget.height :
      gap.y + gap.height - shiftedTarget.y) < 2, "Rows must shift to show the exact landing position");
    await page.evaluate(() => new Promise((resolve) =>
      requestAnimationFrame(() => requestAnimationFrame(resolve))));
    assert.deepEqual(await placeholder.boundingBox(), gap, "The insertion slot must not oscillate under a stationary pointer");
    if (duringDrag) await duringDrag();
    if (cancel) await page.keyboard.press("Escape");
    await page.mouse.up();
    assert.equal(await page.locator(".machine-drag-preview").count(), 0);
    assert.equal(await placeholder.count(), 0, "Drop or cancellation must remove the insertion slot");
    assert.equal(await page.locator('.machine-table tr[style*="transform"]').count(), 0, "Temporary row shifts must be cleared");
    assert.equal(await dialog.isVisible(), false, "Dragging an icon must not open machine details");
    return gap;
  };
  try {
    await pageLink(page, "machines");
    await refreshPage(page);
    assert.equal(await page.getByRole("button", { name: "Custom order", exact: true }).count(), 0, "Direct dragging must not require an ordering dialog");
    assert.deepEqual(await names.allTextContents(), ["alpha", "beta-pair", "disabled", "offline"]);
    await page.getByRole("button", { name: "Reorder alpha", exact: true }).click();
    assert.equal(await dialog.isVisible(), false, "Clicking an icon must not open details or reorder");
    assert.equal(await page.evaluate(() => localStorage.getItem(machineOrderStorageKey())), null);
    await drag("beta-pair", "alpha", false, true);
    assert.deepEqual(await names.allTextContents(), ["alpha", "beta-pair", "disabled", "offline"], "Escape must cancel without saving");
    const landing = await drag("beta-pair", "alpha", false, false, async () => {
      await refreshPage(page);
      assert.equal(await page.locator(".machine-dragging").count(), 1, "Polling must preserve the captured row");
      assert.deepEqual(await names.allTextContents(), ["alpha", "beta-pair", "disabled", "offline"], "Reordering must save only on drop");
      if (process.env.CONTROL_CONSOLE_SCREENSHOTS) {
        await mkdir(process.env.CONTROL_CONSOLE_SCREENSHOTS, { recursive: true });
        await page.screenshot({ path: path.join(process.env.CONTROL_CONSOLE_SCREENSHOTS, "machine-drop-desktop.png") });
      }
    });
    assert.deepEqual(await names.allTextContents(), ["beta-pair", "alpha", "disabled", "offline"]);
    const landed = await page.locator(".machine-table tbody tr").first().boundingBox();
    assert.ok(Math.abs(landed.y - landing.y) < 2, "The committed row must land exactly in the previewed slot");
    await drag("beta-pair", "offline", true);
    assert.equal(await names.last().textContent(), "beta-pair", "Dropping after the last row must append");
    await drag("beta-pair", "alpha");
    const savedBeforeOutside = await page.evaluate(() => [...state.machineOrder]);
    const outsideHandle = page.getByRole("button", { name: "Reorder beta-pair", exact: true }),
      outsideStart = await outsideHandle.boundingBox();
    await page.mouse.move(outsideStart.x + 10, outsideStart.y + 10);
    await page.mouse.down();
    await page.mouse.move(outsideStart.x + 10, 20, { steps: 8 });
    assert.equal(await page.locator(".machine-drop-placeholder").isVisible(), false, "Outside the list there must be no valid drop slot");
    await page.mouse.up();
    assert.deepEqual(await page.evaluate(() => state.machineOrder), savedBeforeOutside, "Dropping outside must not change the order");
    await page.mouse.move(outsideStart.x + 10, outsideStart.y + 10);
    await page.mouse.down();
    await page.mouse.move(outsideStart.x + 10, outsideStart.y + 90, { steps: 8 });
    await page.setViewportSize({ width: 1300, height: 950 });
    await page.waitForFunction(() => machineDrag === null);
    await page.mouse.up();
    assert.equal(await page.locator(".machine-drop-placeholder").count(), 0, "Resizing must cancel cached row geometry and clear the slot");
    assert.deepEqual(await page.evaluate(() => state.machineOrder), savedBeforeOutside);
    await page.setViewportSize({ width: 1440, height: 1000 });
    await page.locator('.context-choice[data-id="order-system"]').click();
    assert.equal(await page.locator(".machine-table tbody tr").first().getAttribute("data-id"), "order-system", "Chip changes must not move Hybrid hosts");
    await page.locator('.brand[href="#overview"]').click();
    await page.locator(".gateway-card").waitFor();
    assert.deepEqual(await names.allTextContents(), ["beta-pair", "alpha"], "Overview must use the same order without showing offline or disabled hosts");
    await page.locator('[data-action="sort"][data-sort="name"]').click();
    assert.deepEqual(await names.allTextContents(), ["alpha", "beta-pair"], "Column sorting must still work");
    await drag("beta-pair", "alpha");
    assert.deepEqual(await names.allTextContents(), ["beta-pair", "alpha"], "Dragging must replace the active column sort");

    // IDs, not display names or selected contexts, determine saved positions.
    nodes = [{ ...alpha, name: "aardvark-renamed" }, companion, pair,
      { ...baseNode, id: "order-new-z", name: "z-new" },
      { ...baseNode, id: "order-new-a", name: "a-new" }];
    await refreshPage(page);
    assert.deepEqual(await names.allTextContents(), ["beta-pair", "aardvark-renamed", "a-new", "z-new"], "Renames must retain positions, removed hosts must disappear and new hosts must append alphabetically");
    await page.reload();
    await page.getByRole("button", { name: "beta-pair", exact: true }).waitFor();
    assert.deepEqual(await names.allTextContents(), ["beta-pair", "aardvark-renamed", "a-new", "z-new"], "Reload must restore the saved order");
    await page.locator("#account-button").click();
    await page.locator('#account-menu [data-action="logout"]').click();
    await page.locator("#modal-form button[type=submit]").click();
    await page.locator("#login").waitFor({ state: "visible" });
    assert.deepEqual(await page.evaluate(() => state.machineOrder), [], "Sign-out must clear the in-memory fleet preference");
    await login(page, "browser-owner");
    await page.getByRole("button", { name: "beta-pair", exact: true }).waitFor();
    assert.deepEqual(await names.allTextContents(), ["beta-pair", "aardvark-renamed", "a-new", "z-new"], "Sign-in must restore this fleet's saved order");

    const scoped = await page.evaluate(() => {
      const original = state.session,
        originalKey = machineOrderStorageKey(),
        saved = loadMachineOrder();
      try {
        state.session = { ...original, userId: "another-fleet" };
        const other = loadMachineOrder();
        state.session = { ...original, role: "superuser" };
        return { saved, other, operator: loadMachineOrder(), separateKey: originalKey !== machineOrderStorageKey() };
      } finally {
        state.session = original;
      }
    });
    assert.deepEqual(scoped.saved, ["hybrid:order-pair", "node:order-alpha", "node:order-disabled", "node:order-offline"]);
    assert.deepEqual(scoped.other, [], "Another fleet must not inherit an order");
    assert.deepEqual(scoped.operator, [], "Operator and account order must remain separate");
    assert.equal(scoped.separateKey, true);

    // A denied storage write must leave the prior order intact.
    await page.evaluate(() => {
      window.fixtureStorageSetItem = Storage.prototype.setItem;
      Storage.prototype.setItem = () => { throw new DOMException("Fixture storage denied", "SecurityError"); };
    });
    try {
      await drag("beta-pair", "aardvark-renamed", true);
      await page.getByText("Browser storage is unavailable. The machine order was not saved.", { exact: true }).waitFor();
      assert.equal(await names.first().textContent(), "beta-pair");
    } finally {
      await page.evaluate(() => {
        Storage.prototype.setItem = window.fixtureStorageSetItem;
        delete window.fixtureStorageSetItem;
      });
    }

    await page.getByRole("button", { name: "Reorder beta-pair", exact: true }).focus();
    await page.keyboard.press("Alt+ArrowDown");
    assert.deepEqual(await names.allTextContents(), ["aardvark-renamed", "beta-pair", "a-new", "z-new"]);
    assert.equal(await page.locator(":focus").getAttribute("aria-label"), "Reorder beta-pair", "Keyboard focus must follow the moved host");
    await refreshPage(page);
    assert.equal(await page.locator(":focus").getAttribute("aria-label"), "Reorder beta-pair", "Polling must retain focus on the correct handle");
    await page.keyboard.press("Alt+ArrowUp");
    assert.equal(await names.first().textContent(), "beta-pair");

    // On a filtered view, only visible slots move. Hidden hosts retain theirs.
    await pageLink(page, "machines");
    await page.locator("#machine-search").fill("new");
    await drag("z-new", "a-new");
    await page.locator("#machine-search").fill("");
    assert.deepEqual(await names.allTextContents(), ["beta-pair", "aardvark-renamed", "z-new", "a-new"]);
    await drag("a-new", "z-new");

    await page.setViewportSize({ width: 390, height: 844 });
    await drag("beta-pair", "aardvark-renamed", true, false, async () => {
      if (process.env.CONTROL_CONSOLE_SCREENSHOTS)
        await page.screenshot({ path: path.join(process.env.CONTROL_CONSOLE_SCREENSHOTS, "machine-drop-mobile.png") });
    });
    assert.deepEqual(await names.allTextContents(), ["aardvark-renamed", "beta-pair", "a-new", "z-new"], "Icon dragging must also work in the stacked mobile layout");
    assert.ok(await page.locator("#app").evaluate((element) => element.scrollWidth <= element.clientWidth));
    const touch = await page.context().newCDPSession(page);
    try {
      const handle = page.getByRole("button", { name: "Reorder beta-pair", exact: true });
      await handle.scrollIntoViewIfNeeded();
      const start = await handle.boundingBox(),
        end = await page.locator(".machine-table tbody tr").first().boundingBox(),
        point = { x: start.x + start.width / 2, y: start.y + start.height / 2 };
      await touch.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [point] });
      await touch.send("Input.dispatchTouchEvent", { type: "touchMove", touchPoints: [{ x: point.x, y: end.y + 10 }] });
      await touch.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] });
      assert.deepEqual(await names.allTextContents(), ["beta-pair", "aardvark-renamed", "a-new", "z-new"], "Touch dragging must save on release");
      assert.equal(await dialog.isVisible(), false);
    } finally {
      await touch.detach();
    }

    await page.setViewportSize({ width: 1440, height: 1000 });
    const previousNodes = nodes;
    nodes = [...nodes, ...Array.from({ length: 24 }, (_, i) => ({
      ...baseNode, id: "order-scroll-" + i, name: "scroll-" + String(i).padStart(2, "0"),
    }))];
    await refreshPage(page);
    const scrollHandle = page.getByRole("button", { name: "Reorder beta-pair", exact: true });
    await scrollHandle.scrollIntoViewIfNeeded();
    const start = await scrollHandle.boundingBox(),
      scrollBox = await page.locator(".content-scroll").boundingBox(),
      beforeScroll = await page.locator(".content-scroll").evaluate((element) => element.scrollTop);
    await page.mouse.move(start.x + start.width / 2, start.y + start.height / 2);
    await page.mouse.down();
    await page.mouse.move(start.x + start.width / 2, scrollBox.y + scrollBox.height - 8, { steps: 8 });
    await page.waitForFunction((before) => document.querySelector(".content-scroll").scrollTop > before + 80, beforeScroll);
    await page.keyboard.press("Escape");
    await page.mouse.up();
    assert.equal(await page.locator(".machine-drag-preview").count(), 0, "Cancelling must stop the auto-scroll drag and remove its preview");
    nodes = previousNodes;
    await refreshPage(page);

    const invalid = await page.evaluate(() => {
      const key = machineOrderStorageKey();
      return ["broken JSON", "{}", "[null]", '["node:valid",42]', '["foreign:bad"]'].map((value) => {
        localStorage.setItem(key, value);
        const order = loadMachineOrder();
        localStorage.removeItem(key);
        return order;
      });
    });
    assert.ok(invalid.every((order) => order.length === 0), "Malformed preferences must fall back safely");
  } finally {
    await page.evaluate(() => {
      localStorage.removeItem(machineOrderStorageKey());
      state.machineOrder = [];
      state.sort = "name";
      state.descending = false;
      state.contexts.clear();
    });
    if (await dialog.isVisible()) await page.keyboard.press("Escape");
    await page.setViewportSize({ width: 1440, height: 1000 });
    await page.unroute("**/console/api/status");
    await page.locator('.brand[href="#overview"]').click();
    await refreshPage(page);
  }
}

async function assertContextChips(page) {
  assert.equal(await page.locator(".context-presence").count(), 0, "Context chips must not contain status dots");
  const chips = await page.locator(".machine-table .context-tag").evaluateAll(
    (elements) => elements.map((element) => {
      const computed = getComputedStyle(element);
      const properties = [
        "backgroundColor", "color", "borderTopColor", "borderTopWidth",
        "borderTopStyle", "borderRadius", "height", "paddingLeft",
        "paddingRight", "fontSize", "fontWeight", "lineHeight", "whiteSpace",
      ];
      return {
        label: element.textContent,
        style: Object.fromEntries(properties.map((key) => [key, computed[key]])),
      };
    }),
  );
  const system = chips.find((chip) => chip.label === "System");
  const user = chips.find((chip) => chip.label === "User");
  assert.ok(system && user, "Both execution contexts should have chips");
  assert.deepEqual(system.style, user.style, "System and User chips should share the same outlined treatment");
  assert.equal(system.style.backgroundColor, "rgba(0, 0, 0, 0)");
  assert.equal(system.style.borderTopWidth, "1px");
  assert.equal(system.style.borderTopStyle, "solid");
  assert.equal(system.style.height, "19px");
  assert.equal(system.style.whiteSpace, "nowrap");
}

async function assertBrandAssets(page) {
  const source = await readFile(path.join(root, "internal/gateway/consoleweb/favicon-32.png"));
  const favicon = await page.request.get(base + "/console/static/favicon-32.png");
  assert.equal(favicon.ok(), true);
  assert.match(favicon.headers()["content-type"], /image\/png/);
  assert.deepEqual(await favicon.body(), source, "The favicon must match the generated artwork");
  assert.equal(await page.locator('link[rel="icon"][type="image/png"]').getAttribute("sizes"), "32x32");
  assert.equal(await page.locator('link[rel="apple-touch-icon"]').getAttribute("sizes"), "180x180");
  const dimensions = await page.evaluate(async () => {
    const paths = ["/console/static/favicon-32.png", "/console/static/apple-touch-icon.png", "/console/static/brand-icon.png"];
    return Promise.all(paths.map(async (path) => {
      const image = new Image();
      image.src = path;
      await image.decode();
      const canvas = document.createElement("canvas");
      canvas.width = image.naturalWidth;
      canvas.height = image.naturalHeight;
      const context = canvas.getContext("2d");
      context.drawImage(image, 0, 0);
      const { data } = context.getImageData(0, 0, canvas.width, canvas.height);
      let left = canvas.width, right = -1;
      for (let y = 0; y < canvas.height; y++) {
        for (let x = 0; x < canvas.width; x++) {
          if (data[(y * canvas.width + x) * 4 + 3] > 128) {
            left = Math.min(left, x);
            right = Math.max(right, x);
          }
        }
      }
      if (data[3] !== 0 || (right - left + 1) / canvas.width < 0.9) {
        throw new Error("Icons must have transparent corners and tightly cropped content: " + path);
      }
      return [image.naturalWidth, image.naturalHeight];
    }));
  });
  assert.deepEqual(dimensions, [[32, 32], [180, 180], [64, 64]]);
  const ico = await page.request.get(base + "/console/static/favicon.ico");
  assert.equal(ico.ok(), true);
  const bytes = await ico.body();
  assert.equal(bytes.readUInt16LE(2), 1);
  assert.equal(bytes.readUInt16LE(4), 3);
  assert.deepEqual([bytes[6], bytes[22], bytes[38]], [16, 32, 48]);
  const marks = await page.locator(".brand-mark").evaluateAll((elements) => elements.map((element) => ({
    image: getComputedStyle(element).backgroundImage,
    overlay: getComputedStyle(element, "::after").content,
  })));
  assert.equal(marks.length, 2);
  assert.ok(marks.every((mark) => mark.image.includes("/console/static/brand-icon.png") && mark.overlay === "none"));
}

async function assertResourceIcons(page, gateway = false) {
  const hasGPU = !gateway && await page.locator('.machine-table [data-sort="gpu"]').count() > 0;
  const labels = page.locator(
    gateway
      ? ".gateway-metrics .resource-label"
      : ".machine-table .sort-button:has(svg)",
  );
  assert.deepEqual(
    await labels.evaluateAll((elements) => elements.map((element) => {
      const svg = element.firstElementChild;
      return {
        label: element.childNodes[1].textContent,
        icon: svg.tagName,
        hidden: svg.getAttribute("aria-hidden"),
        width: getComputedStyle(svg).width,
        height: getComputedStyle(svg).height,
        visible: getComputedStyle(svg).opacity,
      };
    })),
    ["CPU", ...(hasGPU ? ["GPU/NPU"] : []), "Memory", gateway ? "State disk" : "Disk"].map((label) => ({
      label,
      icon: "svg",
      hidden: "true",
      width: "14px",
      height: "14px",
      visible: "1",
    })),
    "Resource labels should have leading decorative icons",
  );
}

async function assertStartupCommands(page, snapshot) {
  // Only commands are inspected. Never execute installers or register real OS services.
  let nodes, records = [], requests = [], conflict = false, uncertain = false;
  const attempts = [];
  const startupRoute = (route) => {
    const body = route.request().postDataJSON();
    attempts.push(body);
    return route.fulfill({ json: {
      status: nodes[0].os === "windows" && body.mode === "system" ? "accepted" : "manual",
      reason: "Local installation requires administrator permission",
    } });
  };
  const original = { ...snapshot.nodes[0], id: "startup-fixture", name: "startup-worker", runAs: "user", online: false };
  const statusRoute = (route) => route.fulfill({ json: { ...snapshot, nodes } });
  const installationsRoute = async (route) => {
    if (route.request().method() === "GET") return route.fulfill({ json: records });
    const body = route.request().postDataJSON();
    requests.push(body);
    if (uncertain) return route.abort();
    if (conflict && !body.force) return route.fulfill({ status: 409, body: "name has an existing invitation" });
    return route.fulfill({ json: {
      id: "startup-invitation",
      hybridId: body.serviceMode === "hybrid" ? "startup-invitation" : undefined,
      serviceMode: body.serviceMode,
      expiresAt: new Date(Date.now() + 900000).toISOString(),
      command: body.os === "windows"
        ? "irm 'https://example.test/install/fixture'|iex"
        : "curl -fsSL 'https://example.test/install/fixture' | " + (body.serviceMode === "system" ? "sudo bash" : "bash"),
    } });
  };
  await page.route("**/console/api/status", statusRoute);
  await page.route("**/console/api/fleet/nodes/*/startup", startupRoute);
  await page.route("**/console/api/fleet/installations", installationsRoute);
  const selectMode = async (mode) => {
    await page.locator('#modal-form select[name="serviceMode"]').locator("xpath=preceding-sibling::button[1]").click();
    await page.locator(`[role=option][data-value="${mode}"]`).click();
  };
  const refresh = () => refreshPage(page);
  const open = async (os, drawer = false) => {
    nodes = [{ ...original, os }];
    await refresh();
    await page.getByRole("button", { name: "Actions for startup-worker", exact: true }).click();
    if (drawer) {
      await page.getByRole("menuitem", { name: "View details", exact: true }).click();
      const actions = page.locator(".drawer-actions"),
        more = actions.getByRole("button", { name: "More actions for startup-worker", exact: true }),
        menu = page.getByRole("menu", { name: "Actions for startup-worker", exact: true });
      assert.equal(await actions.getByRole("button", { name: "Startup context", exact: true }).count(), 0);
      assert.equal(await actions.getByRole("button", { name: "Unregister", exact: true }).count(), 0);
      assert.equal(await actions.getByRole("button", { name: "Rename", exact: true }).isVisible(), true);
      assert.equal(await actions.getByRole("button", { name: "Disable", exact: true }).isVisible(), true);
      await more.press("ArrowDown");
      assert.deepEqual(await menu.getByRole("menuitem").allTextContents(), ["Startup context", "Gateway proxy", "Unregister"]);
      await refresh();
      assert.equal(await menu.isVisible(), true, "Polling must preserve drawer overflow menus");
      assert.equal(await more.getAttribute("aria-expanded"), "true");
      const box = await menu.boundingBox(), viewport = page.viewportSize();
      assert.ok(box.x >= 8 && box.y >= 8 && box.x + box.width <= viewport.width - 8 && box.y + box.height <= viewport.height - 8);
      await page.keyboard.press("End");
      assert.equal(await menu.getByRole("menuitem", { name: "Unregister", exact: true }).evaluate((element) => element === document.activeElement), true);
      await page.keyboard.press("Escape");
      assert.equal(await page.locator("dialog.machine-drawer").isVisible(), true, "Escape should close only the menu");
      assert.equal(await more.evaluate((element) => element === document.activeElement), true);
      await more.click();
      await menu.getByRole("menuitem", { name: "Startup context", exact: true }).click();
    } else await page.getByRole("menuitem", { name: "Startup context", exact: true }).click();
  };
  for (const os of ["windows", "linux", "darwin"]) {
    requests = [];
    if (os === "darwin") await page.setViewportSize({ width: 390, height: 844 });
    await open(os, os !== "linux");
    assert.equal(requests.length, 0, "Opening the dialog must not create an invitation");
    const config = os === "windows" ? "C:\\User's profile\\node.json" : "/home/user's profile/node.json";
    await page.getByLabel("Existing node configuration, optional").fill(config);
    if (os !== "windows") {
      // Reusing the User name cannot reserve a new root-owned identity.
      await page.getByLabel("New System routing name").fill(original.name);
      await page.locator("#modal-form button[type=submit]").click();
      await page.locator(".form-error").filter({ hasText: "unused System routing name" }).waitFor();
      assert.equal(requests.length, 0);
      await page.getByLabel("New System routing name").fill("startup-system");
    }
    await page.locator("#modal-form button[type=submit]").click();
    await page.locator("#secret-value").waitFor();
    const command = await page.locator("#secret-value").textContent();
    if (os === "windows") {
      assert.equal(command, "control --config 'C:\\User''s profile\\node.json' service start --mode auto");
      assert.equal(requests.length, 0, "Windows in-place migration needs no invitation");
      await refresh();
      assert.match(await page.locator("#startup-progress").textContent(), /Waiting/);
      nodes[0] = { ...nodes[0], online: true, runAs: "system" };
      await refresh();
      assert.match(await page.locator("#startup-progress").textContent(), /online in System context/);
    } else {
      assert.equal(command, "control --config '/home/user'\\''s profile/node.json' service uninstall --mode user &&\ncurl -fsSL 'https://example.test/install/fixture' | sudo bash");
      assert.equal(requests.length, 1);
      assert.equal(requests[0].name, "startup-system");
      assert.equal(requests[0].serviceMode, "system");
      assert.equal(requests[0].os, os);
      assert.match(await page.locator("#dialog-body").textContent(), /not migrated/);
    }
    await page.getByRole("button", { name: "Done", exact: true }).click();

    requests = [];
    await open(os);
    if (os === "linux") await page.getByLabel("New System routing name").fill("invalid hidden name");
    await selectMode("hybrid");
    assert.equal(await page.locator("#system-name-field").isVisible(), false);
    assert.equal(await page.locator('input[name="systemName"]').isDisabled(), true);
    await page.getByLabel("Existing node configuration, optional").fill(config);
    if (os === "linux") conflict = true;
    await page.locator("#modal-form button[type=submit]").click();
    if (conflict) {
      await page.locator("#force-confirm").check();
      assert.equal(requests.length, 1, "A conflict must not automatically retry");
      await page.locator("#modal-form button[type=submit]").click();
      assert.equal(requests.at(-1).force, true);
      conflict = false;
    }
    await page.locator("#secret-value").waitFor();
    assert.equal(requests.at(-1).name, original.name);
    assert.equal(requests.at(-1).serviceMode, "hybrid");
    const hybrid = await page.locator("#secret-value").textContent();
    assert.doesNotMatch(hybrid, /service uninstall|--mode hybrid/);
    assert.doesNotMatch(hybrid, /powershell\.exe|EncodedCommand|NoProfile -c/);
    assert.match(hybrid, /CONTROL_CONFIG/);
    assert.match(hybrid, /https:\/\/example.test\/install\/fixture/);
    if (os === "windows") {
      assert.match(hybrid, /C:\\User''s profile\\node.json/);
      assert.match(hybrid, /finally/);
    } else {
      assert.ok(hybrid.startsWith("CONTROL_CONFIG='/home/user'\\''s profile/node.json' bash -c '"));
      assert.doesNotMatch(hybrid, /sudo bash/);
    }
    records = [
      { id: "startup-invitation", redeemedId: original.id },
      { id: "system-invitation", hybridId: "startup-invitation", hybridRole: "system", redeemedId: "system-fixture" },
    ];
    nodes[0] = { ...nodes[0], online: true };
    await refresh();
    assert.match(await page.locator("#enrollment-progress").textContent(), /Waiting for both/);
    nodes.push({ ...original, id: "system-fixture", name: original.name + "-system", os, online: true });
    await refresh();
    assert.match(await page.locator("#enrollment-progress").textContent(), /Waiting for both/);
    nodes[1].runAs = "system";
    await refresh();
    assert.match(await page.locator("#enrollment-progress").textContent(), /online in User and System contexts/);
    await page.getByRole("button", { name: "Done", exact: true }).click();
    records = [];
    await page.setViewportSize({ width: 1440, height: 1000 });
  }
  await open("linux");
  await selectMode("hybrid");
  nodes.push({ ...original, id: "existing-system", name: original.name + "-system", os: "linux", runAs: "system" });
  await refresh();
  requests = [];
  await page.locator("#modal-form button[type=submit]").click();
  await page.locator(".form-error").filter({ hasText: "already registered" }).waitFor();
  assert.equal(requests.length, 0, "Existing companions must not be replaced");
  await page.getByRole("button", { name: "Cancel", exact: true }).click();

  await open("linux");
  nodes[0].runAs = "system";
  await refresh();
  requests = [];
  await page.locator("#modal-form button[type=submit]").click();
  await page.locator(".form-error").filter({ hasText: "machine changed" }).waitFor();
  assert.equal(requests.length, 0, "A stale dialog must not generate an installation");
  await page.getByRole("button", { name: "Cancel", exact: true }).click();

  await page.getByRole("button", { name: "startup-worker", exact: true }).click();
  await page.getByRole("button", { name: "More actions for startup-worker", exact: true }).click();
  assert.deepEqual(await page.locator("#machine-menu").getByRole("menuitem").allTextContents(), ["Gateway proxy", "Unregister"]);
  await page.keyboard.press("End");
  await page.keyboard.press("Enter");
  await page.getByLabel("Type startup-worker to confirm").waitFor();
  await page.getByRole("button", { name: "Cancel", exact: true }).click();

  // Lost mutation responses never create a second invitation automatically.
  await open("linux");
  await selectMode("hybrid");
  requests = [];
  uncertain = true;
  await page.locator("#modal-form button[type=submit]").click();
  await page.locator(".form-error").filter({ hasText: "Refresh" }).waitFor();
  assert.equal(requests.length, 1);
  await page.getByRole("button", { name: "Cancel", exact: true }).click();
  await page.unroute("**/console/api/status", statusRoute);
  await page.unroute("**/console/api/fleet/nodes/*/startup", startupRoute);
  await page.unroute("**/console/api/fleet/installations", installationsRoute);
  await refresh();
}

async function assertWorkerSecrets(page) {
  const assertPanel = async () => {
    const panel = page.locator("dialog.secrets-drawer");
    await panel.waitFor();
    const box = await panel.boundingBox(), viewport = page.viewportSize();
    assert.ok(Math.abs(box.x + box.width - viewport.width) < 1, "Secrets panel must sit against the right edge");
    assert.ok(Math.abs(box.y) < 1 && Math.abs(box.height - viewport.height) < 1, "Secrets panel must fill the viewport height");
  };
  const assertMaskedEntry = async () => {
    await assertPanel();
    const input = page.getByLabel("Secret value", { exact: true });
    assert.equal(await input.evaluate((element) => element.tagName), "TEXTAREA", "Avoid HTML inputs that Safari classifies as passwords when CSS-masked");
    assert.equal(await input.getAttribute("rows"), "1");
    assert.equal(await input.getAttribute("wrap"), "off");
    assert.equal(await input.getAttribute("aria-multiline"), "false");
    assert.equal(await input.getAttribute("autocomplete"), "off");
    assert.equal(await input.evaluate((element) => getComputedStyle(element).getPropertyValue("-webkit-text-security")), "disc", "Non-password secret inputs must remain masked");
    assert.equal(await page.locator("#secret-editor input, #secret-editor form").count(), 0);
    assert.equal(await input.evaluate((element) => element.closest("form")), null, "Worker entry must not submit a website credential form");
    assert.equal(await page.locator("#secret-editor [name]").count(), 0, "Avoid username/password-like field names");
    assert.equal(await page.getByLabel("Secret name", { exact: true }).evaluate((element) => element.tagName), "TEXTAREA");
    for (const field of [input, page.getByLabel("Secret name", { exact: true })]) {
      assert.equal(await field.evaluate((element) => element.getBoundingClientRect().height), 38, "Secret editor must stay exactly one input line high");
      assert.equal(await field.evaluate((element) => getComputedStyle(element).resize), "none", "Single-line editors must not have resize handles");
    }
    assert.equal(await page.getByRole("button", { name: /^(Save|Replace) secret$/ }).getAttribute("type"), "button");
  };
  const open = async () => {
    await page.getByRole("button", { name: "Actions for browser-worker", exact: true }).click();
    await page.getByRole("menuitem", { name: "Secrets", exact: true }).click();
    await page.getByRole("dialog", { name: "Secrets on browser-worker", exact: true }).waitFor();
    await assertPanel();
    assert.deepEqual(await page.locator(".secret-table th").allTextContents(), ["Name", "Updated", "Actions"]);
    assert.ok(!(await page.locator(".secret-table").textContent()).includes(" ago"), "Only the update timestamp should be shown, not relative age");
  };
  const requests = [];
  const observe = (request) => {
    if (request.method() === "PUT" && request.url().includes("/secrets/")) requests.push(request.postData());
  };
  page.on("request", observe);
  await open();
  await page.getByRole("button", { name: "Add secret", exact: true }).click();
  await page.getByLabel("Secret name", { exact: true }).fill("cancelled.password");
  await page.getByLabel("Secret value", { exact: true }).fill("private-cancelled-create");
  await page.getByRole("button", { name: "Cancel", exact: true }).click();
  await page.getByText("No secrets defined.", { exact: true }).waitFor();
  await assertPanel();
  assert.equal(await page.locator("#secret-editor").count(), 0, "Create Cancel must clear inputs and return to the list");
  assert.equal(requests.length, 0, "Create Cancel must not write");
  await page.getByRole("button", { name: "Add secret", exact: true }).click();
  await page.getByLabel("Secret name", { exact: true }).waitFor();
  assert.equal(await page.getByLabel("Secret name", { exact: true }).evaluate((element) => element === document.activeElement), true, "New secret entry should focus its name");
  await page.getByLabel("Secret name", { exact: true }).fill("browser.password");
  await assertMaskedEntry();
  await page.getByLabel("Secret name", { exact: true }).fill("invalid/name");
  await page.getByLabel("Secret value", { exact: true }).fill("private-browser-secret-世界");
  await page.getByRole("button", { name: "Save secret", exact: true }).click();
  assert.equal(requests.length, 0, "Invalid names must fail before encryption or a write");
  assert.equal(await page.getByLabel("Secret name", { exact: true }).evaluate((element) => element.validity.valid), false);
  await page.getByLabel("Secret name", { exact: true }).fill("browser.password");
  await page.getByLabel("Secret value", { exact: true }).press("End");
  await page.getByLabel("Secret value", { exact: true }).press("Enter");
  assert.equal(await page.getByLabel("Secret value", { exact: true }).inputValue(), "private-browser-secret-世界", "Enter must not insert a newline or submit");
  assert.equal(requests.length, 0);
  await page.getByLabel("Secret value", { exact: true }).fill("private-browser-secret-世界\nsecond-line");
  await page.getByRole("button", { name: "Save secret", exact: true }).click();
  assert.equal(requests.length, 0, "Multiline pastes must be rejected, not silently changed or sent");
  assert.equal(await page.getByLabel("Secret value", { exact: true }).evaluate((element) => element.validity.valid), false);
  await page.getByLabel("Secret value", { exact: true }).fill("private-browser-secret-世界");
  await page.getByRole("button", { name: "Save secret", exact: true }).click();
  await page.locator("#secret-editor").waitFor({ state: "detached" });
  await page.getByRole("dialog", { name: "Secrets on browser-worker", exact: true }).waitFor();
  await assertPanel();
  assert.equal(await page.locator(".secret-table tbody tr").count(), 1, "Creation must return to the refreshed list");
  await page.getByRole("button", { name: "Replace browser.password", exact: true }).waitFor();
  assert.equal(requests.length, 1);
  assert.ok(!requests[0].includes("private-browser-secret"), "Browser must send ciphertext, not plaintext");
  let record = JSON.parse(await readFile(path.join(dir, "node-state/secrets/values.json"), "utf8"))["browser.password"];
  assert.equal(record.value, "private-browser-secret-世界", "Go must decrypt the browser's Web Crypto envelope");
  const created = record.createdAt;
  assert.equal(record.createdAt, record.updatedAt);
  await page.getByRole("button", { name: "Close dialog", exact: true }).click();
  await open();
  assert.ok(!(await page.locator("#dialog-body").textContent()).includes("private-browser-secret"));
  const replace = page.getByRole("button", { name: "Replace browser.password", exact: true });
  assert.equal(await replace.locator("svg").count(), 1);
  assert.equal(await replace.textContent(), "", "Row actions must be icons, not text buttons");
  assert.equal(await page.getByRole("button", { name: "Add secret", exact: true }).textContent(), "", "Add action must use an accessible plus icon");
  await replace.click();
  await page.getByLabel("Secret value", { exact: true }).waitFor();
  assert.equal(await page.getByLabel("Secret value", { exact: true }).evaluate((element) => element === document.activeElement), true, "Editing must focus the value rather than the read-only name");
  assert.equal(await page.getByLabel("Secret value", { exact: true }).inputValue(), "");
  await assertMaskedEntry();
  await page.getByLabel("Secret value", { exact: true }).fill("private-browser-replacement");
  await page.getByRole("button", { name: "Replace secret", exact: true }).click();
  await page.locator("#secret-editor").waitFor({ state: "detached" });
  await page.getByRole("dialog", { name: "Secrets on browser-worker", exact: true }).waitFor();
  await assertPanel();
  assert.equal(await page.locator(".secret-table tbody tr").count(), 1, "Editing must return to the refreshed list without duplicating entries");
  record = JSON.parse(await readFile(path.join(dir, "node-state/secrets/values.json"), "utf8"))["browser.password"];
  assert.equal(record.value, "private-browser-replacement");
  assert.equal(record.createdAt, created);
  assert.notEqual(record.updatedAt, created);
  const updated = await page.evaluate((value) => date(value), record.updatedAt);
  assert.equal(await page.locator(".secret-table tbody td").nth(1).textContent(), updated, "The returned list must show the new update timestamp");
  await page.getByRole("button", { name: "Close dialog", exact: true }).click();
  const metadataURL = "**/console/api/fleet/nodes/*/secrets";
  const longName = "vpn." + "x".repeat(124);
  const metadataRoute = async (route) => {
    const response = await route.fetch(), listing = await response.json();
    listing.secrets.push({ name: longName, createdAt: "2020-01-01T12:34:56Z", updatedAt: "2026-11-03T12:34:56Z" });
    await route.fulfill({ response, json: listing });
  };
  await page.route(metadataURL, metadataRoute);
  await open();
  assert.equal(await page.getByRole("button", { name: "Delete " + longName, exact: true }).locator("svg").count(), 1);
  for (const width of [320, 390, 768, 1440]) {
    await page.setViewportSize({ width, height: 844 });
    await assertPanel();
    assert.ok(await page.evaluate(() => {
      const panel = document.querySelector("dialog.secrets-drawer"), table = panel.querySelector(".table-wrap");
      return document.documentElement.scrollWidth <= innerWidth && panel.scrollWidth <= panel.clientWidth && table.scrollWidth <= table.clientWidth;
    }), "Secret metadata must fit without horizontal scrolling, including long names and dates");
  }
  await page.unroute(metadataURL, metadataRoute);
  await page.setViewportSize({ width: 1440, height: 1000 });
  await page.getByRole("button", { name: "Replace browser.password", exact: true }).click();
  await page.getByLabel("Secret value", { exact: true }).fill("private-cancelled-value");
  await page.getByRole("button", { name: "Cancel", exact: true }).click();
  assert.equal(requests.length, 2, "Cancel must not write a secret");
  assert.equal(await page.locator("#secret-editor").count(), 0);
  await page.getByRole("button", { name: "Replace browser.password", exact: true }).waitFor();
  await assertPanel();
  record = JSON.parse(await readFile(path.join(dir, "node-state/secrets/values.json"), "utf8"))["browser.password"];
  assert.equal(record.value, "private-browser-replacement", "Edit Cancel must leave the stored value unchanged");
  await page.getByRole("button", { name: "Close dialog", exact: true }).click();
  await open();
  const remove = page.getByRole("button", { name: "Delete browser.password", exact: true });
  assert.equal(await remove.locator("svg").count(), 1);
  assert.equal(await remove.textContent(), "");
  await remove.click();
  await page.getByRole("button", { name: "Delete secret", exact: true }).waitFor();
  await assertPanel();
  await page.getByRole("button", { name: "Delete secret", exact: true }).click();
  await page.locator("#modal-form").waitFor({ state: "detached" });
  await open();
  await page.getByText("No secrets defined.", { exact: true }).waitFor();
  await page.getByRole("button", { name: "Add secret", exact: true }).click();
  await page.getByLabel("Secret name", { exact: true }).fill("uncertain.password");
  await page.getByLabel("Secret value", { exact: true }).fill("private-uncertain-value");
  const abortWrite = (route) => route.request().method() === "PUT" ? route.abort() : route.continue();
  await page.route("**/console/api/fleet/nodes/*/secrets/*", abortWrite);
  await page.getByRole("button", { name: "Save secret", exact: true }).click();
  await page.locator(".form-error").filter({ hasText: "response was lost" }).waitFor();
  assert.equal(requests.length, 3, "An uncertain write must not be retried");
  assert.equal(await page.getByLabel("Secret value", { exact: true }).inputValue(), "", "Failed submissions must clear the value field");
  assert.ok(!(await page.evaluate(() => JSON.stringify(localStorage))).includes("private-"));
  await page.unroute("**/console/api/fleet/nodes/*/secrets/*", abortWrite);
  await page.getByRole("button", { name: "Cancel", exact: true }).click();
  await page.getByText("No secrets defined.", { exact: true }).waitFor();
  await assertPanel();
  assert.equal(requests.length, 3, "Cancel after an uncertain write must not replay it");
  await page.getByRole("button", { name: "Close dialog", exact: true }).click();
  await open();
  await page.getByRole("button", { name: "Add secret", exact: true }).click();
  await page.getByLabel("Secret name", { exact: true }).fill("refresh.password");
  await page.getByLabel("Secret value", { exact: true }).fill("private-refresh-value");
  const failList = (route) => route.abort();
  await page.route(metadataURL, failList);
  await page.getByRole("button", { name: "Save secret", exact: true }).click();
  await page.getByText("The secret was saved, but the list could not be refreshed. Reload the list to inspect its metadata.", { exact: true }).waitFor();
  await assertPanel();
  assert.equal(await page.locator("#secret-editor").count(), 0, "A failed list refresh must not offer to repeat the successful write");
  assert.equal(requests.length, 4);
  await page.unroute(metadataURL, failList);
  await page.getByRole("button", { name: "Reload secrets", exact: true }).click();
  await page.getByRole("button", { name: "Replace refresh.password", exact: true }).waitFor();
  assert.equal(requests.length, 4, "Reload must only read metadata, never repeat a successful write");
  await page.getByRole("button", { name: "Close dialog", exact: true }).click();
  page.off("request", observe);
}

async function assertProxyChips(page, snapshot) {
  const baseNode = { ...snapshot.nodes[0], online: true, status: "summary", disabled: false, controlPending: false, activeCount: 0, proxyEndpoints: [], gatewayProxy: undefined },
    endpoint = "http://10.20.0.5:3128",
    host = { ...baseNode, id: "proxy-host", name: 'bridge "<img src=x onerror=alert(1)>', proxyEndpoints: [{ url: endpoint, managed: true, state: "unavailable", dependents: { invitations: 0, children: 0, retiring: 0 } }] },
    child = { ...baseNode, id: "proxy-child", name: "proxy-child", gatewayProxy: { url: endpoint, worker: host.id } },
    direct = { ...baseNode, id: "direct", name: "direct" },
    user = { ...baseNode, id: "proxy-pair-user", name: "proxy-pair", hybridId: "proxy-pair", hybridRole: "user", runAs: "user", gatewayProxy: { url: endpoint, worker: host.id } },
    system = { ...baseNode, id: "proxy-pair-system", name: "proxy-pair-system", hybridId: "proxy-pair", hybridRole: "system", runAs: "system", proxyEndpoints: [{ url: "http://10.20.0.6:3128", state: "unchecked" }] },
    fixture = { ...snapshot, nodes: [host, child, direct, user, system] };
  const statusRoute = (route) => route.fulfill({ json: fixture });
  await page.route("**/console/api/status", statusRoute);
  const row = (id) => page.locator(`.machine-table tbody tr[data-id="${id}"]`);
  const chips = (id) => row(id).locator(".proxy-tag");
  const close = () => page.getByRole("button", { name: "Close dialog", exact: true }).click();
  try {
    await page.waitForFunction(() => !state.busy);
    await page.evaluate(() => { state.contexts.clear(); state.filter = "all"; state.search = ""; });
    await pageLink(page, "machines");
    await refreshPage(page);
    await row(host.id).waitFor();
    assert.deepEqual(await chips(host.id).allTextContents(), ["Proxy"], "Hosting with zero dependents must still have a chip");
    assert.deepEqual(await chips(child.id).allTextContents(), ["Via proxy"]);
    assert.equal(await chips(direct.id).count(), 0, "Direct and legacy nodes must not get guessed proxy roles");
    assert.equal(await row(host.id).locator(".status-column .badge").textContent(), "Idle", "An unavailable proxy must not replace machine status");
    const gateway = snapshot.gateway.url;
    assert.ok((await chips(host.id).getAttribute("title")).includes(gateway));
    assert.ok((await chips(child.id).getAttribute("title")).includes(gateway));
    assert.ok((await chips(child.id).getAttribute("title")).includes(host.name), "The tooltip must resolve the proxy's fleet alias");
    assert.equal(await page.locator(".machine-table img").count(), 0, "Tooltip metadata must remain escaped text");
    assert.equal(await chips(child.id).getAttribute("onerror"), null);
    await chips(child.id).focus();
    assert.equal(await chips(child.id).evaluate((el) => el === document.activeElement), true, "Chip descriptions must be keyboard focusable");
    await row(host.id).locator(".machine-name").click();
    assert.deepEqual(await page.locator(".proxy-tags .proxy-tag").allTextContents(), ["Proxy"]);
    await page.locator(".drawer-section small").filter({ hasText: "unavailable" }).waitFor();
    assert.equal(await page.locator(".drawer-status .proxy-tag").count(), 0, "Configuration chips must be separate from status");
    await close();

    host.proxyEndpoints[0].dependents = { invitations: 0, children: 2, retiring: 1, machines: 1 };
    await refreshPage(page);
    await row(host.id).locator(".machine-name").click();
    const proxySection = page.locator(".drawer-section").filter({ has: page.getByRole("heading", { name: "Gateway proxy", exact: true }) });
    assert.ok((await proxySection.textContent()).includes("1 machine"));
    assert.ok(!/children|retiring|0 invitations/.test(await proxySection.textContent()), "The summary must not expose internal context counts or removal waits");
    await close();
    assert.equal(await page.evaluate(() => dependentsText({ invitations: 0, children: 0, retiring: 1, machines: 0 })), "", "A removal wait belongs in settings, not the summary");
    assert.equal(await page.evaluate(() => dependentsText({ invitations: 1, children: 2, retiring: 0 })), " · 2 contexts, 1 pending invitation", "Older gateway counts must not be mislabeled as machines");

    host.online = false;
    host.status = "offline";
    host.proxyEndpoints[0].state = "offline";
    child.online = false;
    child.status = "offline";
    await refreshPage(page);
    assert.deepEqual(await chips(host.id).allTextContents(), ["Proxy"], "Offline does not disable proxy hosting");
    assert.deepEqual(await chips(child.id).allTextContents(), ["Via proxy"], "Offline does not clear configured routing");
    assert.equal(await row(child.id).locator(".status-column .badge").textContent(), "Offline");
    child.gatewayProxy.worker = "missing-worker";
    await refreshPage(page);
    assert.ok((await chips(child.id).getAttribute("title")).includes(endpoint), "Unknown or external proxy workers must retain the configured endpoint");
    assert.ok(!(await chips(child.id).getAttribute("title")).includes(host.name), "A missing worker must not borrow an unrelated machine's name");

    host.proxyEndpoints = [];
    child.gatewayProxy = undefined;
    await refreshPage(page);
    assert.equal(await chips(host.id).count(), 0, "Disabling hosting must remove Proxy");
    assert.equal(await chips(child.id).count(), 0, "Switching to direct must remove Via proxy");
    await row(direct.id).locator(".machine-name").click();
    assert.equal(await page.getByRole("heading", { name: "Gateway proxy", exact: true }).count(), 0, "Disabled hosting must not occupy a drawer section");
    assert.equal(await page.locator('.drawer-section [data-action="worker-proxy"]').count(), 0);
    await page.getByRole("button", { name: "More actions for direct", exact: true }).click();
    await page.getByRole("menuitem", { name: "Gateway proxy", exact: true }).waitFor();
    await page.keyboard.press("Escape");
    await close();
    assert.deepEqual(await chips(user.id).allTextContents(), ["Proxy", "Via proxy"], "Grouped rows must include roles configured in either context");
    assert.equal(await row(user.id).getAttribute("data-id"), user.id);
    await row(user.id).locator(".machine-name").click();
    assert.deepEqual(await page.locator(".proxy-tags .proxy-tag").allTextContents(), ["Via proxy"], "A drawer must describe only its explicitly selected context");
    await close();
    await row(user.id).getByRole("button", { name: "System", exact: true }).click();
    await row(system.id).waitFor();
    assert.deepEqual(await chips(system.id).allTextContents(), ["Proxy", "Via proxy"]);
    await row(system.id).locator(".machine-name").click();
    assert.deepEqual(await page.locator(".proxy-tags .proxy-tag").allTextContents(), ["Proxy"]);
    await close();

    for (const readiness of ["ready", "stale", "unchecked", "unavailable"]) {
      system.proxyEndpoints[0].state = readiness;
      await refreshPage(page);
      assert.deepEqual(await chips(system.id).allTextContents(), ["Proxy", "Via proxy"], "Readiness must not change configuration chips");
    }
    await page.setViewportSize({ width: 360, height: 800 });
    await row(system.id).scrollIntoViewIfNeeded();
    assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), "Proxy and context chips must wrap on narrow screens");
    assert.equal(await chips(system.id).count(), 2);
    await page.setViewportSize({ width: 1440, height: 1000 });
    await page.locator('.brand[href="#overview"]').click();
    await row(system.id).waitFor();
    assert.deepEqual(await chips(system.id).allTextContents(), ["Proxy", "Via proxy"], "Overview must show the same role chips");
  } finally {
    await page.setViewportSize({ width: 1440, height: 1000 });
    await page.unroute("**/console/api/status", statusRoute);
    await page.evaluate(() => { state.contexts.clear(); });
    await refreshPage(page);
  }
}

async function assertProxyChoicesRecoverInOpenForm(page, snapshot) {
  const host = { ...snapshot.nodes[0], id: "recovering-proxy", name: "recovering-proxy", online: true, disabled: false,
    proxyEndpoints: [{ url: "http://10.0.0.5:3128", state: "unchecked", managed: true }] };
  const statusRoute = (route) => route.fulfill({ json: { ...snapshot, nodes: [host] } });
  let submissions = 0;
  const observe = (request) => {
    if (request.method() === "POST" && request.url().endsWith("/console/api/fleet/installations")) submissions++;
  };
  page.on("request", observe);
  await page.route("**/console/api/status", statusRoute);
  try {
    await page.waitForFunction(() => !state.busy);
    await pageLink(page, "machines");
    await refreshPage(page);
    await page.locator(".heading-actions [data-action=add]").click();
    const form = page.locator("#modal-form");
    await form.locator('input[name="name"]').fill("retained-machine");
    await form.locator('select[name="os"]').selectOption("windows");
    await form.locator('select[name="serviceMode"]').selectOption("hybrid");
    assert.equal(await form.locator('select[name="connection"]').count(), 0, "Direct-only setup must not show an unusable proxy choice");
    await page.locator("#installation-proxy-status").filter({ hasText: "Configured proxies are not ready yet" }).waitFor();
    await form.locator('input[name="name"]').focus();
    host.proxyEndpoints[0].state = "ready";
    await refreshPage(page);
    await form.locator('select[name="connection"]').waitFor({ state: "attached" });
    assert.equal(await form.locator('input[name="name"]').inputValue(), "retained-machine");
    assert.equal(await form.locator('select[name="os"]').inputValue(), "windows");
    assert.equal(await form.locator('select[name="serviceMode"]').inputValue(), "hybrid");
    assert.equal(await form.locator('input[name="name"]').evaluate((input) => input === document.activeElement), true, "Refreshing proxy choices must preserve editing focus");
    assert.equal(await form.locator('select[name="connection"]').inputValue(), "direct", "New readiness must not automatically choose proxy routing");
    assert.ok(await page.locator("#installation-proxy-status").isHidden());
    await form.locator('select[name="connection"]').selectOption("proxy");
    assert.equal(await form.locator('select[name="proxyWorker"]').inputValue(), host.id);
    assert.equal(await form.locator('select[name="proxyUrl"]').inputValue(), host.proxyEndpoints[0].url);
    host.proxyEndpoints[0].state = "unavailable";
    await refreshPage(page);
    assert.equal(await form.locator('select[name="connection"]').inputValue(), "proxy", "Losing readiness must not silently switch to Direct");
    await page.locator("#installation-proxy-status").filter({ hasText: "Your choice is preserved" }).waitFor();
    await form.locator("button[type=submit]").click();
    await form.locator(".form-error").filter({ hasText: "selected proxy is not ready" }).waitFor();
    assert.equal(submissions, 0, "Unready routing must fail before sending an invitation");
    host.proxyEndpoints[0].state = "ready";
    await refreshPage(page);
    assert.equal(await form.locator('select[name="connection"]').inputValue(), "proxy");
    assert.equal(await form.locator('select[name="proxyWorker"]').inputValue(), host.id);
    assert.equal(await form.locator('select[name="proxyUrl"]').inputValue(), host.proxyEndpoints[0].url);
    assert.ok(await page.locator("#installation-proxy-status").isHidden());
    assert.equal(submissions, 0, "Recovering readiness must not retry creation");
    await form.locator('[data-action="close"]').click();
    await refreshPage(page);
    assert.equal(await page.locator("#modal-form").count(), 0, "Readiness refresh must not reopen a dismissed form");
  } finally {
    page.off("request", observe);
    await page.unroute("**/console/api/status", statusRoute);
    await refreshPage(page);
  }
}

// Removing one context of a grouped host must name the one left running.
async function assertHybridUnregister(page, snapshot) {
  const baseNode = { ...snapshot.nodes[0], status: "summary", activeCount: 0, leased: false, disabled: false, controlPending: false, online: true },
    user = { ...baseNode, id: "paired-user", name: "paired-host", hybridId: "pair-1", hybridRole: "user", runAs: "user" },
    system = { ...baseNode, id: "paired-system", name: "paired-host-system", hybridId: "pair-1", hybridRole: "system", runAs: "system" };
  const statusRoute = (route) => route.fulfill({ json: { ...snapshot, nodes: [system, user] } });
  const removeURL = /\/console\/api\/fleet\/nodes\/[^/]+\?stop=true$/;
  const removed = [];
  const removeRoute = (route) => {
    if (route.request().method() !== "DELETE") return route.fallback();
    removed.push(new URL(route.request().url()).pathname.split("/").pop());
    return route.fulfill({ status: 204 });
  };
  await page.route("**/console/api/status", statusRoute);
  await page.route(removeURL, removeRoute);
  await page.waitForFunction(() => !state.busy);
  await page.evaluate(() => refresh(true));
  const openUnregister = async () => {
    await page.getByRole("button", { name: "paired-host", exact: true }).click();
    await page.getByRole("button", { name: "More actions for paired-host", exact: true }).click();
    await page.locator("#machine-menu").getByRole("menuitem", { name: "Unregister", exact: true }).click();
    await page.getByLabel("Type paired-host to confirm").waitFor();
  };
  await openUnregister();
  await page.locator("#modal-form .notice").filter({ hasText: "paired-host-system" }).filter({ hasText: "stays registered and running" }).waitFor();
  await page.getByLabel("Type paired-host to confirm").fill("paired-host");
  await page.locator("#modal-form button[type=submit]").click();
  await page.locator("#modal-form").waitFor({ state: "detached" });
  assert.deepEqual(removed, [user.id], "Unregistering User must not remove System unless asked");
  removed.length = 0;
  await openUnregister();
  await page.getByLabel("Also unregister paired-host-system").check();
  await page.getByLabel("Type paired-host to confirm").fill("paired-host");
  await page.locator("#modal-form button[type=submit]").click();
  await page.locator("#modal-form").waitFor({ state: "detached" });
  assert.deepEqual(removed, [user.id, system.id], "The explicit choice removes both contexts");
  await page.unroute(removeURL, removeRoute);
  await page.unroute("**/console/api/status", statusRoute);
  await page.waitForFunction(() => !state.busy);
  await page.evaluate(() => refresh(true));
}

async function assertMachineVersionLayout(page, snapshot) {
  const viewport = page.viewportSize(),
    shortVersion = "v0.8.3-2-gd2e9b2d-dirty",
    longVersion = shortVersion + '-20261113T041234Z-"quoted"<build>',
    node = {
      ...snapshot.nodes[0], id: "version-layout", name: "vm.fedora", os: "linux",
      online: true, status: "summary", activeCount: 0, leased: false,
      software: { version: shortVersion, arch: "amd64" },
      system: {
        arch: "amd64", logicalCPUs: 6, cpuUsagePercent: 2,
        memoryUsedBytes: 4 * 2 ** 30, memoryTotalBytes: 16 * 2 ** 30,
        disks: [{ path: "work", usedBytes: 28 * 2 ** 30, totalBytes: 128 * 2 ** 30 }],
        gpus: [],
      },
    },
    statusRoute = (route) => route.fulfill({ json: { ...snapshot, nodes: [node] } }),
    table = page.locator(".machine-table"),
    version = table.locator(".machine-version");
  await page.route("**/console/api/status", statusRoute);
  try {
    for (const gpus of [[], [{ name: "Test GPU", kind: "discrete", memoryUsedBytes: 2 ** 30, memoryTotalBytes: 8 * 2 ** 30 }]]) {
      node.system.gpus = gpus;
      for (const width of [1440, 1366]) {
        await page.setViewportSize({ width, height: 1000 });
        node.software.version = shortVersion;
        await refreshPage(page);
        const baseline = await table.locator("table").boundingBox();
        node.software.version = longVersion;
        await refreshPage(page);
        assert.equal(await version.getAttribute("title"), longVersion, "Hover must retain the exact escaped version");
        assert.equal(await version.textContent(), " · " + longVersion, "Ellipsis must not discard the underlying version");
        assert.ok(await version.evaluate((element) =>
          element.scrollWidth > element.clientWidth &&
          getComputedStyle(element).textOverflow === "ellipsis"), "Long versions must be visually ellipsized");
        const expanded = await table.locator("table").boundingBox();
        assert.ok(Math.abs(expanded.width - baseline.width) < 1, "Long versions must not widen the machine table");
        assert.ok(await table.evaluate((element) => element.scrollWidth <= element.clientWidth),
          `Machine table must fit at ${width}px, including the GPU column`);
      }
    }
    await page.getByRole("button", { name: node.name, exact: true }).click();
    assert.equal(await page.locator("dialog dt").filter({ hasText: /^Version$/ }).locator("xpath=following-sibling::dd[1]").textContent(), longVersion);
    await page.getByRole("button", { name: "Close dialog", exact: true }).click();
    for (const width of [1024, 768, 390, 320]) {
      await page.setViewportSize({ width, height: 900 });
      assert.equal(await version.isVisible(), false, "Narrow layouts must keep hiding the version");
      if (width <= 650)
        assert.ok(await table.evaluate((element) => element.scrollWidth <= element.clientWidth), "Stacked machine rows must not overflow");
    }
  } finally {
    await page.unroute("**/console/api/status", statusRoute);
    await page.setViewportSize(viewport);
    await refreshPage(page);
  }
}

async function assertHybridGrouping(page, snapshot) {
  const baseNode = { ...snapshot.nodes[0], status: "summary", activeCount: 0, leased: false, disabled: false, controlPending: false },
    resources = (cpu, used) => ({ ...baseNode.system, cpuUsagePercent: cpu, memoryUsedBytes: used, memoryTotalBytes: 100, disks: [{ path: "work", usedBytes: used, totalBytes: 100 }], gpus: [] }),
    gpu = { name: 'NVIDIA Test <img src=x onerror="alert(1)">', vendor: "NVIDIA", kind: "discrete", usagePercent: 67, memoryTotalBytes: 8 * 2 ** 30, memoryUsedBytes: 2 * 2 ** 30 },
    user = { ...baseNode, id: "paired-user", name: "paired-host", hybridId: "pair-1", hybridRole: "user", runAs: "user", online: false, system: { ...resources(23, 25), gpus: [{ ...gpu, usagePercent: 11, memoryUsedBytes: 0 }] }, connectionRoutes: { direct: 1, turn: 0, relay: 0, unknown: 0 }, tunnels: { forward: 0, reverse: 1 } },
    hidden = [{ ...gpu, name: "Intel Graphics", vendor: "Intel", kind: "integrated", usagePercent: 99, memoryUsedBytes: 7 * 2 ** 30 }, { ...gpu, name: "Parsec Virtual Display Adapter", kind: "virtual" }, { name: "VMware SVGA 3D" }],
    system = { ...baseNode, id: "paired-system", name: "paired-host-system", hybridId: "pair-1", hybridRole: "system", runAs: "system", online: true, system: { ...resources(71, 75), logicalCPUs: 16, gpus: [gpu, ...hidden, { name: "Intel AI Boost", vendor: "Intel", kind: "npu" }] }, connectionRoutes: { direct: 3, turn: 2, relay: 1, unknown: 1 }, tunnels: { forward: 2, reverse: 3 } },
    ordinary = { ...baseNode, id: "ordinary-system", name: "unpaired-system", runAs: "system", online: true, system: { ...resources(50, 50), gpus: [{ name: "Test GPU", kind: "discrete", usagePercent: 99, memoryTotalBytes: 8 * 2 ** 30, memoryUsedBytes: 2 ** 30 }] }, connectionRoutes: { direct: 2, turn: 0, relay: 0, unknown: 0 } };
  const statusRoute = (route) => route.fulfill({ json: { ...snapshot, nodes: [system, user, ordinary] } });
  await page.route("**/console/api/status", statusRoute);
  await page.waitForFunction(() => !state.busy);
  await page.evaluate(() => refresh(true));
  assert.equal(await page.locator("#nav-count").textContent(), "2", "A Hybrid companion must not add a machine to the counter");
  // Hash navigation can finish between the snapshot and table render. Wait for
  // the expected rows rather than reading a transient previous-page table.
  await page.waitForFunction(() => document.querySelectorAll(".machine-table tbody tr").length === 2);
  assert.equal(await page.locator(".machine-table tbody tr").count(), 2);
  assert.equal(await page.getByRole("button", { name: "paired-host-system", exact: true }).count(), 0);
  const row = page.locator(".machine-table tbody tr").filter({ has: page.getByRole("button", { name: "paired-host", exact: true }) });
  assert.equal(await row.getAttribute("data-id"), user.id, "User remains the default even when System is the only online context");
  assert.equal(await row.getByRole("button", { name: "User", exact: true }).getAttribute("aria-pressed"), "true");
  assert.equal(await row.locator(".status-column .badge").textContent(), "Idle", "Host status must include the online System context while User is selected");
  await assertResourceIcons(page);
  const assertSystemMetrics = async () => {
    assert.equal(await row.locator(".cpu-column meter").getAttribute("value"), "71");
    assert.equal(await row.locator(".cpu-column small").textContent(), "16 cores");
    assert.equal(await row.locator(".memory-column meter").getAttribute("value"), "75");
    assert.equal(await row.locator(".disk-column meter").getAttribute("value"), "75");
    assert.equal(await row.locator(".gpu-column meter").getAttribute("value"), "25", "GPU bar must show loaded dedicated memory, not compute utilization");
    assert.deepEqual(await row.locator(".gpu-usage").evaluateAll((elements) => elements.map((element) => element.title)), [gpu.name, "Intel AI Boost"]);
    assert.ok(!(await row.locator(".gpu-column").textContent()).includes(gpu.name), "GPU names must appear only in hover tooltips");
    assert.ok(!(await row.locator(".gpu-column").textContent()).includes("Intel AI Boost"), "NPU names must appear only in hover tooltips");
    assert.match(await row.locator(".gpu-usage").last().textContent(), /NPU detected/, "Hybrid fallback must retain an NPU without dedicated memory readings");
    assert.equal(await row.locator(".gpu-compute").textContent(), "Compute 67%");
    assert.equal(await row.locator(".gpu-usage small").first().textContent(), "2.0 GiB of 8.0 GiB");
    assert.equal(await row.locator(".gpu-column img").count(), 0, "GPU names must be escaped");
    assert.deepEqual(await row.locator(".counter").allTextContents(), ["Direct peers 4", "Relayed peers 3", "Tunnels 5"]);
  };
  await assertSystemMetrics();
  delete gpu.usagePercent;
  await refreshPage(page);
  assert.equal(await row.locator(".gpu-column meter").getAttribute("value"), "25", "AMD-style memory-only reports must still show loaded VRAM");
  assert.equal(await row.locator(".gpu-compute").count(), 0);
  gpu.usagePercent = 67;
  await refreshPage(page);
  assert.equal(await row.locator(".machine-menu-button").getAttribute("data-id"), user.id, "Telemetry fallback must not retarget management actions");
  await page.evaluate(() => refresh(true));
  await assertSystemMetrics();
  assert.equal(await row.getAttribute("data-id"), user.id, "Polling must preserve User selection while displaying System telemetry");
  const sortedIDs = await page.evaluate(() => {
    const previous = { sort: state.sort, descending: state.descending };
    try {
      return ["cpu", "gpu", "memory", "disk", "peers"].map((sort) => {
        state.sort = sort;
        state.descending = false;
        const ascending = sortedMachines(groupedMachines(state.snapshot.nodes)).map((node) => node.id);
        state.descending = true;
        return { ascending, descending: sortedMachines(groupedMachines(state.snapshot.nodes)).map((node) => node.id) };
      });
    } finally {
      Object.assign(state, previous);
    }
  });
  for (const sorted of sortedIDs) {
    assert.deepEqual(sorted.ascending, [ordinary.id, user.id], "Sorting must use the displayed System telemetry");
    assert.deepEqual(sorted.descending, [user.id, ordinary.id]);
  }
  const assertOfflineChip = async (label, offline) => {
    await page.waitForFunction(({ id, offline }) => {
      const chip = document.querySelector(`.machine-table .context-choice[data-id="${id}"]`);
      if (chip?.getAttribute("aria-description") !== (offline ? "Offline" : "Online")) return false;
      const label = chip.querySelector("span"),
        strike = getComputedStyle(label, "::after");
      if (getComputedStyle(chip).textDecorationLine !== "none") return false;
      if (!offline) return strike.content === "none";
      return strike.content === '\"\"' && strike.pointerEvents === "none" &&
        Math.abs(parseFloat(strike.top) - label.getBoundingClientRect().height / 2) < 0.1 &&
        strike.transform === "matrix(1, 0, 0, 1, 0, -0.5)";
    }, { id: label === "User" ? user.id : system.id, offline });
  };
  await assertOfflineChip("User", true);
  await assertOfflineChip("System", false);
  await assertContextChips(page);
  await row.getByRole("button", { name: "System", exact: true }).click();
  await assertContextChips(page);
  assert.equal(await row.getAttribute("data-id"), system.id);
  assert.equal(await row.locator(".status-column .badge").textContent(), "Idle");
  await assertSystemMetrics();
  await page.waitForFunction(() => !state.busy);
  await page.evaluate(() => refresh(true));
  assert.equal(await row.getAttribute("data-id"), system.id, "Polling preserves an explicit context selection");
  await row.getByRole("button", { name: "paired-host", exact: true }).click();
  await page.getByRole("dialog", { name: system.name, exact: true }).waitFor();
  await page.getByRole("heading", { name: "Resources", exact: true }).waitFor();
  assert.equal(await page.getByRole("heading", { name: "GPU/NPU resources", exact: true }).count(), 0, "GPU readings belong in the shared Resources section");
  const resourceGrid = page.locator("dialog .metadata").first();
  assert.deepEqual(await resourceGrid.locator("dt").allTextContents(), ["CPU utilization", "GPU memory usage", "NPU memory usage", "Memory usage", "Disk usage", "Processor", "Sampled"]);
  const assertGPUAlignment = async () => {
    const cpu = await resourceGrid.locator('meter[aria-label="CPU utilization"]').boundingBox(),
      gpu = await resourceGrid.locator(".gpu-usage meter").boundingBox(),
      memory = await resourceGrid.locator('meter[aria-label="Memory utilization"]').boundingBox();
    assert.ok(cpu && gpu && memory);
    assert.equal(gpu.x, cpu.x, "GPU bars must align with CPU bars");
    assert.equal(gpu.x, memory.x, "GPU bars must align with memory bars");
    assert.equal(gpu.width, cpu.width, "GPU bars must match CPU bar width");
    assert.equal(gpu.width, memory.width, "GPU bars must match memory bar width");
    assert.equal(await resourceGrid.locator(".gpu-usage small").first().evaluate((element) => getComputedStyle(element).fontSize), await resourceGrid.locator("dd > small").first().evaluate((element) => getComputedStyle(element).fontSize));
  };
  await assertGPUAlignment();
  await page.setViewportSize({ width: 390, height: 844 });
  await assertGPUAlignment();
  await page.setViewportSize({ width: 1440, height: 1000 });
  assert.equal(await page.locator("dialog .gpu-usage meter").getAttribute("value"), "25");
  assert.equal(await page.locator("dialog .gpu-usage small").first().textContent(), "2.0 GiB of 8.0 GiB");
  assert.deepEqual(await page.locator("dialog .gpu-usage").evaluateAll((elements) => elements.map((element) => element.title)), [gpu.name, "Intel AI Boost"], "Drawer tooltips must also exclude integrated and virtual adapters");
  assert.ok(!(await page.locator("dialog").textContent()).includes(gpu.name), "Drawer GPU names must appear only in hover tooltips");
  await page.getByRole("button", { name: "Close dialog", exact: true }).click();
  await row.getByRole("button", { name: "User", exact: true }).click();
  assert.equal(await row.getAttribute("data-id"), user.id);
  assert.equal(await page.locator('.stat[data-filter="online"] .stat-value').textContent(), "2", "Online counts hosts, not execution profiles");
  const refresh = () => refreshPage(page);
  for (const updating of [user, system]) {
    const other = updating === user ? system : user;
    other.online = true;
    updating.status = "update";
    updating.updateState = "restarting";
    for (const online of [true, false, true]) {
      updating.online = online;
      await refresh();
      assert.equal(await row.locator(".status-column .badge").textContent(), "Updating", "Restarting either context must not flash Idle during disconnect or reconnect");
      assert.equal(await row.locator(".status-column small").textContent(), "Restarting");
      assert.equal(await row.getAttribute("data-id"), user.id, "Update status must not change the selected action target");
      assert.equal(await page.locator('.stat[data-filter="online"] .stat-value').textContent(), "2", "Restart grace must not alter host online counts");
      await assertOfflineChip(updating === user ? "User" : "System", !online);
    }
    updating.updateState = "startup-refresh-required";
    await refresh();
    assert.equal(await row.locator(".status-column small").textContent(), "Startup refresh required", "Settled software with a retained supervisor must show an actionable blocker rather than Staged");
    // The gateway ends grace on confirmation or expiry. Do not retain stale
    // client-side Updating after normal availability returns.
    updating.status = "offline";
    updating.online = false;
    await refresh();
    assert.equal(await row.locator(".status-column .badge").textContent(), "Idle", "Expired restart grace must restore the online companion's status");
    updating.status = "summary";
    updating.online = true;
    await refresh();
    assert.equal(await row.locator(".status-column .badge").textContent(), "Idle", "Confirmed resumption must restore normal health immediately");
    delete updating.updateState;
  }
  user.online = false;
  system.activeCount = 2;
  system.work = { exec: 2 };
  await refresh();
  assert.equal(await row.locator(".status-column .badge").textContent(), "Busy", "Work in System must remain visible while User is selected");
  assert.match(await row.locator(".status-column small").textContent(), /2 exec sessions/);
  await pageLink(page, "machines");
  await page.locator('[data-action="filter-state"][data-filter="online"]').click();
  assert.equal(await row.count(), 1);
  await page.locator('[data-action="filter-state"][data-filter="offline"]').click();
  assert.equal(await row.count(), 0, "A host with an online context must not appear in Offline");
  await page.locator('[data-action="filter-state"][data-filter="all"]').click();
  await row.getByRole("button", { name: "paired-host", exact: true }).click();
  await page.getByRole("dialog", { name: user.name, exact: true }).waitFor();
  assert.equal(await page.locator("dialog .badge").first().textContent(), "Offline", "The drawer must retain the selected context's own status");
  assert.equal(await page.locator("dialog .gpu-usage meter").count(), 0, "Offline drawer must hide GPU usage");
  assert.equal(await page.locator("dialog .gpu-usage").getAttribute("title"), gpu.name, "Offline drawers must retain GPU model tooltips");
  assert.equal(await page.locator("dialog .gpu-usage small").textContent(), "8.0 GiB total");
  await page.getByRole("button", { name: "Close dialog", exact: true }).click();
  system.online = false;
  system.lastSeen = "2026-01-02T00:00:00Z";
  user.lastSeen = "2026-01-01T00:00:00Z";
  await refresh();
  assert.equal(await row.locator(".status-column .badge").textContent(), "Offline");
  assert.equal(await row.locator("meter").count(), 0, "Both offline contexts must leave live resources unknown");
  assert.equal(await row.locator(".gpu-usage").getAttribute("title"), gpu.name, "Offline rows must retain GPU model tooltips");
  assert.deepEqual(await row.locator(".counter").allTextContents(), ["Direct peers –", "Relayed peers –", "Tunnels –"]);
  assert.equal(await row.locator(".status-column small").textContent(), await page.evaluate((lastSeen) => "Seen " + age(lastSeen).split(" ")[0] + " ago", system.lastSeen), "Offline host age uses the most recently seen context");
  await assertOfflineChip("User", true);
  await assertOfflineChip("System", true);
  await page.locator('[data-action="filter-state"][data-filter="offline"]').click();
  assert.equal(await row.count(), 1);
  await page.locator('[data-action="filter-state"][data-filter="all"]').click();
  user.online = true;
  await row.getByRole("button", { name: "System", exact: true }).click();
  await refresh();
  assert.equal(await row.getAttribute("data-id"), system.id, "An offline System selection must not switch to User automatically");
  assert.equal(await row.locator(".status-column .badge").textContent(), "Idle");
  assert.equal(await row.locator("meter").count(), 0, "An explicit offline System selection must not borrow User metrics");
  await assertOfflineChip("User", false);
  await assertOfflineChip("System", true);
  system.online = true;
  await row.getByRole("button", { name: "User", exact: true }).click();
  await refresh();
  assert.equal(await row.locator(".status-column .badge").textContent(), "Busy", "An idle User context must not hide System work");
  assert.equal(await row.locator(".cpu-column meter").getAttribute("value"), "23", "An online User must show its own metrics");
  assert.equal(await row.locator(".gpu-column meter").getAttribute("value"), "0");
  assert.equal(await row.locator(".gpu-usage small").first().textContent(), "0 B of 8.0 GiB");
  assert.equal(await row.locator(".memory-column meter").getAttribute("value"), "25");
  assert.equal(await row.locator(".disk-column meter").getAttribute("value"), "25");
  assert.deepEqual(await row.locator(".counter").allTextContents(), ["Direct peers 1", "Relayed peers 0", "Tunnels 1"]);
  user.online = false;
  await refresh();
  await assertSystemMetrics();
  user.online = true;
  await refresh();
  assert.equal(await row.getAttribute("data-id"), user.id);
  assert.equal(await row.locator(".cpu-column meter").getAttribute("value"), "23", "User reconnection must restore its telemetry automatically");
  assert.equal(await row.locator(".gpu-column meter").getAttribute("value"), "0");
  await assertOfflineChip("User", false);
  await assertOfflineChip("System", false);
  await page.locator('.brand[href="#overview"]').click();
  await page.setViewportSize({ width: 390, height: 844 });
  assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
  await page.setViewportSize({ width: 1440, height: 1000 });
  for (const node of [user, system, ordinary]) node.system.gpus = hidden;
  await refresh();
  await page.locator('.machine-table [data-sort="gpu"]').waitFor({ state: "detached" });
  assert.equal(await page.locator('.machine-table [data-sort="gpu"]').count(), 0, "Fleets with only integrated, virtual, or unknown adapters must hide the GPU/NPU column");
  assert.equal(await page.locator(".gpu-column").count(), 0);
  user.disabled = true;
  system.online = false;
  await refresh();
  assert.equal(await row.count(), 0, "Overview must not combine an online disabled context with an offline enabled context");
  user.disabled = false;
  system.disabled = true;
  system.online = true;
  await refresh();
  assert.equal(await row.count(), 1, "An online enabled User keeps its Hybrid host on Overview even with a disabled companion");
  user.online = false;
  ordinary.disabled = true;
  await refresh();
  assert.equal(await page.locator(".machine-table tbody tr").count(), 0);
  await page.getByText("No online enabled machines", { exact: true }).waitFor();
  assert.equal(await page.getByRole("button", { name: "Clear filters", exact: true }).count(), 0, "Overview must not offer irrelevant search controls");
  assert.equal(await page.locator("#nav-count").textContent(), "2", "Overview filtering must not change the fleet count");
  await page.locator('.panel-heading [data-action="filter-machines"][data-filter="all"]').click();
  await page.locator("#machine-search").waitFor();
  assert.equal(await page.locator(".machine-table tbody tr").count(), 2, "All machines must retain hidden offline and disabled hosts");
  await page.locator('.brand[href="#overview"]').click();
  await page.unroute("**/console/api/status", statusRoute);
  await page.waitForFunction(() => !state.busy);
  await page.evaluate(() => refresh(true));
}

async function assertAcceleratorPresence(page, snapshot) {
  const npu = { name: "Snapdragon(R) X Plus - X1P42100 - Qualcomm(R) Hexagon(TM) NPU", kind: "npu" },
    baseNode = { ...snapshot.nodes[0], status: "summary", activeCount: 0, leased: false, disabled: false, controlPending: false, online: true },
    surface = { ...baseNode, id: "surface", name: "surface", system: { ...baseNode.system, gpus: [npu] } },
    cpuOnly = { ...baseNode, id: "cpu-only", name: "cpu-only", system: { ...baseNode.system, gpus: [] } };
  const statusRoute = (route) => route.fulfill({ json: { ...snapshot, nodes: [surface, cpuOnly] } });
  await page.route("**/console/api/status", statusRoute);
  await refreshPage(page);
  const row = page.locator('.machine-table tbody tr[data-id="surface"]'),
    device = row.locator(".gpu-usage");
  const assertPresence = async (device) => {
    assert.equal(await device.getAttribute("title"), npu.name);
    assert.match(await device.textContent(), /NPU detected/);
    assert.equal(await device.locator("small").first().textContent(), "Memory unavailable");
    assert.equal(await device.locator("meter").count(), 0, "Missing dedicated memory must not become a zero reading");
  };
  await assertPresence(device);
  assert.equal(await page.locator('.machine-table tbody tr[data-id="cpu-only"] .gpu-usage').count(), 0, "No accelerator must remain distinct from an NPU without metrics");
  await pageLink(page, "machines");
  await assertPresence(device);
  await page.setViewportSize({ width: 390, height: 844 });
  await assertPresence(device);
  assert.ok(await device.isVisible());
  assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
  await page.setViewportSize({ width: 1440, height: 1000 });
  await row.getByRole("button", { name: "surface", exact: true }).click();
  await assertPresence(page.locator("dialog .gpu-usage"));
  await page.getByRole("button", { name: "Close dialog", exact: true }).click();
  npu.usagePercent = 54;
  await refreshPage(page);
  await assertPresence(device);
  assert.equal(await device.locator(".gpu-compute").textContent(), "Compute 54%");
  surface.online = false;
  await refreshPage(page);
  await assertPresence(device);
  assert.equal(await device.locator(".gpu-compute").count(), 0, "Offline inventory must not expose cached compute activity");
  await row.getByRole("button", { name: "surface", exact: true }).click();
  await assertPresence(page.locator("dialog .gpu-usage"));
  assert.equal(await page.locator("dialog .gpu-compute").count(), 0);
  await page.getByRole("button", { name: "Close dialog", exact: true }).click();
  surface.online = true;
  npu.kind = "discrete";
  await refreshPage(page);
  assert.match(await device.textContent(), /GPU detected/, "Discrete GPUs without memory readings must also remain visible");
  await page.unroute("**/console/api/status", statusRoute);
  await refreshPage(page);
  await page.locator('.brand[href="#overview"]').click();
}

try {
  start(
    [
      "gateway",
      "--listen",
      "127.0.0.1:" + gatewayPort,
      "--data",
      path.join(dir, "gateway"),
    ],
    { CONTROL_SUPERUSER_KEY: superkey },
  );
  await until(async () => {
    try {
      return (await fetch(base + "/healthz")).ok;
    } catch {
      return false;
    }
  }, "Gateway did not start");
  await api("/v1/console/password", "PUT", { password });
  const account = await api("/v1/admin/users", "POST", {
    name: "browser-owner",
  });
  await api("/v1/console/password", "PUT", { password }, account.token);
  const key = await api(
    "/v1/fleet/keys",
    "POST",
    { name: "Fixture worker" },
    account.token,
  );
  const config = path.join(dir, "node.json");
  await writeFile(
    config,
    JSON.stringify({
      name: "browser-worker",
      gateway: base,
      token: key.token,
      dataDir: "node-state",
      workDir: "node-work",
      listen: "127.0.0.1:" + nodePort,
      allow: { "@orchestrator": ["secrets.manage"] },
    }),
  );
  start(["node", "--config", config]);
  await until(
    async () =>
      (await api("/v1/nodes", "GET", undefined, account.token)).some(
        (n) => n.name === "browser-worker" && n.online,
      ),
    "Worker did not register",
  );

  browser = await chromium.launch({
    headless: true,
    ...(process.env.CONTROL_CONSOLE_TEST_CHROME
      ? { executablePath: process.env.CONTROL_CONSOLE_TEST_CHROME }
      : {}),
  });
  const context = await browser.newContext({
    viewport: { width: 1440, height: 1000 },
    colorScheme: "dark",
    reducedMotion: "reduce",
  });
  const page = await context.newPage();
  const errors = [];
  page.on("pageerror", (error) => errors.push(error.message));
  page.on("console", (message) => {
    if (
      message.type() === "error" &&
      message.text().includes("Content Security Policy")
    )
      errors.push(message.text());
  });
  await login(page, "browser-owner");
  await assertAsyncConsoleOwnership(browser);
  await assertAuditRequestOwnership(browser);
  await assertBrandAssets(page);
  await page
    .getByRole("button", { name: "browser-worker", exact: true })
    .waitFor();
  assert.equal(await page.locator("#operator-nav").isVisible(), false);
  await assertResourceIcons(page, true);
  await assertResourceIcons(page);
  await pageLink(page, "machines");
  await assertResourceIcons(page);
  await assertWorkerSecrets(page);
  await assertGatewayProxySetup(page);
  await page.locator("#machine-search").fill("not-a-real-machine");
  await page.getByText("No matching machines", { exact: true }).waitFor();
  await page.locator("#machine-search").fill("browser-worker");
  const worker = page.getByRole("button", {
      name: "browser-worker",
      exact: true,
    }),
    actions = page.getByRole("button", {
      name: "Actions for browser-worker",
      exact: true,
    }),
    machineMenu = page.getByRole("menu", {
      name: "Actions for browser-worker",
      exact: true,
    });
  let machineMutations = 0;
  page.on("request", (request) => {
    if (
      request.url().includes("/console/api/fleet/nodes/") &&
      !request.url().endsWith("/startup") &&
      request.method() !== "GET"
    )
      machineMutations++;
  });
  await worker.click({ button: "right" });
  await machineMenu.waitFor();
  await page.keyboard.press("Escape");
  // Some browsers fire contextmenu while the secondary button is still down.
  // Releasing that opening press must not light-dismiss the new menu.
  const workerBox = await worker.boundingBox();
  await page.mouse.move(
    workerBox.x + workerBox.width / 2,
    workerBox.y + workerBox.height / 2,
  );
  await page.mouse.down({ button: "right" });
  await worker.dispatchEvent("contextmenu", {
    clientX: workerBox.x + workerBox.width / 2,
    clientY: workerBox.y + workerBox.height / 2,
    button: 2,
  });
  await machineMenu.waitFor();
  await page.mouse.up({ button: "right" });
  await page.evaluate(() => new Promise(requestAnimationFrame));
  assert.equal(
    await machineMenu.isVisible(),
    true,
    "Releasing the right button dismissed the menu",
  );
  const registered = (await api("/v1/nodes", "GET", undefined, account.token))[0];
  assert.deepEqual(await machineMenu.getByRole("menuitem").allTextContents(), [
    "View details",
    "Rename",
    "Secrets",
    ...(registered.runAs === "user" ? ["Startup context"] : []),
    "Gateway proxy",
    "Disable",
    "Unregister",
  ]);
  const highlightedActions = () =>
    machineMenu.getByRole("menuitem").evaluateAll((items) =>
      items
        .filter((item) => getComputedStyle(item).backgroundColor !== "rgba(0, 0, 0, 0)")
        .map((item) => item.textContent),
    );
  await machineMenu
    .getByRole("menuitem", { name: "Rename", exact: true })
    .hover();
  assert.deepEqual(
    await highlightedActions(),
    ["Rename"],
    "Mouse hover should not leave View details highlighted",
  );
  await page.keyboard.press("End");
  assert.deepEqual(
    await highlightedActions(),
    ["Unregister"],
    "Keyboard focus should be the only highlighted action",
  );
  await machineMenu
    .getByRole("menuitem", { name: "Disable", exact: true })
    .hover();
  assert.deepEqual(
    await highlightedActions(),
    ["Disable"],
    "Pointer movement should clear the old keyboard highlight",
  );
  await page.keyboard.press("Home");
  assert.equal(await page.locator("#dialog").isVisible(), false);
  assert.equal(
    await machineMenu
      .getByRole("menuitem", { name: "Wake", exact: true })
      .count(),
    0,
  );
  // Polling replaces the table but must preserve the open menu and its focus.
  await page.evaluate(() => refresh(true));
  assert.equal(await machineMenu.isVisible(), true);
  assert.equal(await actions.getAttribute("aria-expanded"), "true");
  await page.keyboard.press("Escape");
  assert.equal(
    await worker.evaluate((element) => element === document.activeElement),
    true,
  );
  await worker.press("Shift+F10");
  await page.keyboard.press("End");
  await page.keyboard.press("Enter");
  await page.getByLabel("Type browser-worker to confirm").waitFor();
  await page.getByRole("button", { name: "Cancel", exact: true }).click();
  assert.equal(
    machineMutations,
    0,
    "Opening or cancelling a menu action mutated the machine",
  );
  await actions.click();
  await machineMenu.waitFor();
  await actions.click();
  assert.equal(
    await machineMenu.isVisible(),
    false,
    "The actions button should toggle the menu closed",
  );
  await actions.click();
  await machineMenu
    .getByRole("menuitem", { name: "Disable", exact: true })
    .click();
  await page
    .getByRole("button", { name: "Disable machine", exact: true })
    .click();
  await page
    .locator(".machine-table")
    .getByText("Disabled", { exact: true })
    .waitFor();
  await actions.click();
  await machineMenu
    .getByRole("menuitem", { name: "Enable", exact: true })
    .click();
  await page
    .getByRole("button", { name: "Enable machine", exact: true })
    .click();
  await page
    .locator(".machine-table")
    .getByText("Disabled", { exact: true })
    .waitFor({ state: "hidden" });
  assert.equal(
    machineMutations,
    2,
    "Lifecycle actions should each submit once",
  );
  await worker.click({ button: "right" });
  await page.keyboard.press("ArrowDown");
  await page.keyboard.press("Enter");
  await page
    .getByRole("dialog", { name: "Rename browser-worker", exact: true })
    .waitFor();
  await page.locator("#modal-form input[name=name]").fill("renamed-worker");
  await page.locator("#modal-form button[type=submit]").click();
  await page.locator("#dialog").waitFor({ state: "hidden" });
  await page.locator("#machine-search").fill("renamed-worker");
  await page
    .getByRole("button", { name: "renamed-worker", exact: true })
    .waitFor();

  await pageLink(page, "keys");
  await page.getByRole("button", { name: "Create key", exact: false }).click();
  await page
    .locator("#modal-form input[name=name]")
    .fill('Browser-created key <img src=x onerror="alert(1)">');
  await page.locator("#modal-form button[type=submit]").click();
  await page.locator("#secret-value").waitFor();
  assert.match(await page.locator("#secret-value").textContent(), /^ctl_/);
  await page.getByRole("button", { name: "Done", exact: true }).click();
  await page
    .getByText('Browser-created key <img src=x onerror="alert(1)">', {
      exact: true,
    })
    .waitFor();
  assert.equal(
    await page.locator("img").count(),
    0,
    "Untrusted key name was interpreted as markup",
  );
  const keyRow = page.locator("tr").filter({ hasText: "Browser-created key" });
  await keyRow.getByRole("button", { name: "Revoke", exact: true }).click();
  await page.locator("#modal-form button[type=submit]").click();
  await keyRow.getByText("Revoked", { exact: true }).waitFor();
  await keyRow.getByRole("button", { name: "Rename", exact: true }).click();
  await page
    .locator("#modal-form input[name=name]")
    .fill("Renamed browser key");
  await page.locator("#modal-form button[type=submit]").click();
  await page.getByText("Renamed browser key", { exact: true }).waitFor();

  // The rename above is attributed to this browser login in the audit log.
  await pageLink(page, "activity");
  const renamed = page.locator("tr").filter({ hasText: "Renamed key" });
  await renamed.getByText("Renamed browser key", { exact: true }).waitFor();
  await renamed.getByText("browser-owner", { exact: true }).waitFor();

  await pageLink(page, "invitations");
  await page.getByText("No invitations yet", { exact: true }).waitFor();
  await page.locator(".heading-actions [data-action=add]").click();
  await page.locator("#modal-form button[type=submit]").click();
  await page
    .locator("#modal-form .form-error")
    .filter({ hasText: "publish a bundle" })
    .waitFor();
  await page.locator("#modal-form [data-action=close]").click();

  // Direct is the only available connection, so there is no choice to present.
  await page.locator(".heading-actions [data-action=add]").click();
  assert.equal(await page.locator('#modal-form select[name="connection"]').count(), 0);
  assert.equal(await page.locator("#proxy-worker-field").count(), 0);
  await page.locator("#modal-form [data-action=close]").click();
  for (const readiness of ["offline", "stale", "ready"]) {
    await page.evaluate((readiness) => {
      const n = state.snapshot.nodes.find((entry) => entry.name === "renamed-worker");
      n.proxyEndpoints = [{ url: "http://10.0.0.5:3128", state: readiness }];
      addMachine();
      n.proxyEndpoints = [];
    }, readiness);
    assert.equal(await page.locator('#modal-form select[name="connection"]').count(), readiness === "ready" ? 1 : 0);
    if (readiness === "ready") {
      await page.locator('#modal-form select[name="connection"]').selectOption("proxy");
      await page.locator("#proxy-worker-field").waitFor();
      assert.equal(await page.locator('#modal-form select[name="proxyUrl"]').inputValue(), "http://10.0.0.5:3128");
    }
    await page.locator("#modal-form [data-action=close]").click();
  }
  assert.equal((await api("/v1/fleet/installations", "GET", undefined, account.token)).length, 0,
    "Opening connection choices must not create an invitation");

  const os = await publishInstallers();
  const groupedInvitations = await page.evaluate(() => {
    const shared = { expiresAt: new Date(Date.now() + 60000).toISOString() };
    return groupInvitations([
      { ...shared, id: "system-first", name: "paired-system", hybridId: "pair", hybridRole: "system", redeemedId: "system-node" },
      { ...shared, id: "plain", name: "unrelated" },
      { ...shared, id: "plain-system", name: "unrelated-system" },
      { ...shared, id: "user-second", name: "paired", hybridId: "pair", hybridRole: "user" },
      { ...shared, id: "complete-system", name: "complete-system", hybridId: "complete", hybridRole: "system", redeemedId: "complete-system-node" },
      { ...shared, id: "complete-user", name: "complete", hybridId: "complete", hybridRole: "user", redeemedId: "complete-user-node" },
    ]).map((i) => ({ id: i.id, name: i.name, redeemedId: i.redeemedId || "", startup: i.serviceMode || "" }));
  });
  assert.deepEqual(groupedInvitations, [
    { id: "user-second", name: "paired", redeemedId: "", startup: "hybrid" },
    { id: "plain", name: "unrelated", redeemedId: "", startup: "" },
    { id: "plain-system", name: "unrelated-system", redeemedId: "", startup: "" },
    { id: "complete-user", name: "complete", redeemedId: "complete-user-node", startup: "hybrid" },
  ], "Grouping must use pair metadata, prefer User regardless of ordering, and wait for both redemptions");
  await page.locator(".heading-actions [data-action=add]").click();
  await page
    .locator("#modal-form input[name=name]")
    .fill("pending-browser-worker");
  await page
    .locator("#modal-form select[name=os]")
    .locator("xpath=preceding-sibling::button[1]")
    .click();
  await page.locator(`[role=option][data-value="${os}"]`).click();
  assert.equal(
    await page.locator("#modal-form select[name=os]").inputValue(),
    os,
  );
  await page.locator("#modal-form button[type=submit]").click();
  await page.locator("#secret-value").waitFor();
  assert.match(
    await page.locator("#secret-value").textContent(),
    /\/install\//,
  );
  await page.getByRole("button", { name: "Done", exact: true }).click();
  await page.getByText("pending-browser-worker", { exact: true }).waitFor();
  await page.locator(".heading-actions [data-action=add]").click();
  await page
    .locator("#modal-form input[name=name]")
    .fill("pending-browser-worker");
  await page
    .locator("#modal-form select[name=os]")
    .locator("xpath=preceding-sibling::button[1]")
    .click();
  await page.locator(`[role=option][data-value="${os}"]`).click();
  assert.equal(
    await page.locator("#modal-form select[name=os]").inputValue(),
    os,
  );
  await page.locator("#modal-form button[type=submit]").click();
  await page.locator("#force-confirm").check();
  await page.locator("#modal-form button[type=submit]").click();
  await page.locator("#secret-value").waitFor();
  await page.getByRole("button", { name: "Done", exact: true }).click();
  await page
    .locator("tr")
    .filter({ hasText: "pending-browser-worker" })
    .getByText("Revoked", { exact: true })
    .waitFor();
  const pending = page
    .locator("tr")
    .filter({ hasText: "pending-browser-worker" })
    .filter({ has: page.getByText("Pending", { exact: true }) });
  await pending.getByRole("button", { name: "Revoke", exact: true }).click();
  await page.locator("#modal-form button[type=submit]").click();

  // Clear standalone history first, then exercise real Hybrid pair creation,
  // pair-level revocation, grouped counts, cancellation, and durable clearing.
  await page.getByRole("button", { name: "Clear history", exact: true }).click();
  await page.locator("#modal-form button[type=submit]").click();
  await page.getByText("No invitations yet", { exact: true }).waitFor();
  const hybridRequest = { name: "browser-hybrid", os, gateway: base, serviceMode: "hybrid" };
  for (let attempt = 0; attempt < 2; attempt++) {
    const link = await api("/v1/fleet/installations", "POST", hybridRequest, account.token);
    await page.locator(".heading-actions [data-action=refresh]").click();
    const row = page.locator("tr").filter({ hasText: "browser-hybrid" })
      .filter({ has: page.getByText("Pending", { exact: true }) });
    await row.getByText("Hybrid", { exact: true }).waitFor();
    assert.equal(await row.count(), 1, "Hybrid invitation must occupy one row");
    assert.equal(await page.getByText("browser-hybrid-system", { exact: true }).count(), 0);
    await row.getByRole("button", { name: "Revoke", exact: true }).click();
    await page.locator("#modal-form button[type=submit]").click();
    await until(async () => {
      const invitations = await api("/v1/fleet/installations", "GET", undefined, account.token);
      const pair = invitations.filter((i) => i.hybridId === link.hybridId);
      return pair.length === 2 && pair.every((i) => i.revoked);
    }, "Grouped revoke did not revoke both Hybrid invitations");
  }
  await api("/v1/fleet/installations", "POST", { ...hybridRequest, name: "pending-hybrid" }, account.token);
  await page.locator(".heading-actions [data-action=refresh]").click();
  await page.getByText("pending-hybrid", { exact: true }).waitFor();
  assert.equal(await page.locator("tr").filter({ hasText: "browser-hybrid" }).count(), 2,
    "Same-name attempts must remain separate rows");
  await page.getByRole("button", { name: "Clear history", exact: true }).click();
  await page.getByText("Clear 2 invitations from this list.", { exact: false }).waitFor();
  await page.getByRole("button", { name: "Cancel", exact: true }).click();
  assert.equal((await api("/v1/fleet/installations", "GET", undefined, account.token)).length, 6);
  await page.getByRole("button", { name: "Clear history", exact: true }).click();
  await page.locator("#modal-form button[type=submit]").click();
  await page.getByText("browser-hybrid", { exact: true }).first().waitFor({ state: "hidden" });
  await page.getByText("pending-hybrid", { exact: true }).waitFor();
  const remainingInvitations = await api("/v1/fleet/installations", "GET", undefined, account.token);
  assert.equal(remainingInvitations.length, 2);
  assert.ok(remainingInvitations.every((i) => !i.revoked && i.hybridId));
  await page.reload();
  await page.getByText("pending-hybrid", { exact: true }).waitFor();
  assert.equal(await page.getByText("browser-hybrid", { exact: true }).count(), 0,
    "Cleared Hybrid history returned after refresh");

  await page.locator('a[href="#overview"]').first().click();
  await page.waitForFunction(
    () =>
      document
        .querySelector('.brand[href="#overview"]')
        .getAttribute("aria-current") === "page",
  );
  await page
    .getByRole("button", { name: "renamed-worker", exact: true })
    .waitFor();

  // Overview always shows the fleet, even after searching the Machines page.
  await pageLink(page, "machines");
  await page.locator("#machine-search").fill("no-matches");
  await page.locator('.brand[href="#overview"]').click();
  await page
    .getByRole("button", { name: "renamed-worker", exact: true })
    .waitFor();
  await page.locator('.stat[data-filter="offline"]').click();
  await page.getByText("No matching machines", { exact: true }).waitFor();
  assert.equal(
    await page
      .locator('[data-action="filter-state"][aria-pressed="true"]')
      .getAttribute("data-filter"),
    "offline",
  );
  await page
    .getByRole("button", { name: "Clear filters", exact: true })
    .click();
  assert.equal(
    await page
      .locator('[data-action="filter-state"][aria-pressed="true"]')
      .getAttribute("data-filter"),
    "all",
  );
  await page
    .getByRole("button", { name: "renamed-worker", exact: true })
    .waitFor();
  await page.locator('.brand[href="#overview"]').click();
  await page.locator(".gateway-card").waitFor();

  // A failed read must flag the cached view instead of presenting it as current.
  await page.route("**/console/api/status", (route) => route.abort());
  await page.locator(".heading-actions [data-action=refresh]").click();
  await page.locator("#stale-banner").waitFor({ state: "visible" });
  await page.unroute("**/console/api/status");
  await page.locator(".heading-actions [data-action=refresh]").click();
  await page.locator("#stale-banner").waitFor({ state: "hidden" });

  // Exercise table density with explicitly synthetic records, including long
  // names and missing metrics. Only this browser's read response is replaced.
  const snapshot = await api("/v1/status", "GET", undefined, account.token);
  await assertOverviewFullFleet(page, snapshot);
  await assertCustomMachineOrder(page, snapshot);
  await assertMachineVersionLayout(page, snapshot);
  const fixtureNames = [
    "build-linux-01",
    "design-mac",
    "render-windows",
    "staging-linux",
    "test-runner-with-a-long-routing-name",
    "workstation",
  ];
  const visualSnapshot = {
    ...snapshot,
    nodes: fixtureNames.map((name, i) => ({
      ...snapshot.nodes[0],
      id: "visual-fixture-" + i,
      name,
      os: ["linux", "darwin", "windows"][i % 3],
      runAs: i === 5 ? undefined : i % 2 === 0 ? "system" : "user",
      online: i !== 3,
      wakeVia: i === 3 ? "build-linux-01" : undefined,
      status: i === 3 ? "offline" : "summary",
      activeCount: i === 2 ? 3 : 0,
      disabled: i === 4,
      leased: false,
      software: { version: "v0.8.2", arch: "arm64" },
      system: {
        arch: "arm64",
        cpuUsagePercent: i === 5 ? undefined : [12, 4, 87, 0, 2][i],
        memoryTotalBytes: 32 * 1024 ** 3,
        memoryUsedBytes: (i + 3) * 1024 ** 3,
      },
    })),
  };
  await page.route("**/console/api/status", (route) =>
    route.fulfill({ json: visualSnapshot }),
  );
  await page.locator(".heading-actions [data-action=refresh]").click();
  await page
    .getByRole("button", { name: "build-linux-01", exact: true })
    .waitFor();
  await page.locator("#toast").waitFor({ state: "hidden" });
  assert.equal(await page.locator(".machine-table tbody tr").count(), 4, "Overview must hide offline and disabled machines");
  assert.equal(await page.getByRole("button", { name: "staging-linux", exact: true }).count(), 0);
  assert.equal(await page.getByRole("button", { name: "test-runner-with-a-long-routing-name", exact: true }).count(), 0);
  assert.equal(await page.locator('.stat[data-filter="online"] .stat-value').textContent(), "5", "Overview summary counts must retain the full fleet");
  assert.equal(await page.locator('.stat[data-filter="offline"] .stat-value').textContent(), "1");
  await page.locator('.panel-heading [data-action="filter-machines"][data-filter="all"]').click();
  await page.locator("#machine-search").waitFor();
  assert.equal(await page.locator(".machine-table tbody tr").count(), 6, "All machines must include offline and disabled machines");
  await assertContextChips(page);
  await page.locator('[data-action="sort"][data-sort="cpu"]').click();
  assert.equal(
    await page.locator(".machine-name").first().textContent(),
    "render-windows",
  );
  assert.equal(
    await page
      .locator('.machine-table th[aria-sort="descending"]')
      .textContent(),
    "CPU↓",
  );
  await page.locator('[data-action="sort"][data-sort="cpu"]').click();
  assert.equal(
    await page.locator(".machine-name").first().textContent(),
    "test-runner-with-a-long-routing-name",
  );
  assert.equal(
    await page.locator(".machine-name").last().textContent(),
    "workstation",
  );
  await page.locator('[data-action="sort"][data-sort="name"]').click();
  const offline = page.getByRole("button", {
    name: "staging-linux",
    exact: true,
  });
  await offline.dispatchEvent("contextmenu", { clientX: 1438, clientY: 998 });
  await page.getByRole("menuitem", { name: "Wake", exact: true }).waitFor();
  const menuBox = await page.locator("#machine-menu").boundingBox();
  assert.ok(
    menuBox.x >= 8 &&
      menuBox.y >= 8 &&
      menuBox.x + menuBox.width <= 1432 &&
      menuBox.y + menuBox.height <= 992,
    "Context menu extends beyond the viewport",
  );
  await page.keyboard.press("Escape");
  await page
    .getByRole("button", {
      name: "Actions for test-runner-with-a-long-routing-name",
      exact: true,
    })
    .click();
  await page.getByRole("menuitem", { name: "Enable", exact: true }).waitFor();
  await page.locator("#page-title").click();
  assert.equal(await page.locator("#machine-menu").isVisible(), false);
  await page.locator('.brand[href="#overview"]').click();
  if (process.env.CONTROL_CONSOLE_SCREENSHOTS) {
    await mkdir(process.env.CONTROL_CONSOLE_SCREENSHOTS, { recursive: true });
    await page.screenshot({
      path: path.join(
        process.env.CONTROL_CONSOLE_SCREENSHOTS,
        "overview-dark.png",
      ),
      fullPage: true,
    });
  }
  await page.locator("#account-button").click();
  await page.locator('#account-menu [data-theme-choice="light"]').click();
  assert.equal(await page.locator("html").getAttribute("data-theme"), "light");
  await assertContextChips(page);
  await assertResourceIcons(page, true);
  await assertResourceIcons(page);
  await page.keyboard.press("Escape");
  await page.locator("#account-menu").waitFor({ state: "hidden" });
  if (process.env.CONTROL_CONSOLE_SCREENSHOTS)
    await page.screenshot({
      path: path.join(
        process.env.CONTROL_CONSOLE_SCREENSHOTS,
        "overview-light.png",
      ),
      fullPage: true,
    });
  for (const width of [320, 768, 1024]) {
    await page.setViewportSize({ width, height: 900 });
    assert.ok(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= innerWidth,
      ),
      `Page overflows at ${width}px`,
    );
  }
  await page.setViewportSize({ width: 390, height: 844 });
  await page
    .getByRole("button", { name: "Actions for build-linux-01", exact: true })
    .click();
  await page
    .getByRole("menuitem", { name: "View details", exact: true })
    .click();
  await page.locator("dialog.machine-drawer").waitFor();
  await page.getByRole("button", { name: "Close dialog", exact: true }).click();
  assert.equal(await page.locator(".profile").isVisible(), true);
  assert.equal(
    await page.locator(".machine-table .cpu-column").first().isVisible(),
    true,
  );
  await page.evaluate(
    () =>
      new Promise((resolve) =>
        requestAnimationFrame(() => requestAnimationFrame(resolve)),
      ),
  );
  assert.ok(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
    "Mobile page overflows horizontally",
  );
  if (process.env.CONTROL_CONSOLE_SCREENSHOTS)
    await page.screenshot({
      path: path.join(
        process.env.CONTROL_CONSOLE_SCREENSHOTS,
        "overview-mobile.png",
      ),
      fullPage: true,
    });
  await page
    .getByRole("button", { name: "build-linux-01", exact: true })
    .click();
  await page.getByText("CPU utilization", { exact: true }).waitFor();
  await page.getByText("Memory usage", { exact: true }).waitFor();
  assert.equal(await page.locator("dialog.machine-drawer").isVisible(), true);
  if (process.env.CONTROL_CONSOLE_SCREENSHOTS)
    await page.screenshot({
      path: path.join(
        process.env.CONTROL_CONSOLE_SCREENSHOTS,
        "machine-mobile.png",
      ),
      fullPage: true,
    });
  await page.getByRole("button", { name: "Close dialog", exact: true }).click();
  await page.unroute("**/console/api/status");
  await page.setViewportSize({ width: 1440, height: 1000 });
  const mutationsBeforeStartup = machineMutations;
  await pageLink(page, "machines");
  await assertStartupCommands(page, snapshot);
  await page.locator('.brand[href="#overview"]').click();
  await assertHybridGrouping(page, snapshot);
  await assertAcceleratorPresence(page, snapshot);
  await assertProxyChips(page, snapshot);
  await assertProxyChoicesRecoverInOpenForm(page, snapshot);
  assert.equal(machineMutations, mutationsBeforeStartup, "Preparing startup commands must not mutate machine lifecycle policy");
  await assertHybridUnregister(page, snapshot);

  await page.locator("#account-button").click();
  await page.locator('#account-menu a[href="#account"]').click();
  await page
    .locator("#account-form input[name=currentPassword]")
    .fill(password);
  await page
    .locator("#account-form input[name=password]")
    .fill("new isolated browser password");
  await page
    .locator("#account-form input[name=confirm]")
    .fill("new isolated browser password");
  await page.locator("#account-form button[type=submit]").click();
  await page.locator("#login").waitFor({ state: "visible" });
  await login(page, "browser-owner", "new isolated browser password");
  await page.locator("#account-button").click();
  await page.locator("#account-menu [data-action=logout]").click();
  await page.locator("#modal-form button[type=submit]").click();
  await page.locator("#login").waitFor({ state: "visible" });
  if (process.env.CONTROL_CONSOLE_SCREENSHOTS)
    await page.screenshot({
      path: path.join(process.env.CONTROL_CONSOLE_SCREENSHOTS, "login.png"),
      fullPage: true,
    });

  await login(page, "@operator");
  await pageLink(page, "users");
  await page.getByText("browser-owner", { exact: true }).waitFor();
  await pageLink(page, "updates");
  await page.getByText("Selected version", { exact: true }).waitFor();
  assert.deepEqual(errors, []);
  console.log(
    "Console browser smoke passed: login, fleet isolation, search, machine context menus, confirmed lifecycle actions, rename, escaped keys, enrollment commands, confirmed invitation replacement, stale views, themes, mobile layout, password change, logout, and operator pages.",
  );
} catch (error) {
  console.error(logs.join(""));
  if (browser) {
    for (const context of browser.contexts()) {
      for (const page of context.pages()) {
        console.error(await page.locator("body").innerText());
        await page.screenshot({
          path: path.join(dir, "failure.png"),
          fullPage: true,
        });
      }
    }
  }
  throw error;
} finally {
  if (browser) await browser.close();
  for (const { child } of processes.reverse()) child.kill("SIGTERM");
  await Promise.all(
    processes.map(async ({ child, exited }) => {
      const timer = setTimeout(() => child.kill("SIGKILL"), 10000);
      try {
        await exited;
      } finally {
        clearTimeout(timer);
      }
    }),
  );
  // Leave isolated state for failure inspection, never delete broad temp paths.
  console.log("Fixture state: " + dir);
}
