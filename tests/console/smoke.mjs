// Optional browser verification. All state, credentials, and processes belong to
// this fixture. No saved Control login, real fleet, or desktop profile is used.
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { createHash } from "node:crypto";
import { mkdtemp, writeFile, mkdir } from "node:fs/promises";
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
      workDir: ".",
      listen: "127.0.0.1:" + nodePort,
      allow: {},
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
  await page
    .getByRole("button", { name: "browser-worker", exact: true })
    .waitFor();
  assert.equal(await page.locator("#operator-nav").isVisible(), false);
  await pageLink(page, "machines");
  await page.locator("#machine-search").fill("not-a-real-machine");
  await page.getByText("No matching machines", { exact: true }).waitFor();
  await page.locator("#machine-search").fill("browser-worker");
  await page
    .getByRole("button", { name: "browser-worker", exact: true })
    .click();
  await page.getByRole("button", { name: "Rename", exact: true }).click();
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

  const os = await publishInstallers();
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
      online: i !== 3,
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
    "Console browser smoke passed: login, fleet isolation, search, rename, escaped keys, enrollment commands, confirmed invitation replacement, stale views, themes, mobile layout, password change, logout, and operator pages.",
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
