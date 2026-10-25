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

async function assertContextChips(page) {
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

async function assertResourceIcons(page, gateway = false) {
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
    ["CPU", "Memory", gateway ? "State disk" : "Disk"].map((label) => ({
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
        ? 'powershell.exe -NoProfile -c "irm \'https://example.test/install/fixture\'|iex"'
        : "curl -fsSL 'https://example.test/install/fixture' | " + (body.serviceMode === "system" ? "sudo bash" : "bash"),
    } });
  };
  await page.route("**/console/api/status", statusRoute);
  await page.route("**/console/api/fleet/installations", installationsRoute);
  const selectMode = async (mode) => {
    await page.locator('#modal-form select[name="serviceMode"]').locator("xpath=preceding-sibling::button[1]").click();
    await page.locator(`[role=option][data-value="${mode}"]`).click();
  };
  const refresh = async () => {
    await page.waitForFunction(() => !state.busy);
    await page.evaluate(() => refresh(true));
  };
  const open = async (os, drawer = false) => {
    nodes = [{ ...original, os }];
    await refresh();
    await page.getByRole("button", { name: "Actions for startup-worker", exact: true }).click();
    if (drawer) {
      await page.getByRole("menuitem", { name: "View details", exact: true }).click();
      await page.getByRole("button", { name: "Change startup context", exact: true }).click();
    } else await page.getByRole("menuitem", { name: "Change startup context", exact: true }).click();
  };
  for (const os of ["windows", "linux", "darwin"]) {
    requests = [];
    await open(os, os === "windows");
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
  await page.unroute("**/console/api/fleet/installations", installationsRoute);
  await refresh();
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
  await assertResourceIcons(page, true);
  await assertResourceIcons(page);
  await pageLink(page, "machines");
  await assertResourceIcons(page);
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
    ...(registered.runAs === "user" ? ["Change startup context"] : []),
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
  await assertStartupCommands(page, snapshot);
  assert.equal(machineMutations, mutationsBeforeStartup, "Preparing startup commands must not mutate machine lifecycle policy");

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
