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
      assert.deepEqual(await menu.getByRole("menuitem").allTextContents(), ["Startup context", "Unregister"]);
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
  assert.deepEqual(await page.locator("#machine-menu").getByRole("menuitem").allTextContents(), ["Unregister"]);
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
  await assertBrandAssets(page);
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
    ...(registered.runAs === "user" ? ["Startup context"] : []),
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
