// @ts-check
"use strict";
// All shipped JavaScript is checked by tests/console/tsconfig.json.
/**
 * API projections follow internal/model, internal/gateway and internal/update.
 * Optional readings stay unknown rather than becoming zero in the UI.
 * @typedef {{username: string, role: string, userId: string, csrf: string}} ConsoleSession
 * @typedef {{version: string, arch: string, os: string}} Software
 * @typedef {{path: string, totalBytes: number, usedBytes: number, freeBytes: number, error?: string}} DiskUsage
 * @typedef {{name: string, kind?: string, vendor?: string, memoryTotalBytes?: number, memoryUsedBytes?: number, usagePercent?: number}} GPUInfo
 * @typedef {object} SystemInfo
 * @property {string} os
 * @property {string} arch
 * @property {string} [platform]
 * @property {string} [platformVersion]
 * @property {string} [cpuModel]
 * @property {number} logicalCPUs
 * @property {number} [cpuUsagePercent]
 * @property {number} memoryUsedBytes
 * @property {number} memoryTotalBytes
 * @property {DiskUsage[]} disks
 * @property {GPUInfo[]} [gpus]
 * @property {string} sampledAt
 * @typedef {object} Machine
 * @property {string} id
 * @property {string} name
 * @property {string} os
 * @property {boolean} online
 * @property {string} lastSeen
 * @property {string} status
 * @property {Software} [software]
 * @property {SystemInfo} [system]
 * @property {Record<string, string>} [labels]
 * @property {boolean} [disabled]
 * @property {boolean} [controlPending]
 * @property {string} [updateState]
 * @property {number} activeCount
 * @property {boolean} [leased]
 * @property {string} [leaseExpires]
 * @property {string} [lastActive]
 * @property {{tasks: number, exec: number, transfers: number, other: number}} [work]
 * @property {{forward: number, reverse: number}} [tunnels]
 * @property {{direct: number, turn: number, relay: number, unknown: number}} [connectionRoutes]
 * @property {string} [runAs]
 * @property {string} [hybridId]
 * @property {string} [hybridRole]
 * @property {string} [wakeVia]
 * @property {Machine[]} [contexts] Presentation-only grouping, never sent to the API.
 * @property {string} [targetName]
 * @typedef {Machine & {contexts: Machine[]}} MachineGroup
 * @typedef {{observedAt: string, nodes: Machine[], gateway?: {url: string, startedAt: string, software: Software, system?: SystemInfo}}} FleetSnapshot
 * @typedef {{id: string, name: string, autoName?: boolean, expiresAt: string, revoked?: boolean, redeemedId?: string, serviceMode?: string, hybridId?: string, hybridRole?: string, asset?: {os: string}}} Invitation
 * @typedef {Invitation & {command: string}} InstallationLink
 * @typedef {{id: string, name: string, role: string, createdAt: string, revoked?: boolean}} FleetKey
 * @typedef {{id: string, name: string, createdAt: string, disabled?: boolean}} FleetUser
 * @typedef {{id: number, at: string, userId: string, fleet?: string, actor: string, via: string, keyId?: string, action: string, target?: string, targetId?: string, detail?: string, sourceIp?: string}} AuditEvent
 * @typedef {{events: AuditEvent[], next?: number}} AuditPage
 * @typedef {{state?: string, software?: Software, paused?: boolean, error?: string}} UpdateStatus
 * @typedef {{deployment?: {id: string, manifest: {version: string}, phase: string, source: string, updatedAt: string, error?: string, gatewayState?: string, deferred?: string[]}, gatewayUpdate?: {state: string, relayCarriers?: number}, nodes: Record<string, UpdateStatus>, nodeProgress?: Record<string, string>}} UpdateResult
 * @typedef {{status: string, reason?: string}} StartupResult
 * @typedef {{key: string, secrets: {name: string, updatedAt: string}[]}} SecretList
 * @typedef {{id: string, expiresAt: number, key: string, nonce: string, ciphertext: string}} SecretEnvelope
 * @typedef {{removed: number, hidden?: number}} ClearResult
 * @typedef {{token: string}} IssuedKey
 * @typedef {{name: string, autoName?: boolean, os: string, serviceMode: string, ttlSeconds: number, gateway: string, force: boolean}} InstallationRequest
 * @typedef {{login: {username: string, password: string}, logout: never, 'fleet/installations': InstallationRequest, 'fleet/keys': {name: string}, 'admin/users': {name: string}, 'admin/updates/check': never} & Record<`fleet/nodes/${string}/startup`, {mode: string}> & Record<`fleet/nodes/${string}/wake`, never>} PostBody
 * @typedef {{'console/password': {username?: string, currentPassword?: string, password: string}} & Record<`fleet/nodes/${string}/secrets/${string}`, SecretEnvelope>} PutBody
 * @typedef {Record<`fleet/nodes/${string}`, {name?: string, force?: boolean, disabled?: boolean}> & Record<`fleet/keys/${string}`, {name: string}>} PatchBody
 * @typedef {{session: ConsoleSession, status: FleetSnapshot, 'fleet/installations': Invitation[], 'fleet/keys': FleetKey[], 'admin/users': FleetUser[], 'admin/updates': UpdateResult} & Record<`audit?${string}`, AuditPage> & Record<`fleet/nodes/${string}/secrets`, SecretList>} GetAPI
 * @typedef {{login: ConsoleSession, logout: null, 'fleet/installations': InstallationLink, 'fleet/keys': IssuedKey, 'admin/users': IssuedKey, 'admin/updates/check': unknown} & Record<`fleet/nodes/${string}/startup`, StartupResult> & Record<`fleet/nodes/${string}/wake`, {source: string}>} PostAPI
 * @typedef {{'fleet/installations': ClearResult} & Record<`audit${string}`, ClearResult> & Record<`fleet/nodes/${string}` | `fleet/keys/${string}` | `fleet/installations/${string}` | `admin/users/${string}`, unknown>} DeleteAPI
 * @typedef {{page: 'invitations', data: Invitation[]} | {page: 'keys', data: FleetKey[]} | {page: 'users', data: FleetUser[]} | {page: 'updates', data: UpdateResult} | {page: 'activity', data: AuditPage}} Auxiliary
 * @typedef {object} ConsoleState
 * @property {ConsoleSession | null} session
 * @property {FleetSnapshot | null} snapshot
 * @property {string} page
 * @property {string} auditCategory
 * @property {string} search
 * @property {string} filter
 * @property {string} sort
 * @property {boolean} descending
 * @property {boolean} busy
 * @property {Auxiliary | null} auxiliary
 * @property {InstallationLink | null} invitation
 * @property {{id: string, startedAt: number} | null} startupChange
 * @property {Map<string, string>} contexts
 * @property {boolean} stale
 * @property {string | null} updatedAt
 * @typedef {[number, number]} Point
 * @typedef {{a: number, b: number, length: number}} MeshEdge
 * @typedef {Record<string, string>} MeshTiming
 * @typedef {[SVGElement, MeshTiming]} AnimatedPeer
 * @typedef {HTMLSelectElement & {syncListbox?: () => void}} EnhancedSelect
 * @typedef {{'#dialog': HTMLDialogElement, '#login-form': HTMLFormElement, '#modal-form': HTMLFormElement, '#machine-search': HTMLInputElement, '#entry-reference': HTMLTextAreaElement, '#entry-content': HTMLTextAreaElement}} ConsoleElements
 */
/**
 * @template {string} S
 * @typedef {S extends keyof ConsoleElements ? ConsoleElements[S] : S extends `input[${string}` | `${string} input[${string}` ? HTMLInputElement : S extends `button[${string}` ? HTMLButtonElement : S extends `${string} select[${string}` ? HTMLSelectElement : S extends keyof HTMLElementTagNameMap ? HTMLElementTagNameMap[S] : HTMLElement} SelectedElement
 */

/**
 * Required DOM lookup. Use find() for elements that may be absent.
 * @template {string} S
 * @param {S} selector
 * @param {ParentNode} [root]
 * @returns {SelectedElement<S>}
 */
function $(selector, root = document) {
  const element = find(selector, root);
  if (!element) throw new Error("Missing console element: " + selector);
  return element;
}
/**
 * @template {string} S
 * @param {S} selector
 * @param {ParentNode} [root]
 * @returns {SelectedElement<S> | null}
 */
function find(selector, root = document) {
  return /** @type {SelectedElement<S> | null} */ (root.querySelector(selector));
}
/** @param {unknown} error @returns {string} */
function errorMessage(error) {
  return error instanceof Error ? error.message : "Unexpected console error.";
}
/** @param {HTMLFormElement} form @returns {Record<string, string>} */
function formFields(form) {
  /** @type {Record<string, string>} */
  const fields = {};
  for (const [name, value] of new FormData(form)) {
    if (typeof value !== "string") throw new Error("File fields are not supported.");
    fields[name] = value;
  }
  return fields;
}
/** @param {unknown} value @returns {string} */
const esc = (value) =>
  String(value ?? "").replace(
    /[&<>"']/g,
    (c) =>
      (/** @type {Record<string, string>} */ ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }))[
        c
      ],
  );
// Static paths only. Fleet-provided values never select SVG markup.
/** @type {Record<string, string>} */
const icons = {
  machine:
    '<rect x="3" y="4" width="18" height="13" rx="2"/><path d="M8 21h8m-4-4v4"/>',
  laptop:
    '<rect x="5" y="3" width="14" height="13" rx="1"/><path d="m5 16-3 4h20l-3-4"/>',
  windows: '<path d="M3 4h8v7H3zm10 0h8v7h-8zM3 13h8v7H3zm10 0h8v7h-8z"/>',
  invitation:
    '<rect x="3" y="5" width="18" height="14" rx="2"/><path d="m3 6 9 7 9-7"/>',
  key: '<circle cx="8" cy="8" r="5"/><path d="m12 12 9 9m-3-3 3-3m-6 0 3-3"/>',
  users:
    '<circle cx="9" cy="7" r="3"/><path d="M3 21v-3a6 6 0 0 1 12 0v3m2-17a3 3 0 0 1 0 6m1 5a5 5 0 0 1 3 5v1"/>',
  update: '<path d="M12 16V3m-5 5 5-5 5 5M4 15v6h16v-6"/>',
  settings:
    '<path d="M4 7h16M4 17h16"/><circle cx="9" cy="7" r="3"/><circle cx="15" cy="17" r="3"/>',
  gateway:
    '<rect x="3" y="3" width="18" height="7" rx="2"/><rect x="3" y="14" width="18" height="7" rx="2"/><path d="M7 6.5h.01M7 17.5h.01M16 6.5h2M16 17.5h2"/>',
  cpu:
    '<rect x="5" y="5" width="14" height="14" rx="2"/><rect x="9" y="9" width="6" height="6" rx="1"/><path d="M9 2v3m6-3v3M9 19v3m6-3v3M2 9h3m-3 6h3m14-6h3m-3 6h3"/>',
  memory:
    '<rect x="3" y="6" width="18" height="12" rx="2"/><path d="M7 10v4m5-4v4m5-4v4M7 18v3m5-3v3m5-3v3"/>',
  disk:
    '<rect x="3" y="5" width="18" height="14" rx="2"/><path d="M3 14h18m-5 2.5h.01m2.99 0h.01"/>',
  theme:
    '<circle cx="12" cy="12" r="9"/><path d="M12 3a9 9 0 0 1 0 18Z" fill="currentColor" stroke="none"/>',
  refresh:
    '<path d="M3 12a9 9 0 0 1 9-9c2.52 0 4.93 1 6.74 2.74L21 8M21 3v5h-5M21 12a9 9 0 0 1-9 9c-2.52 0-4.93-1-6.74-2.74L3 16M8 16H3v5"/>',
  plus: '<path d="M12 5v14M5 12h14"/>',
  edit: '<path d="m16 3 5 5M4 20l4-1L21 6a2.1 2.1 0 0 0-3-3L5 16l-1 4Z"/>',
  trash: '<path d="M3 6h18M9 6V3h6v3M5 6l1 15h12l1-15M10 10v7m4-7v7"/>',
  close: '<path d="m6 6 12 12M6 18 18 6"/>',
  arrow: '<path d="M4 12h16m-6-6 6 6-6 6"/>',
  copy: '<rect x="9" y="9" width="12" height="12" rx="2"/><path d="M5 15H4a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1h10a1 1 0 0 1 1 1v1"/>',
  more: '<circle cx="5" cy="12" r="1"/><circle cx="12" cy="12" r="1"/><circle cx="19" cy="12" r="1"/>',
  search: '<circle cx="10" cy="10" r="6"/><path d="m15 15 6 6"/>',
  chevron: '<path d="m7 10 5 5 5-5"/>',
  check: '<path d="m5 12 5 5 9-10"/>',
  direct: '<path d="M4 8h15m-3-3 3 3-3 3M20 16H5m3-3-3 3 3 3"/>',
  relay:
    '<circle cx="12" cy="12" r="3"/><path d="M3 12h6m6 0h6M12 3v6m0 6v6"/>',
  tunnel: '<path d="M4 20V11a8 8 0 0 1 16 0v9M9 20v-5a3 3 0 0 1 6 0v5"/>',
  activity: '<path d="M3 12h4l3-8 4 16 3-8h4"/>',
  logout:
    '<path d="M15 4h4a1 1 0 0 1 1 1v14a1 1 0 0 1-1 1h-4M10 17l5-5-5-5m5 5H3"/>',
};
/** @param {string | undefined} name @returns {string} */
function icon(name) {
  return `<svg class="icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${icons[name || "machine"] || icons.machine}</svg>`;
}
/** @type {NodeListOf<HTMLElement>} */ (document.querySelectorAll("[data-icon]")).forEach((element) => {
  element.innerHTML = icon(element.dataset.icon);
});
/** @type {ConsoleState} */
const state = {
  session: null,
  snapshot: null,
  page: "overview",
  auditCategory: "",
  search: "",
  filter: "all",
  sort: "name",
  descending: false,
  busy: false,
  auxiliary: null,
  invitation: null,
  startupChange: null,
  contexts: new Map(),
  stale: false,
  updatedAt: null,
};
/** @type {Record<string, [string, string]>} */
const pages = {
  overview: ["Overview", ""],
  machines: ["Machines", "Resources and access for your registered machines."],
  invitations: [
    "Invitations",
    "One-time installation links and enrollment status.",
  ],
  keys: ["Access keys", "Credentials for machine enrollment and discovery."],
  account: ["Account", "Manage your browser login."],
  activity: ["Activity", "Sign-ins and management changes, newest first."],
  users: ["Accounts", "Private fleets and browser access on this gateway."],
  updates: ["Updates", "Software releases and gateway-wide rollout status."],
};
const lightScheme = matchMedia("(prefers-color-scheme: light)");
let themeChoice = "system";
try {
  themeChoice = localStorage.getItem("control-theme") || "system";
} catch {
  /* Storage is optional. */
}
/** @returns {void} */
function applyTheme() {
  document.documentElement.dataset.theme =
    themeChoice === "system"
      ? lightScheme.matches
        ? "light"
        : "dark"
      : themeChoice;
  /** @type {NodeListOf<HTMLElement>} */ (document.querySelectorAll("[data-theme-choice]")).forEach((button) => {
    button.setAttribute(
      "aria-pressed",
      String(button.dataset.themeChoice === themeChoice),
    );
  });
}
applyTheme();
lightScheme.addEventListener("change", applyTheme);
// Chrome reveals focus on a mouse-focused element after any key press, so
// modifier shortcuts such as Cmd+Shift+4 screenshots would show a ring.
document.addEventListener("keydown", (event) => {
  if (
    event.metaKey ||
    event.ctrlKey ||
    ["Meta", "Control", "Shift", "Alt", "CapsLock"].includes(event.key)
  )
    return;
  document.documentElement.dataset.keyboard = "";
});
document.addEventListener("pointerdown", () => {
  delete document.documentElement.dataset.keyboard;
});
// The menu opens below its button; popovers otherwise center in the viewport.
$("#account-menu").addEventListener("beforetoggle", (event) => {
  if (event.newState !== "open") return;
  const box = $("#account-button").getBoundingClientRect();
  $("#account-menu").style.top = box.bottom + 6 + "px";
  $("#account-menu").style.right =
    document.documentElement.clientWidth - box.right + "px";
});

$("#nav-more-menu").addEventListener("beforetoggle", (event) => {
  if (event.newState !== "open") return;
  const box = $("#nav-more").getBoundingClientRect();
  $("#nav-more-menu").style.top = box.bottom + 6 + "px";
  $("#nav-more-menu").style.left = Math.max(12, box.left - 8) + "px";
});
// Links that do not fit move, from the end, into the "More" menu.
/** @returns {void} */
function fitNav() {
  const nav = $("nav"),
    more = $("#nav-more"),
    operator = $("#operator-nav");
  const links = [.../** @type {NodeListOf<HTMLAnchorElement>} */ (nav.querySelectorAll("a[data-page]"))].filter(
    (a) => !operator.hidden || !operator.contains(a),
  );
  links.forEach((a) => a.classList.remove("nav-hidden"));
  operator.classList.remove("nav-hidden");
  more.hidden = true;
  /** @type {HTMLAnchorElement[]} */
  const overflowed = [];
  if (nav.scrollWidth > nav.clientWidth) {
    more.hidden = false;
    while (
      nav.scrollWidth > nav.clientWidth &&
      overflowed.length < links.length
    ) {
      const link = links[links.length - 1 - overflowed.length];
      link.classList.add("nav-hidden");
      overflowed.unshift(link);
    }
    // Drop the operator divider once all of its links have moved.
    operator.classList.toggle(
      "nav-hidden",
      [...operator.querySelectorAll("a")].every((a) => overflowed.includes(a)),
    );
  }
  $("#nav-more-menu").innerHTML = overflowed
    .map((a) => {
      const label = [...a.childNodes]
        .filter((node) => node.nodeType === Node.TEXT_NODE)
        .map((node) => node.textContent)
        .join("")
        .trim();
      return `<a href="${a.getAttribute("href")}" data-page="${a.dataset.page}" class="menu-item${a.dataset.page === state.page ? " active" : ""}">${a.querySelector("[data-icon]")?.innerHTML || ""}${esc(label)}</a>`;
    })
    .join("");
  more.classList.toggle(
    "active",
    overflowed.some((a) => a.dataset.page === state.page),
  );
}
new ResizeObserver(fitNav).observe($(".topbar"));

/**
 * @template {keyof GetAPI} P
 * @overload
 * @param {P} path
 * @returns {Promise<GetAPI[P]>}
 */
/**
 * @template {keyof PostAPI} PP
 * @overload
 * @param {PP} path
 * @param {'POST'} method
 * @param {PostBody[PP]} [body]
 * @returns {Promise<PostAPI[PP]>}
 */
/**
 * @template {keyof DeleteAPI} DP
 * @overload
 * @param {DP} path
 * @param {'DELETE'} method
 * @returns {Promise<DeleteAPI[DP]>}
 */
/**
 * @template {keyof PutBody} UP
 * @overload
 * @param {UP} path
 * @param {'PUT'} method
 * @param {PutBody[UP]} body
 * @returns {Promise<unknown>}
 */
/**
 * @template {keyof PatchBody} MP
 * @overload
 * @param {MP} path
 * @param {'PATCH'} method
 * @param {PatchBody[MP]} body
 * @returns {Promise<unknown>}
 */
/**
 * JSON is trusted only at this typed, authenticated API boundary.
 * @param {string} path
 * @param {string} [method]
 * @param {unknown} [body]
 * @returns {Promise<unknown>}
 */
async function api(path, method = "GET", body) {
  /** @type {Record<string, string>} */
  const headers = { Accept: "application/json" };
  if (body !== undefined) headers["Content-Type"] = "application/json";
  if (method !== "GET") headers["X-CSRF-Token"] = state.session?.csrf || "";
  /** @type {Response} */
  let response;
  try {
    response = await fetch("/console/api/" + path, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
      credentials: "same-origin",
      cache: "no-store",
      signal: AbortSignal.timeout(
        path === "admin/updates/check" ? 180000 : 20000,
      ),
    });
  } catch {
    throw new Error(
      method === "GET"
        ? "Gateway unreachable. Showing the last available snapshot."
        : "The response was lost. This action may have completed. Refresh and inspect its status before trying again.",
    );
  }
  if (!response.ok) {
    const message = (await response.text()).slice(0, 4096).trim();
    if (response.status === 401 && path !== "login" && state.session)
      showLogin();
    const error = /** @type {Error & {status?: number}} */ (new Error(message || "Request failed."));
    error.status = response.status;
    throw error;
  }
  if (response.status === 204) return null;
  return /** @type {Promise<unknown>} */ (response.json());
}

/** @returns {void} */
function showLogin() {
  closeMachineMenu();
  state.session = null;
  state.snapshot = null;
  state.auxiliary = null;
  state.invitation = null;
  state.contexts.clear();
  state.search = "";
  state.filter = "all";
  state.stale = false;
  state.updatedAt = null;
  banner("");
  closeDialog();
  $("#page-content").replaceChildren();
  $("#app").hidden = true;
  $("#login").hidden = false;
  drawMesh();
  history.replaceState(null, "", "/login");
  $("input[name=password]", $("#login-form")).value = "";
  $("#login-form input[name=username]").focus();
}

/** @param {ConsoleSession} session @returns {void} */
function showApp(session) {
  state.session = session;
  $("#login").hidden = true;
  $("#app").hidden = false;
  $("#profile-name").textContent = session.username;
  $("#menu-name").textContent = session.username;
  $("#menu-role").textContent =
    session.role === "superuser" ? "Gateway operator" : "Fleet owner";
  $("#avatar").textContent = session.username
    .replace("@", "")
    .slice(0, 1)
    .toUpperCase();
  $("#operator-nav").hidden = session.role !== "superuser";
  fitNav();
  history.replaceState(null, "", "/console" + (location.hash || "#overview"));
  navigate();
}

$("#login-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const form = $("#login-form"),
    button = $("button[type=submit]", form);
  const fields = formFields(form);
  button.disabled = true;
  form.setAttribute("aria-busy", "true");
  $(".login-submit", form).textContent = "Signing in…";
  form.classList.remove("rejected");
  $("#login-error").textContent = "";
  try {
    const session = await api("login", "POST", {
      username: fields.username,
      password: fields.password,
    });
    form.reset();
    showApp(session);
  } catch (error) {
    $("#login-error").textContent = errorMessage(error);
    form.classList.add("rejected");
  } finally {
    button.disabled = false;
    form.removeAttribute("aria-busy");
    $(".login-submit", form).textContent = "Sign in";
  }
});
$("#login-form").addEventListener("animationend", (event) => {
  if (event.animationName === "shake")
    $("#login-form").classList.remove("rejected");
});

// The sign-in backdrop is a new random peer mesh on every view. A few hubs are
// scattered away from the card; node density falls off from them as a sum of
// Gaussians. Variable-radius Poisson-disk sampling places nodes, dense near
// hubs and sparse between them, and links form their relative neighborhood
// graph, which is connected and planar, so sparse regions get long bridges.
// Packets favor busy links near hubs and stay outside the card.
const svgNS = "http://www.w3.org/2000/svg";
const meshFlows = 8;

/** @param {string} name @param {Record<string, string | number>} attributes @returns {SVGElement} */
function svgElement(name, attributes) {
  const element = document.createElementNS(svgNS, name);
  for (const [key, value] of Object.entries(attributes))
    element.setAttribute(key, String(value));
  return element;
}

// Dart throwing around active samples (Bridson) with a spacing that varies
// by position. Meshes stay under a few hundred nodes, so a direct scan is fast.
/** @param {number} width @param {number} height @param {(point: Point) => number} spacing @param {Point[]} seeds @returns {Point[]} */
function poissonDisk(width, height, spacing, seeds) {
  const points = [...seeds],
    active = [...seeds];
  /** @param {Point} point @returns {boolean} */
  const fits = (point) =>
    points.every(
      (other) =>
        Math.hypot(other[0] - point[0], other[1] - point[1]) >=
        (spacing(point) + spacing(other)) / 2,
    );
  while (active.length) {
    const index = Math.floor(Math.random() * active.length),
      [x, y] = active[index],
      radius = spacing([x, y]);
    let placed = false;
    for (let attempt = 0; attempt < 30 && !placed; attempt++) {
      const angle = Math.random() * 2 * Math.PI,
        distance = radius * (1 + Math.random()),
        point = /** @type {Point} */ ([
          x + distance * Math.cos(angle),
          y + distance * Math.sin(angle),
        ]);
      if (
        point[0] >= 0 &&
        point[0] < width &&
        point[1] >= 0 &&
        point[1] < height &&
        fits(point)
      ) {
        points.push(point);
        active.push(point);
        placed = true;
      }
    }
    if (!placed) active.splice(index, 1);
  }
  return points;
}

// An edge survives when no third node is closer to both of its endpoints.
/** @param {Point[]} points @returns {MeshEdge[]} */
function neighborhoodGraph(points) {
  /** @param {number} a @param {number} b @returns {number} */
  const distance = (a, b) =>
    Math.hypot(points[a][0] - points[b][0], points[a][1] - points[b][1]);
  /** @type {MeshEdge[]} */
  const edges = [];
  for (let a = 0; a < points.length; a++)
    for (let b = a + 1; b < points.length; b++) {
      const length = distance(a, b);
      let empty = true;
      for (let c = 0; c < points.length && empty; c++)
        if (
          c !== a &&
          c !== b &&
          Math.max(distance(a, c), distance(b, c)) < length
        )
          empty = false;
      if (empty) edges.push({ a, b, length });
    }
  return edges;
}

/** @returns {void} */
function drawMesh() {
  const [base, motion] = document.querySelectorAll(".login-backdrop");
  // Layout offsets ignore the entrance transforms still running on the layers
  // and card. The layers extend 16px past the page on every side.
  const page = $("#login"),
    main = $(".login-main", page),
    card = $(".login-card", page);
  if (!page.clientWidth || !page.clientHeight) return;
  const width = page.clientWidth + 32,
    height = page.clientHeight + 32;
  for (const layer of [base, motion])
    layer.setAttribute("viewBox", `0 0 ${width} ${height}`);
  const left = main.offsetLeft + card.offsetLeft + 16 - 24,
    top = main.offsetTop + card.offsetTop + 16 - 24,
    right = left + card.offsetWidth + 48,
    bottom = top + card.offsetHeight + 48;
  /** @param {Point} point @returns {boolean} */
  const underCard = ([x, y]) => x > left && x < right && y > top && y < bottom;

  const diagonal = Math.hypot(width, height),
    sigma = diagonal * 0.11,
    separation = Math.min(width, height) * 0.38;
  /** @type {Point[]} */
  const hubs = [];
  for (let attempt = 0; attempt < 400 && hubs.length < 4; attempt++) {
    const hub = /** @type {Point} */ ([
      width * (0.06 + Math.random() * 0.88),
      height * (0.08 + Math.random() * 0.84),
    ]);
    if (
      !underCard(hub) &&
      hubs.every(
        (other) =>
          Math.hypot(other[0] - hub[0], other[1] - hub[1]) > separation,
      )
    )
      hubs.push(hub);
  }
  /** @param {Point} point @returns {number} */
  const influence = ([x, y]) =>
    Math.max(
      0,
      ...hubs.map(([hx, hy]) =>
        Math.exp(-((x - hx) ** 2 + (y - hy) ** 2) / (2 * sigma * sigma)),
      ),
    );
  const near = diagonal * 0.032,
    far = diagonal * 0.13,
    spacing = /** @param {Point} point */ (point) => far - (far - near) * influence(point);
  // Nodes may sit just outside the layer so links run off every edge.
  const margin = far,
    points = poissonDisk(
      width + margin * 2,
      height + margin * 2,
      ([x, y]) => spacing([x - margin, y - margin]),
      hubs.map(([x, y]) => [x + margin, y + margin]),
    ).map(([x, y]) => /** @type {Point} */ ([x - margin, y - margin]));
  const weight = points.map(influence),
    edges = neighborhoodGraph(points);

  // Packets prefer busy links near hubs; no two share an endpoint.
  /** @param {Point} point @returns {boolean} */
  const visible = ([x, y]) =>
    x > 24 && x < width - 24 && y > 24 && y < height - 24 && !underCard([x, y]);
  const candidates = edges
    .filter(({ a, b, length }) => {
      if (length < near * 1.2) return false;
      for (let t = 0; t <= 1; t += 0.1)
        if (
          !visible([
            points[a][0] + (points[b][0] - points[a][0]) * t,
            points[a][1] + (points[b][1] - points[a][1]) * t,
          ])
        )
          return false;
      return true;
    })
    .map((edge) => ({
      edge,
      key: Math.random() ** (1 / (0.15 + weight[edge.a] + weight[edge.b])),
    }))
    .sort((x, y) => y.key - x.key)
    .map(({ edge }) => edge);
  /** @type {Set<MeshEdge>} */
  const flows = new Set();
  /** @type {Set<number>} */
  const used = new Set();
  for (const edge of candidates) {
    if (flows.size === meshFlows) break;
    if (used.has(edge.a) || used.has(edge.b)) continue;
    flows.add(edge);
    used.add(edge.a);
    used.add(edge.b);
  }

  // Soft color pools and dot fields mark each hub's neighborhood.
  /** @type {SVGElement[]} */
  const pools = [];
  /** @type {SVGElement[]} */
  const fields = [];
  hubs.forEach(([x, y], index) => {
    const position = { cx: x.toFixed(1), cy: y.toFixed(1) };
    pools.push(
      svgElement("circle", {
        ...position,
        r: (sigma * 2.2).toFixed(1),
        fill: `url(#login-glow-${index % 2 ? "b" : "a"})`,
      }),
    );
    fields.push(
      svgElement("circle", {
        ...position,
        r: (sigma * 1.6).toFixed(1),
        fill: "url(#login-dot-fade)",
      }),
    );
  });

  /** @type {SVGElement[]} */
  const links = [];
  /** @type {SVGElement[]} */
  const nodes = [];
  /** @type {AnimatedPeer[]} */
  const packets = [];
  /** @type {AnimatedPeer[]} */
  const peers = [];
  /** @type {Map<number, MeshTiming[]>} */
  const targets = new Map();
  let order = 0;
  for (const edge of edges) {
    const from = points[edge.a],
      to = points[edge.b],
      strength = (weight[edge.a] + weight[edge.b]) / 2;
    links.push(
      svgElement("line", {
        x1: from[0].toFixed(1),
        y1: from[1].toFixed(1),
        x2: to[0].toFixed(1),
        y2: to[1].toFixed(1),
        "stroke-opacity": (0.14 + strength * 0.4).toFixed(2),
        class: edge.a < hubs.length || edge.b < hubs.length ? "spoke" : "",
      }),
    );
    if (!flows.has(edge)) continue;
    // Traffic mostly heads toward the busier end.
    const toward = weight[edge.b] > weight[edge.a] ? edge.b : edge.a,
      target = Math.random() < 0.7 ? toward : edge.a + edge.b - toward,
      source = edge.a + edge.b - target;
    // Staggered starts with independent periods keep packets out of step.
    const timing = {
      "--t": (5.5 + Math.random() * 4).toFixed(2) + "s",
      "--d": (0.8 + order++ * 0.7 + Math.random() * 0.6).toFixed(2) + "s",
    };
    const packet = svgElement("line", {
      pathLength: 100,
      x1: points[source][0].toFixed(1),
      y1: points[source][1].toFixed(1),
      x2: points[target][0].toFixed(1),
      y2: points[target][1].toFixed(1),
    });
    packets.push([packet, timing]);
    if (!targets.has(target)) targets.set(target, []);
    targets.get(target)?.push(timing);
  }
  points.forEach(([x, y], index) => {
    const position = { cx: x.toFixed(1), cy: y.toFixed(1) };
    if (index < hubs.length) {
      nodes.push(svgElement("circle", { ...position, r: 22, class: "halo" }));
      nodes.push(svgElement("circle", { ...position, r: 7, class: "hub" }));
    }
    const timings = targets.get(index);
    if (timings) {
      for (const timing of timings)
        peers.push([
          svgElement("circle", { ...position, r: 5, class: "ping" }),
          timing,
        ]);
      peers.push([
        svgElement("circle", { ...position, r: 4.5, class: "peer" }),
        timings[0],
      ]);
    } else if (index >= hubs.length)
      nodes.push(
        svgElement("circle", {
          ...position,
          r: (2 + weight[index] * 2.5).toFixed(1),
          class: weight[index] > 0.55 ? "busy" : "",
        }),
      );
  });
  for (const [element, timing] of [...packets, ...peers])
    for (const [property, value] of Object.entries(timing))
      element.style.setProperty(property, value);
  $(".net-pools", base).replaceChildren(...pools);
  $("#login-dot-mask", base).replaceChildren(...fields);
  $(".net-links", base).replaceChildren(...links);
  $(".net-nodes", base).replaceChildren(...nodes);
  $(".net-flows", motion).replaceChildren(
    ...packets.map(([element]) => element),
  );
  $(".net-peers", motion).replaceChildren(
    ...peers.map(([element]) => element),
  );
}

let meshTimer = 0;
addEventListener("resize", () => {
  clearTimeout(meshTimer);
  meshTimer = setTimeout(() => {
    if (!$("#login").hidden) drawMesh();
  }, 250);
});

// The sign-in backdrop leans slightly toward the pointer. Moving the layers
// directly keeps style recalculation off the rest of the page.
const reducedMotion = matchMedia("(prefers-reduced-motion: reduce)");
let parallaxFrame = 0;
$("#login").addEventListener("pointermove", (event) => {
  if (reducedMotion.matches || event.pointerType !== "mouse" || parallaxFrame)
    return;
  parallaxFrame = requestAnimationFrame(() => {
    parallaxFrame = 0;
    const x = ((event.clientX / innerWidth) * 2 - 1) * -12,
      y = ((event.clientY / innerHeight) * 2 - 1) * -12;
    /** @type {NodeListOf<SVGElement>} */ (document.querySelectorAll(".login-backdrop")).forEach((layer) => {
      layer.style.transform = `translate3d(${x.toFixed(1)}px, ${y.toFixed(1)}px, 0)`;
    });
  });
});

/** @param {unknown} value @returns {string} */
function bytes(value) {
  if (value == null || !Number.isFinite(Number(value))) return "Unknown";
  let n = Number(value),
    i = 0;
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  while (n >= 1024 && i < 4) {
    n /= 1024;
    i++;
  }
  return (i ? n.toFixed(n < 10 ? 1 : 0) : n) + " " + units[i];
}
/** @param {number | null | undefined} used @param {number | null | undefined} total @returns {number | null} */
function percent(used, total) {
  return used != null && total != null && total > 0 ? Math.min(100, Math.max(0, (used / total) * 100)) : null;
}
/** @param {string | null | undefined} value @returns {string} */
function age(value) {
  if (!value || String(value).startsWith("0001")) return "Unknown";
  const seconds = Math.max(0, (Date.now() - new Date(value).getTime()) / 1000);
  if (!Number.isFinite(seconds)) return "Unknown";
  if (seconds < 60) return Math.floor(seconds) + "s";
  if (seconds < 3600) return Math.floor(seconds / 60) + "m";
  if (seconds < 86400)
    return (
      Math.floor(seconds / 3600) +
      "h " +
      Math.floor((seconds % 3600) / 60) +
      "m"
    );
  return Math.floor(seconds / 86400) + "d";
}
/** @param {string | null | undefined} value @returns {string} */
function date(value) {
  return !value || String(value).startsWith("0001")
    ? "Unknown"
    : new Date(value).toLocaleString();
}
/** @param {string | undefined} os @returns {string} */
function osName(os) {
  return (
    (/** @type {Record<string, string>} */ ({ darwin: "macOS", windows: "Windows", linux: "Linux" }))[os || ""] ||
    os ||
    "Unknown OS"
  );
}
/** @param {Machine} node @returns {string} */
function machineState(node) {
  if (node.contexts && node.contexts.length > 1) node = hostStatusContext(node);
  if (node.controlPending) return node.disabled ? "Disabling" : "Enabling";
  if (node.disabled) return "Disabled";
  if (node.status === "update") return "Updating";
  if (!node.online) return "Offline";
  if (node.status !== "summary" && node.status !== "ready") return "Online";
  if (node.leased) return "Reserved";
  return node.activeCount > 0 ? "Busy" : "Idle";
}
/** @param {Machine} node @returns {string} */
function statusDetail(node) {
  if (node.contexts && node.contexts.length > 1) node = hostStatusContext(node);
  if (node.status === "update")
    return (/** @type {Record<string, string>} */ ({ "waiting-idle": "Waiting for idle", "waiting-gateway": "Waiting for gateway", "downloading": "Downloading", "staged": "Staged", "reserved": "Restart reserved", "restarting": "Restarting" }))[node.updateState || ""] || "";
  if (!node.online)
    return age(node.lastSeen) === "Unknown"
      ? ""
      : "Seen " + age(node.lastSeen).split(" ")[0] + " ago";
  if (!["summary", "ready"].includes(node.status)) return "No health report";
  if (node.activeCount > 0) return workSummary(node);
  if (node.leased && known(node.leaseExpires))
    return "Until " + clock(node.leaseExpires);
  return known(node.lastActive)
    ? "Active " + age(node.lastActive).split(" ")[0] + " ago"
    : "";
}
// Host status includes connected contexts and gateway-confirmed restart grace,
// not just the context selected for metrics and actions. Prefer active work
// over idle reports and newer observations when statuses match.
/** @param {Machine} node @returns {Machine} */
function hostStatusContext(node) {
  const contexts = node.contexts || [node];
  const active = contexts.filter((context) => context.online || machineState(context) === "Updating"),
    candidates = active.length ? active : contexts,
    priority = ["Updating", "Busy", "Reserved", "Enabling", "Online", "Idle", "Disabling", "Disabled", "Offline"];
  return [...candidates].sort((a, b) =>
    priority.indexOf(machineState(a)) - priority.indexOf(machineState(b)) ||
    (Date.parse(b.lastSeen) || 0) - (Date.parse(a.lastSeen) || 0),
  )[0];
}
/** @param {string | null | undefined} value @returns {value is string} */
function known(value) {
  return !!value && !String(value).startsWith("0001");
}
// Older nodes report only a total, so fall back to it without a breakdown.
/** @param {Machine} node @returns {string} */
function workSummary(node) {
  const w = node.work;
  if (!w) return node.activeCount + " active";
  const parts = [
    w.tasks && plural(w.tasks, "task"),
    w.exec && plural(w.exec, "exec session"),
    w.transfers && plural(w.transfers, "transfer"),
    w.other && plural(w.other, "operation"),
  ].filter(Boolean);
  return parts.join(" · ") || (tunnelCount(node) ? "Tunnels only" : "");
}
/** @param {Machine} node @returns {number} */
function tunnelCount(node) {
  return (node.tunnels?.forward || 0) + (node.tunnels?.reverse || 0);
}
/** @param {Machine} node @returns {number | null} */
function peerCount(node) {
  const r = node.connectionRoutes;
  return r ? r.direct + r.turn + r.relay + r.unknown : null;
}
/** @param {Machine} node @returns {string} */
function routeText(node) {
  const r = node.connectionRoutes;
  return r
    ? [
        r.direct && r.direct + " direct",
        r.turn && r.turn + " TURN",
        r.relay && r.relay + " relay",
        r.unknown && r.unknown + " unknown",
      ]
        .filter(Boolean)
        .join(" · ")
    : "";
}
/** @param {Machine} node @returns {string} */
function tunnelText(node) {
  const t = node.tunnels;
  if (!t || !tunnelCount(node)) return "";
  return (
    [t.forward && t.forward + " forward", t.reverse && t.reverse + " reverse"]
      .filter(Boolean)
      .join(" · ") + (tunnelCount(node) === 1 ? " tunnel" : " tunnels")
  );
}
// Three fixed counters keep every row's shape stable as connections come and
// go: direct peers, relayed peers (TURN or gateway relay), and tunnels.
/** @param {Machine} node @returns {string} */
function connectionCell(node) {
  const r = node.online ? node.connectionRoutes : null,
    t = node.online ? node.tunnels : null;
  /** @param {string} iconName @param {string} label @param {number | null} value @param {string} title @returns {string} */
  const counter = (iconName, label, value, title) =>
    `<span class="counter${value ? "" : " zero"}" title="${esc(title)}">${icon(iconName)}<span class="sr-only">${label} </span>${value ?? "–"}</span>`;
  return `<div class="counters">${counter("direct", "Direct peers", r ? r.direct + r.unknown : null, r ? `Direct peers: ${r.direct}` + (r.unknown ? `, unknown route: ${r.unknown}` : "") : "Direct peers: not reported")}${counter("relay", "Relayed peers", r ? r.turn + r.relay : null, r ? `Relayed peers: ${r.turn} TURN, ${r.relay} gateway relay` : "Relayed peers: not reported")}${counter("tunnel", "Tunnels", t ? t.forward + t.reverse : null, t ? `Tunnels: ${t.forward} forward, ${t.reverse} reverse` : "Tunnels: not reported")}</div>`;
}
/** @param {Machine} node @returns {string} */
function platformText(node) {
  const sys = node.system || /** @type {Partial<SystemInfo>} */ ({}),
    version = sys.platformVersion || "",
    distro = sys.platform && sys.platform !== node.os ? sys.platform : "";
  return [
    distro ? osName(node.os) : (osName(node.os) + " " + version).trim(),
    distro && (distro + " " + version).trim(),
    sys.arch || node.software?.arch,
  ]
    .filter(Boolean)
    .join(" · ");
}
/** @param {Partial<SystemInfo> | null | undefined} sys @returns {DiskUsage | undefined} */
function diskUsage(sys) {
  return sys?.disks?.find((disk) => disk.totalBytes > 0);
}
// Older nodes do not report their execution context, so they get no tag.
/** @type {Record<string, [string, string]>} */
const runAsLabels = {
  system: ["System", "System service, starts at boot"],
  user: ["User", "User session, runs while signed in"],
};
/** @param {Machine} node @returns {string} */
function runAsTag(node) {
  const label = runAsLabels[node.runAs || ""];
  return label
    ? `<span class="context-tag" title="${label[1]}">${label[0]}</span>`
    : "";
}
/** @param {number} count @param {string} noun @returns {string} */
function plural(count, noun) {
  return count + " " + noun + (count === 1 ? "" : "s");
}
/** @param {string} label @returns {string} */
function badge(label) {
  const kind = [
    "Idle",
    "Online",
    "Redeemed",
    "Active",
    "Enabled",
    "Complete",
  ].includes(label)
    ? "good"
    : [
          "Busy",
          "Reserved",
          "Updating",
          "Pending",
          "Disabling",
          "Enabling",
        ].includes(label)
      ? "warn"
      : ["Offline", "Revoked", "Disabled", "Failed"].includes(label)
        ? "bad"
        : "";
  return `<span class="badge ${kind}"><span class="status-dot"></span>${esc(label)}</span>`;
}
/** @param {number | null | undefined} value @param {string} label @returns {string} */
function meter(value, label) {
  if (value == null || !Number.isFinite(value))
    return '<span class="unknown">—<span class="sr-only">Unknown</span></span>';
  return `<div class="resource"><meter min="0" max="100" low="85" high="95" optimum="0" value="${Math.min(100, Math.max(0, value))}" aria-label="${esc(label)}"></meter><span>${Math.round(value)}%</span></div>`;
}
/** @param {string} title @param {string} description @param {string} [action] @returns {string} */
function empty(title, description, action = "") {
  return `<div class="empty"><strong>${esc(title)}</strong><p>${esc(description)}</p>${action}</div>`;
}
/** @param {string} title @param {string} body @param {string} [side] @returns {string} */
function panel(title, body, side = "") {
  return `<section class="panel"><div class="panel-heading"><h2>${title}</h2>${side}</div>${body}</section>`;
}
/** @param {string[]} headers @param {string[]} rows @returns {string} */
function table(headers, rows) {
  return `<div class="table-wrap" role="region" aria-label="Records" tabindex="0"><table><thead><tr>${headers
    .map((h) => `<th scope="col">${esc(h)}</th>`)
    .join("")}</tr></thead><tbody>${rows.join("")}</tbody></table></div>`;
}
/** @type {[string, string, string?][]} */
const machineColumns = [
  ["Machine", "name"],
  ["Status", "status"],
  ["Connections", "peers"],
  ["CPU", "cpu", "cpu"],
  ["GPU/NPU", "gpu", "cpu"],
  ["Memory", "memory", "memory"],
  ["Disk", "disk", "disk"],
];
/** @param {string[]} rows @param {boolean} showGPUs @returns {string} */
function machineTable(rows, showGPUs) {
  const head = machineColumns
    .filter(([, key]) => key !== "gpu" || showGPUs)
    .map(([label, key, resourceIcon]) => {
      const selected = state.sort === key;
      return `<th scope="col" aria-sort="${selected ? (state.descending ? "descending" : "ascending") : "none"}"><button class="sort-button${selected ? " sorted" : ""}" data-action="sort" data-sort="${key}">${resourceIcon ? icon(resourceIcon) : ""}${label}<span aria-hidden="true">${selected ? (state.descending ? "↓" : "↑") : "↕"}</span></button></th>`;
    })
    .join("");
  return `<div class="table-wrap machine-table" role="region" aria-label="Machines" tabindex="0"><table><thead><tr>${head}</tr></thead><tbody>${rows.join("")}</tbody></table></div>`;
}

// An offline User selection can still show its online System companion's
// telemetry. Keep the selected identity for details, actions, and routing.
/** @param {Machine} node @returns {Machine} */
function machineMetricsContext(node) {
  if (!node.online && node.hybridRole === "user")
    return node.contexts?.find((context) => context.hybridRole === "system" && context.online) || node;
  return node;
}
/** @param {GPUInfo[] | undefined} gpus @returns {GPUInfo[]} */
function visibleGPUs(gpus) {
  return (gpus || []).filter((gpu) => ["discrete", "npu"].includes(gpu.kind || ""));
}
/** @param {GPUInfo[] | undefined} gpus @returns {number | null} */
function gpuMemoryUsage(gpus) {
  const values = visibleGPUs(gpus).map((gpu) => gpu.memoryUsedBytes == null ? null : percent(gpu.memoryUsedBytes, gpu.memoryTotalBytes)).filter(/** @returns {value is number} */ (value) => value != null && Number.isFinite(value));
  return values.length ? Math.max(...values) : null;
}
/** @param {GPUInfo[] | undefined} gpus @param {boolean} online @param {boolean} [metadata] @returns {string} */
function gpuResources(gpus, online, metadata = false) {
  gpus = visibleGPUs(gpus);
  if (!gpus.length) return meter(null, "GPU/NPU memory usage");
  return gpus.map((gpu) => {
    const used = online ? gpu.memoryUsedBytes : null,
      kind = gpu.kind === "npu" ? "NPU" : "GPU",
      memory = used == null ? null : percent(used, gpu.memoryTotalBytes),
      memoryText = gpu.memoryTotalBytes ? used == null ? bytes(gpu.memoryTotalBytes) + " total" : bytes(used) + " of " + bytes(gpu.memoryTotalBytes) : used == null ? "" : bytes(used) + " used";
    const reading = memory == null ? `<span class="muted">${kind} detected</span>` : meter(memory, kind + " memory usage for " + gpu.name);
    const resource = `<div class="gpu-usage" title="${esc(gpu.name)}">${reading}<small>${memoryText || "Memory unavailable"}</small>${online && gpu.usagePercent != null ? `<small class="gpu-compute">Compute ${Math.round(gpu.usagePercent)}%</small>` : ""}</div>`;
    return metadata ? `<dt>${kind} memory usage</dt><dd>${resource}</dd>` : resource;
  }).join("");
}
/** @template {Machine} T @param {T[]} nodes @returns {T[]} */
function sortedMachines(nodes) {
  /** @param {Machine} n @returns {string | number | null | undefined} */
  const value = (n) => {
    const metrics = machineMetricsContext(n);
    switch (state.sort) {
      case "status":
        return machineState(n);
      case "cpu":
        return metrics.online ? metrics.system?.cpuUsagePercent : null;
      case "gpu":
        return metrics.online ? gpuMemoryUsage(metrics.system?.gpus) : null;
      case "memory":
        return metrics.online
          ? percent(metrics.system?.memoryUsedBytes, metrics.system?.memoryTotalBytes)
          : null;
      case "peers":
        return metrics.online ? peerCount(metrics) : null;
      case "disk": {
        const disk = metrics.online ? diskUsage(metrics.system) : null;
        return percent(disk?.usedBytes, disk?.totalBytes);
      }
      default:
        return n.name;
    }
  };
  return [...nodes].sort((a, b) => {
    const av = value(a),
      bv = value(b);
    if (av == null && bv != null) return 1;
    if (bv == null && av != null) return -1;
    const order =
      av == null
        ? 0
        : typeof av === "string"
          ? av.localeCompare(String(bv), undefined, { sensitivity: "base" })
          : av - Number(bv);
    return (state.descending ? -order : order) || a.name.localeCompare(b.name);
  });
}

/** @param {Machine[]} nodes @param {boolean} showGPUs @returns {string[]} */
function machineRows(nodes, showGPUs) {
  return nodes.map((n) => {
    const metrics = machineMetricsContext(n),
      sys = (metrics.online ? metrics.system : null) || /** @type {Partial<SystemInfo>} */ ({}),
      memory = percent(sys?.memoryUsedBytes, sys?.memoryTotalBytes),
      disk = diskUsage(sys) || {usedBytes: 0, totalBytes: 0},
      diskPercent = percent(disk?.usedBytes, disk?.totalBytes),
      gpuColumn = showGPUs ? `<td class="resource-column gpu-column" data-label="GPU/NPU">${gpuResources(metrics.system?.gpus, metrics.online)}</td>` : "",
      detail = statusDetail(n);
    const platformIcon =
      (/** @type {Record<string, string>} */ ({ darwin: "laptop", windows: "windows", linux: "gateway" }))[n.os] ||
      "machine";
    return `<tr data-id="${esc(n.id)}"><td class="name-column"><div class="machine-cell"><span class="os-icon">${icon(platformIcon)}</span><div class="machine-text"><div class="machine-title"><button class="machine-name" data-action="details" data-id="${esc(n.id)}" title="${esc(n.targetName || n.name)}">${esc(n.name)}</button>${contextChips(n)}</div><small>${esc(osName(n.os))} · ${esc(n.system?.arch || n.software?.arch || "Unknown")}<span class="machine-version"> · ${esc(n.software?.version || "Unknown version")}</span></small></div><button class="icon-button machine-menu-button" data-action="machine-menu" data-id="${esc(n.id)}" aria-label="Actions for ${esc(n.name)}" aria-haspopup="menu" aria-controls="machine-menu" aria-expanded="false">${icon("more")}</button></div></td><td class="status-column">${badge(machineState(n))}${detail ? `<small>${esc(detail)}</small>` : ""}</td><td class="resource-column connection-column" data-label="Connections">${connectionCell(metrics)}</td><td class="resource-column cpu-column" data-label="CPU">${meter(sys?.cpuUsagePercent, "CPU utilization")}${metrics.system?.logicalCPUs ? `<small>${plural(metrics.system.logicalCPUs, "core")}</small>` : ""}</td>${gpuColumn}<td class="resource-column memory-column" data-label="Memory">${meter(memory, "Memory utilization")}${memory == null ? "" : `<small>${bytes(sys.memoryUsedBytes)} of ${bytes(sys.memoryTotalBytes)}</small>`}</td><td class="resource-column disk-column" data-label="Disk">${meter(diskPercent, "Disk utilization")}${diskPercent == null ? "" : `<small>${bytes(disk.usedBytes)} of ${bytes(disk.totalBytes)}</small>`}</td></tr>`;
  });
}
/** @param {Machine[]} nodes @returns {MachineGroup[]} */
function groupedMachines(nodes) {
  /** @type {Map<string, Machine[]>} */
  const groups = new Map();
  for (const node of nodes) {
    const paired = node.hybridId && ["user", "system"].includes(node.hybridRole || ""),
      key = paired ? "hybrid:" + node.hybridId : "node:" + node.id;
    if (!groups.has(key)) groups.set(key, []);
    groups.get(key)?.push(node);
  }
  return [...groups.values()].map((contexts) => {
    const user = contexts.find((n) => n.hybridRole === "user"),
      selected = contexts.find((n) => n.id === state.contexts.get(n.hybridId || "")) || user || contexts[0];
    return { ...selected, targetName: selected.name, name: user?.name || contexts[0].name, contexts };
  });
}
/** @param {Machine} node @returns {string} */
function contextChips(node) {
  if (!node.hybridId) return runAsTag(node);
  return [...(node.contexts || [node])]
    .sort((a, b) => (a.hybridRole === "user" ? -1 : 1) - (b.hybridRole === "user" ? -1 : 1))
    .map((context) => `<button class="context-tag context-choice${context.online ? "" : " offline"}" data-action="context" data-id="${esc(context.id)}" aria-pressed="${node.id === context.id}" aria-description="${context.online ? "Online" : "Offline"}" title="${esc(context.name)} · ${context.online ? "Online" : "Offline"}${context.hybridRole === "system" ? " · For operations requiring elevation" : " · Default execution context"}"><span>${context.hybridRole === "user" ? "User" : "System"}</span></button>`)
    .join("");
}
/** @param {Machine} n @returns {boolean} */
function machineMatches(n) {
  const text = [
    n.name,
    n.os,
    osName(n.os),
    ...(n.contexts || []).map((context) => context.name),
    ...Object.entries(n.labels || {}).flat(),
  ]
    .join(" ")
    .toLowerCase();
  return (
    text.includes(state.search.trim().toLowerCase()) && inState(n, state.filter)
  );
}
// Filters roll transitional states into the core ones: a reservation is busy,
// updating machines are online, and pending policy follows the disabled flag.
const stateFilters = [
  ["all", "All"],
  ["online", "Online"],
  ["busy", "Busy"],
  ["idle", "Idle"],
  ["offline", "Offline"],
  ["disabled", "Disabled"],
];
/** @param {Machine} n @param {string} filter @returns {boolean} */
function inState(n, filter) {
  if (n.contexts && n.contexts.length > 1)
    return filter === "offline"
      ? n.contexts.every((context) => inState(context, filter))
      : n.contexts.some((context) => inState(context, filter));
  switch (filter) {
    case "online":
      return n.online;
    case "offline":
      return !n.online;
    case "busy":
      return ["Busy", "Reserved"].includes(machineState(n));
    case "idle":
      return machineState(n) === "Idle";
    case "disabled":
      return !!n.disabled;
    default:
      return true;
  }
}
/** @param {Machine[]} nodes @returns {string} */
function stateFilter(nodes) {
  return `<div class="state-filter" role="group" aria-label="Filter machines by state">${stateFilters
    .map(([key, label]) => {
      const count = nodes.filter((n) => inState(n, key)).length;
      return `<button data-action="filter-state" data-filter="${key}" aria-pressed="${state.filter === key}"${count || key === "all" ? "" : ' class="empty-state"'}>${label}<span>${count}</span></button>`;
    })
    .join("")}</div>`;
}
/** @param {boolean} overview @returns {string} */
function machinePanel(overview) {
  const all = groupedMachines(state.snapshot?.nodes || []);
  const nodes = overview
    ? all.filter((node) => node.contexts.some((context) => context.online && !context.disabled))
    : all.filter(machineMatches);
  const sorted = sortedMachines(nodes);
  const rows = overview ? sorted.slice(0, 8) : sorted;
  const showGPUs = all.some((node) => (node.contexts || [node]).some((context) => visibleGPUs(context.system?.gpus).length));
  const body = rows.length
    ? machineTable(machineRows(rows, showGPUs), showGPUs)
    : empty(
        all.length ? overview ? "No online enabled machines" : "No matching machines" : "No machines registered",
        all.length
          ? overview ? "Offline and disabled machines are available under All machines." : "Try another search or state filter."
          : "Add a machine to see its availability and resources.",
        all.length
          ? overview ? '<a class="secondary" href="#machines" data-action="filter-machines" data-filter="all">All machines</a>' : '<button class="secondary" data-action="clear-filters">Clear filters</button>'
          : `<button class="primary" data-action="add">${icon("plus")}Add machine</button>`,
      );
  if (overview)
    return panel(
      "Machines",
      body,
      all.length
        ? `<a href="#machines" data-action="filter-machines" data-filter="all">${all.length > rows.length ? "View all " + all.length : "All machines"}${icon("arrow")}</a>`
        : "",
    );
  const count =
    nodes.length === all.length
      ? plural(all.length, "machine")
      : `${nodes.length} of ${all.length}`;
  return `<section class="panel"><div class="table-tools"><label class="search">${icon("search")}<input id="machine-search" type="search" aria-label="Search machines" placeholder="Search names, platforms, or labels" value="${esc(state.search)}"></label>${stateFilter(all)}<span class="result-count">${count}</span></div>${body}</section>`;
}
/** @returns {string} */
function gatewayPanel() {
  const g = state.snapshot?.gateway || /** @type {Partial<NonNullable<FleetSnapshot['gateway']>>} */ ({}),
    sys = g?.system || /** @type {Partial<SystemInfo>} */ ({}),
    disk = sys.disks?.[0],
    sampled = age(sys.sampledAt),
    uptime = age(g?.startedAt);
  const sampleStale =
    sampled !== "Unknown" && Date.now() - Date.parse(sys.sampledAt || "") > 60000;
  const facts = [
    g?.software?.version && "Version " + esc(g.software.version),
    uptime !== "Unknown" && "Up " + esc(uptime),
    sampleStale && `<span class="warn-text">Sampled ${esc(sampled)} ago</span>`,
  ].filter(Boolean);
  /** @param {string} name @param {string} resourceIcon @param {number | null | undefined} value @param {string} text @returns {string} */
  const metric = (name, resourceIcon, value, text) =>
    `<div class="gateway-metric"><div class="metric-heading"><span class="resource-label">${icon(resourceIcon)}${esc(name)}</span><strong>${esc(text)}</strong></div>${value == null ? '<div class="metric-empty" aria-hidden="true"></div>' : meter(value, name)}</div>`;
  return `<section class="gateway-card" aria-label="Gateway health"><div class="gateway-identity"><span class="gateway-icon">${icon("gateway")}</span><div><span class="gateway-label">Gateway</span><strong>${esc(g.url || location.host)}</strong>${facts.length ? `<small>${facts.join(" · ")}</small>` : ""}</div></div><div class="gateway-metrics" title="${sampled === "Unknown" ? "Sample time unknown" : "Sampled " + esc(sampled) + " ago"}">${metric("CPU", "cpu", sys.cpuUsagePercent, sys.cpuUsagePercent == null ? "Unknown" : Math.round(sys.cpuUsagePercent) + "%")}${metric("Memory", "memory", percent(sys.memoryUsedBytes, sys.memoryTotalBytes), sys.memoryTotalBytes ? bytes(sys.memoryUsedBytes) + " of " + bytes(sys.memoryTotalBytes) : "Unknown")}${metric("State disk", "disk", percent(disk?.usedBytes, disk?.totalBytes), disk?.totalBytes ? bytes(disk.usedBytes) + " of " + bytes(disk.totalBytes) : "Unknown")}</div></section>`;
}
/** @returns {string} */
function renderOverview() {
  const raw = state.snapshot?.nodes || [],
    nodes = groupedMachines(raw),
    online = nodes.filter((n) => inState(n, "online")).length,
    offline = nodes.length - online,
    reported = raw.filter(
      (n) => n.online && ["summary", "ready"].includes(n.status),
    );
  const work = reported.reduce((sum, n) => sum + n.activeCount, 0);
  /** @param {string} label @param {number} value @param {string} filter @param {string} tone @returns {string} */
  const stat = (label, value, filter, tone) =>
    `<a class="stat ${tone}" href="#machines" data-action="filter-machines" data-filter="${filter}"><span class="stat-label">${tone ? '<span class="status-dot"></span>' : ""}${label}</span><span class="stat-value">${value}</span></a>`;
  return `${gatewayPanel()}<div class="fleet-summary">${stat("Online", online, "online", online ? "good" : "")}${stat("Offline", offline, "offline", offline ? "bad" : "")}${stat("Active work", work, "busy", work ? "warn" : "")}</div>${machinePanel(true)}`;
}
// Everything but pending invitations: the gateway deletes revoked and expired
// ones and hides redeemed ones, which keep their machine's credential.
/** @param {Invitation[]} items @returns {Invitation[]} */
function clearable(items) {
  return items.filter(
    (i) =>
      i.revoked || i.redeemedId || Date.now() > new Date(i.expiresAt).getTime(),
  );
}
// A redeemed invitation is managed through its machine, not revoked here.
/** @param {Invitation} i @param {string} status @returns {string} */
function inviteAction(i, status) {
  if (status === "Redeemed")
    return state.snapshot?.nodes.some((n) => n.id === i.redeemedId)
      ? `<button class="secondary compact" data-action="details" data-id="${esc(i.redeemedId)}">Open machine</button>`
      : "";
  return status === "Pending"
    ? `<button class="secondary compact" data-action="revoke-invite" data-id="${esc(i.id)}">Revoke</button>`
    : "";
}
/** @param {Invitation[]} items @returns {string} */
function renderInvitations(items) {
  /** @type {Record<string, string>} */
  const startup = { user: "User", system: "System", hybrid: "Hybrid", "": "" };
  const rows = items.map((item) => {
    const i = {...item, serviceMode: item.serviceMode || ""};
    const status = i.revoked
      ? "Revoked"
      : i.redeemedId
        ? "Redeemed"
        : Date.now() > new Date(i.expiresAt).getTime()
          ? "Expired"
          : "Pending";
    return `<tr><td><strong>${esc(i.name || "Target hostname")}</strong>${i.autoName ? '<small class="muted"> · Automatic name</small>' : ""}</td><td>${esc(osName(i.asset?.os))}</td><td>${badge(status)}</td><td>${esc(startup[i.serviceMode] || i.serviceMode || "Platform default")}</td><td class="muted">${esc(date(i.expiresAt))}</td><td class="action-column">${inviteAction(i, status)}</td></tr>`;
  });
  return panel(
    "Installation invitations",
    rows.length
      ? table(["Machine", "Platform", "Status", "Startup", "Expires", ""], rows)
      : empty(
          "No invitations yet",
          "Create a one-time command to install a worker.",
          `<button class="primary" data-action="add">${icon("plus")}Add machine</button>`,
        ),
    clearable(items).length
      ? `<button class="secondary compact" data-action="clear-invites">Clear history</button>`
      : "",
  );
}
/** @param {FleetKey[]} items @returns {string} */
function renderKeys(items) {
  const keys = items.filter((k) => k.role === "common");
  return (
    panel(
      "Fleet keys",
      keys.length
        ? table(
            ["Name", "Status", "Created", "Key ID", ""],
            keys.map(
              (k) =>
                `<tr><td><strong>${esc(k.name)}</strong></td><td>${badge(k.revoked ? "Revoked" : "Active")}</td><td class="muted">${esc(date(k.createdAt))}</td><td class="mono muted">${esc(k.id)}</td><td class="action-column"><div class="row-actions"><button class="secondary compact" data-action="rename-key" data-id="${esc(k.id)}" data-name="${esc(k.name)}">Rename</button>${k.revoked ? "" : `<button class="secondary compact" data-action="revoke-key" data-id="${esc(k.id)}">Revoke</button>`}</div></td></tr>`,
            ),
          )
        : empty(
            "No common keys",
            "Installed machines normally use invitation-bound keys.",
          ),
      `<button class="secondary compact" data-action="create-key">${icon("plus")}Create key</button>`,
    ) +
    '<p class="notice">Common keys allow enrollment and discovery, not independent task execution. Account keys are not listed here. Keep keys out of worker-accessible files.</p>'
  );
}
/** @param {FleetUser[]} items @returns {string} */
function renderUsers(items) {
  return panel(
    "Private fleet accounts",
    table(
      ["Account", "Status", "Created", ""],
      items.map(
        (u) =>
          `<tr><td><strong>${esc(u.name)}</strong></td><td>${badge(u.disabled ? "Disabled" : "Enabled")}</td><td class="muted">${esc(date(u.createdAt))}</td><td class="action-column">${!u.disabled && u.id !== "legacy" ? `<div class="row-actions"><button class="secondary compact" data-action="reset-password" data-name="${esc(u.name)}">Set browser password</button><button class="secondary compact" data-action="disable-user" data-id="${esc(u.id)}" data-name="${esc(u.name)}">Disable</button></div>` : u.id === "legacy" ? '<span class="muted">Operator fleet</span>' : ""}</td></tr>`,
      ),
    ),
    `<button class="secondary compact" data-action="create-user">${icon("plus")}Create account</button>`,
  );
}
/** @param {UpdateResult} result @returns {string} */
function renderUpdates(result) {
  const d = result.deployment;
  const body = d
    ? `<dl class="metadata padded"><dt>Selected version</dt><dd>${esc(d.manifest?.version)}</dd><dt>Phase</dt><dd>${badge(d.phase === "complete" ? "Complete" : d.phase === "failed" ? "Failed" : d.phase)}</dd><dt>Gateway</dt><dd>${esc(result.gatewayUpdate?.state || d.gatewayState || "Staging")}${result.gatewayUpdate?.relayCarriers ? `, ${esc(result.gatewayUpdate.relayCarriers)} relay carrier(s) pending` : ""}</dd><dt>Source</dt><dd>${esc(d.source)}</dd><dt>Updated</dt><dd>${esc(date(d.updatedAt))}</dd><dt>Deployment ID</dt><dd class="mono">${esc(d.id)}</dd></dl>${d.error ? `<p class="form-error padded">${esc(d.error)}</p>` : ""}`
    : empty(
        "No selected deployment",
        "Publish a bundle through the CLI or check for a stable release.",
      );
  const deferred = new Set(d?.deferred || []);
  const acknowledgments = { ...result.nodes };
  for (const id of deferred) acknowledgments[id] ||= {};
  const rows = Object.entries(acknowledgments).map(([id, s]) => {
    const name = state.snapshot?.nodes.find((n) => n.id === id)?.name;
    const progress = result.nodeProgress?.[id] || s.state;
    return `<tr><td>${name ? `<strong>${esc(name)}</strong><small class="mono muted id-line">${esc(id.slice(0, 16))}</small>` : `<span class="mono">${esc(id)}</span>`}</td><td>${esc(s.software?.version)}</td><td>${deferred.has(id) ? "Deferred, awaiting confirmation" : esc(progress || "Awaiting status")}</td><td>${deferred.has(id) || progress === "offline-pending" ? "Unknown" : s.paused ? "Paused" : "Open"}</td><td${s.error ? "" : ' class="muted"'}>${esc(s.error || "None")}</td></tr>`;
  });
  return (
    panel(
      "Managed software",
      body,
      '<button class="secondary compact" data-action="check-release">Check for release</button>',
    ) +
    `<p class="notice">Idle nodes update independently, up to two contexts at a time. Busy nodes remain pending. The gateway separately waits for relay traffic to drain. ${deferred.size ? `${deferred.size} offline participant(s) deferred; they will update when they reconnect. ` : ""}Publish development bundles with <code>control update push DIR</code>. Browser access does not bypass binary verification or maintenance admission.</p>` +
    (rows.length
      ? panel(
          "Node acknowledgments",
          table(["Machine", "Version", "State", "Admission", "Error"], rows),
        )
      : "")
  );
}
/** @type {Record<string, [string, string]>} */
const auditActions = {
  "session.signin": ["Signed in", "good"],
  "session.signin_failed": ["Sign-in failed", "bad"],
  "session.signout": ["Signed out", ""],
  "key.create": ["Created key", "good"],
  "key.rename": ["Renamed key", ""],
  "key.revoke": ["Revoked key", "bad"],
  "invitation.create": ["Created invitation", "good"],
  "invitation.revoke": ["Revoked invitation", "bad"],
  "invitation.clear": ["Cleared invitation history", ""],
  "audit.clear": ["Cleared activity log", "warn"],
  "machine.enroll": ["Enrolled machine", "good"],
  "machine.rename": ["Renamed machine", ""],
  "machine.enable": ["Enabled machine", "good"],
  "machine.disable": ["Disabled machine", "warn"],
  "machine.unregister": ["Unregistered machine", "bad"],
  "machine.wake": ["Sent wake packet", ""],
  "machine.startup": ["Requested startup context", ""],
  "machine.secrets.set": ["Set worker secret", ""],
  "machine.secrets.delete": ["Deleted worker secret", ""],
  "account.create": ["Created account", "good"],
  "account.disable": ["Disabled account", "bad"],
  "account.password": ["Set browser password", ""],
  "update.publish": ["Published update", "warn"],
  "update.check": ["Checked for release", ""],
};
/** @param {number} [before] @returns {`audit?${string}`} */
function auditPath(before = 0) {
  const query = new URLSearchParams({ limit: "100" });
  if (state.auditCategory) query.set("category", state.auditCategory);
  if (before) query.set("before", String(before));
  return `audit?${query}`;
}
// Polling adds newer events above the pages already loaded.
/** @param {AuditPage} data @returns {AuditPage} */
function mergeAudit(data) {
  const current = state.auxiliary?.page === "activity" ? state.auxiliary.data : null;
  if (!current?.events?.length) return data;
  const newest = current.events[0].id;
  return {
    events: [...data.events.filter((e) => e.id > newest), ...current.events],
    next: current.next,
  };
}
/** @param {string} value @returns {string} */
function stamp(value) {
  return new Date(value).toLocaleString([], {
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}
/** @returns {string} */
function renderActivity() {
  const operator = state.session?.role === "superuser",
    data = state.auxiliary?.page === "activity" ? state.auxiliary.data : null,
    categories = [
      ["", "All activity"],
      ["session", "Sign-ins"],
      ["key", "Keys"],
      ["invitation", "Invitations"],
      ["machine", "Machines"],
      ["account", "Accounts"],
      ...(operator ? [["update", "Updates"]] : []),
    ];
  const rows = (data?.events || []).map((e) => {
    const [label, tone] = auditActions[e.action] || [e.action, ""];
    return `<tr><td class="muted" title="${esc(date(e.at))}">${esc(stamp(e.at))}</td><td><span class="badge ${tone}"><span class="status-dot"></span>${esc(label)}</span>${e.detail ? `<small class="muted cell-detail">${esc(e.detail)}</small>` : ""}</td><td${e.targetId ? ` title="${esc(e.targetId)}"` : ""}>${e.target ? `<strong>${esc(e.target)}</strong>` : '<span class="muted">—</span>'}</td><td><span>${esc(e.actor || "Unknown")}</span><small class="muted cell-detail">${esc(e.via)}${e.keyId && e.keyId !== "superuser" ? " · " + esc(e.keyId.slice(0, 12)) : ""}</small></td><td class="mono muted">${esc(e.sourceIp || "—")}</td>${operator ? `<td class="muted">${esc(e.userId === "legacy" ? "Operator" : e.fleet || e.userId)}</td>` : ""}</tr>`;
  });
  const body = !data
    ? empty("Loading activity", "Reading the audit log from the gateway.")
    : rows.length
      ? table(
          [
            "Time",
            "Action",
            "Target",
            "Actor",
            "Source IP",
            ...(operator ? ["Fleet"] : []),
          ],
          rows,
        )
      : empty(
          state.auditCategory ? "No matching activity" : "No activity yet",
          "Sign-ins and management changes appear here as they happen.",
        );
  return `<section class="panel"><div class="table-tools"><select id="audit-category" class="filter" aria-label="Filter activity">${categories.map(([value, text]) => `<option value="${value}" ${value === state.auditCategory ? "selected" : ""}>${text}</option>`).join("")}</select><span class="result-count">${data ? plural(data.events.length, "event") + (data.next ? " loaded" : "") : ""}</span><span class="audit-actions">${data?.events.length ? '<button class="secondary compact" data-action="clear-audit">Clear log</button>' : ""}</span></div>${body}<div class="audit-footer">${data?.next ? '<button class="secondary compact" data-action="audit-more">Load older</button>' : ""}</div></section>`;
}
/** @returns {string} */
function renderAccount() {
  if (!state.session) return "";
  return `<section class="panel content-card"><h2>Browser password</h2><p>Signed in as ${esc(state.session.username)}. Changing your password signs out every browser session for this login. CLI keys stay unchanged.</p><form id="account-form"><label>Current password<input name="currentPassword" type="password" autocomplete="current-password" required maxlength="1024"></label><label>New password<input name="password" type="password" autocomplete="new-password" required minlength="12" maxlength="1024"></label><label>Confirm new password<input name="confirm" type="password" autocomplete="new-password" required minlength="12" maxlength="1024"></label><p class="field-help">Use at least 12 characters. Spaces and password managers are supported.</p><p class="form-error" role="alert"></p><button class="primary" type="submit">Change password</button></form></section>`;
}

/** @returns {void} */
function render() {
  if (!state.session) return;
  const content = $("#page-content");
  const active = document.activeElement;
  let focusSelector = "";
  /** @type {[number | null, number | null] | undefined} */
  let selection;
  if (active instanceof HTMLElement && content.contains(active)) {
    if (active.id) focusSelector = "#" + CSS.escape(active.id);
    else if (active.dataset.action) {
      focusSelector = `[data-action="${CSS.escape(active.dataset.action)}"]`;
      if (active.dataset.id)
        focusSelector += `[data-id="${CSS.escape(active.dataset.id)}"]`;
      if (active.dataset.sort)
        focusSelector += `[data-sort="${CSS.escape(active.dataset.sort)}"]`;
      if (active.dataset.filter)
        focusSelector += `[data-filter="${CSS.escape(active.dataset.filter)}"]`;
    } else if (active.getAttribute("href"))
      focusSelector = `a[href="${CSS.escape(active.getAttribute("href") || "")}"]`;
    if (active instanceof HTMLInputElement && active.id === "machine-search")
      selection = [active.selectionStart, active.selectionEnd];
  }
  if (state.page === "overview")
    content.innerHTML = state.snapshot
      ? renderOverview()
      : empty("Loading your fleet", "Waiting for the gateway snapshot.");
  if (state.page === "machines") {
    if (!find("#machine-search", content)) content.innerHTML = machinePanel(false);
    else {
      // Keep native inputs alive during both typing and background polling.
      // Replacing an input can lose keystrokes, selection, or an open dropdown.
      const template = document.createElement("template");
      template.innerHTML = machinePanel(false);
      for (const selector of [
        ".table-wrap, .empty",
        ".result-count",
        ".state-filter",
      ]) {
        $(selector, content).replaceWith($(selector, template.content));
      }
    }
  }
  if (state.page === "activity") {
    if (!find("#audit-category", content)) content.innerHTML = renderActivity();
    else {
      // Keep the filter control alive across polling, as on the Machines page.
      const template = document.createElement("template");
      template.innerHTML = renderActivity();
      for (const selector of [
        ".table-wrap, .empty",
        ".result-count",
        ".audit-actions",
        ".audit-footer",
      ]) {
        $(selector, content).replaceWith($(selector, template.content));
      }
    }
  }
  if (state.page === "account") content.innerHTML = renderAccount();
  if (["invitations", "keys", "users", "updates"].includes(state.page)) {
    const auxiliary = state.auxiliary;
    if (!auxiliary || auxiliary.page !== state.page)
      content.innerHTML = empty("Loading", "Fetching current records from the gateway.");
    else switch (auxiliary.page) {
      case "invitations": content.innerHTML = renderInvitations(auxiliary.data); break;
      case "keys": content.innerHTML = renderKeys(auxiliary.data); break;
      case "users": content.innerHTML = renderUsers(auxiliary.data); break;
      case "updates": content.innerHTML = renderUpdates(auxiliary.data); break;
    }
  }
  enhanceSelects(content);
  content.setAttribute("aria-busy", "false");
  const replacement = focusSelector && find(focusSelector, content);
  if (replacement) {
    replacement.focus({ preventScroll: true });
    if (selection && replacement instanceof HTMLInputElement) replacement.setSelectionRange(...selection);
  }
  syncMachineMenu();
}
/** @param {string} value @returns {string} */
function clock(value) {
  return new Date(value).toLocaleTimeString([], {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}
/** @param {string} message @returns {void} */
function banner(message) {
  state.stale = !!message;
  $("#stale-banner").hidden = !message;
  $("#stale-banner").textContent =
    message && state.updatedAt
      ? message + " Last update " + clock(state.updatedAt) + "."
      : message;
}
/** @param {boolean} [manual] @returns {Promise<void>} */
async function refresh(manual = false) {
  if (!state.session || state.busy || (document.hidden && !manual)) return;
  state.busy = true;
  const page = state.page,
    session = state.session,
    button = $(".heading-actions [data-action=refresh]");
  if (manual) button.classList.add("spinning");
  try {
    const snapshot = await api("status");
    if (session !== state.session) return;
    state.snapshot = snapshot;
    state.updatedAt = snapshot.observedAt;
    $("#nav-count").textContent = String(groupedMachines(snapshot.nodes).length);
    /** @type {Auxiliary | null} */
    let auxiliary = null;
    switch (page) {
      case "invitations": auxiliary = {page, data: await api("fleet/installations")}; break;
      case "keys": auxiliary = {page, data: await api("fleet/keys")}; break;
      case "users": auxiliary = {page, data: await api("admin/users")}; break;
      case "updates": auxiliary = {page, data: await api("admin/updates")}; break;
      case "activity": auxiliary = {page, data: await api(auditPath())}; break;
    }
    if (session !== state.session) return;
    if (auxiliary && page === state.page)
      state.auxiliary = auxiliary.page === "activity"
        ? {page: "activity", data: mergeAudit(auxiliary.data)} : auxiliary;
    if (state.invitation && $("#dialog").open) await updateInvitation();
    if (state.startupChange && $("#dialog").open) {
      const node = snapshot.nodes.find((n) => n.id === state.startupChange?.id);
      $("#startup-progress").textContent =
        node?.online && node.runAs === "system"
          ? node.name + " is online in System context. You can close this dialog."
          : Date.now() - state.startupChange.startedAt > 60000
            ? "System startup was not confirmed. Inspect the machine and startup-change.log before using the local recovery command. No automatic retry was made."
            : "Waiting for the machine to reconnect and report System context…";
    }
    banner("");
    // Resource refreshes preserve focus. Never replace an in-progress password form.
    if (page === state.page && state.page !== "account") render();
  } catch (error) {
    if (state.session) banner(errorMessage(error));
  } finally {
    state.busy = false;
    // Keep fast manual refreshes visible as one short turn of the icon.
    if (manual) setTimeout(() => button.classList.remove("spinning"), 500);
    if (pendingRefresh) {
      pendingRefresh = false;
      queueMicrotask(() => refresh());
    }
  }
}
/** @returns {void} */
function navigate() {
  if (!state.session) return;
  closeMachineMenu();
  let page = location.hash.slice(1) || "overview";
  if (
    !pages[page] ||
    (["users", "updates"].includes(page) && state.session.role !== "superuser")
  )
    page = "overview";
  state.page = page;
  state.auxiliary = null;
  const info = pages[page];
  $("#page-title").textContent = info[0];
  $("#page-description").textContent = info[1];
  // Refresh never re-renders the password form, so it has nothing to update there.
  $(".heading-actions [data-action=refresh]").hidden = page === "account";
  $(".heading-actions [data-action=add]").hidden = ![
    "overview",
    "machines",
    "invitations",
  ].includes(page);
  document.title = "Control · " + info[0];
  /** @type {NodeListOf<HTMLElement>} */ (document.querySelectorAll("[data-page]")).forEach((a) => {
    a.classList.toggle("active", a.dataset.page === page);
    if (a.dataset.page === page) a.setAttribute("aria-current", "page");
    else a.removeAttribute("aria-current");
  });
  fitNav();
  $(".content-scroll").scrollTop = 0;
  render();
  // A previous page's request cannot prevent the new page fetching its data.
  if (state.busy) pendingRefresh = true;
  else refresh();
}
let pendingRefresh = false;
window.addEventListener("hashchange", navigate);

// One popover keeps focus through table polling and moves into a modal drawer
// when opened there, so the modal does not make its actions inert.
/** @type {{id: string, action: string, drawer: boolean} | null} */
let machineMenuTarget = null;
/** @returns {HTMLElement | null} */
function machineMenuSource() {
  return (
    machineMenuTarget &&
    find(
      `${machineMenuTarget.drawer ? ".drawer-actions" : ".machine-table"} [data-action="${machineMenuTarget.action}"][data-id="${CSS.escape(machineMenuTarget.id)}"]`,
    )
  );
}
/** @param {boolean} [restoreFocus] @returns {void} */
function closeMachineMenu(restoreFocus = false) {
  const menu = $("#machine-menu");
  if (!menu.matches(":popover-open")) return;
  const source = machineMenuSource();
  menu.hidePopover();
  if (restoreFocus) source?.focus({ preventScroll: true });
}
/** @returns {void} */
function syncMachineMenu() {
  const menu = $("#machine-menu");
  if (!menu.matches(":popover-open")) return;
  const target = machineMenuTarget;
  const node = state.snapshot?.nodes.find((n) => n.id === target?.id);
  if (!node || !target || !machineMenuSource()) {
    closeMachineMenu();
    return;
  }
  /** @param {string} action @param {string} label @param {boolean} [danger] @returns {string} */
  const item = (action, label, danger = false) =>
    `<button role="menuitem" tabindex="-1" class="menu-item${danger ? " destructive" : ""}" data-action="${action}" data-id="${esc(node.id)}">${label}</button>`;
  const startup = canChangeStartup(node) ? item("startup", "Startup context") : "",
    secrets = node.online && !node.disabled ? item("worker-secrets", "Secrets") : "",
    separator = '<div role="separator" class="menu-separator"></div>';
  const html = target.drawer
    ? secrets + startup + (secrets || startup ? separator : "") + item("unregister", "Unregister", true)
    : item("details", "View details") +
      item("rename", "Rename") +
      secrets +
      startup +
      (!node.online && node.wakeVia ? item("wake", "Wake") : "") +
      item("toggle", node.disabled ? "Enable" : "Disable") +
      separator +
      item("unregister", "Unregister", true);
  if (menu.innerHTML !== html) {
    const focused = menu.contains(document.activeElement),
      action = document.activeElement instanceof HTMLElement ? document.activeElement.dataset.action : "";
    menu.innerHTML = html;
    if (focused)
      (
        find(`[data-action="${action}"]`, menu) || $("[role=menuitem]", menu)
      ).focus();
  }
  menu.setAttribute("aria-label", "Actions for " + node.name);
  const trigger = find(
    `${target.drawer ? ".drawer-actions" : ".machine-table"} .machine-menu-button[data-id="${CSS.escape(node.id)}"]`,
  );
  trigger?.setAttribute("aria-expanded", "true");
}
/** @param {HTMLElement} source @param {{x: number, y: number}} [point] @returns {void} */
function openMachineMenu(source, point) {
  closeMachineMenu();
  document
    .querySelectorAll("[popover]:popover-open")
    .forEach((popover) => { if (popover instanceof HTMLElement) popover.hidePopover(); });
  machineMenuTarget = {
    id: source.dataset.id || "",
    action: source.dataset.action || "",
    drawer: !!source.closest(".drawer-actions"),
  };
  const menu = $("#machine-menu"),
    box = source.getBoundingClientRect();
  (machineMenuTarget.drawer ? $("#dialog") : document.body).append(menu);
  menu.showPopover();
  syncMachineMenu();
  if (!menu.matches(":popover-open")) return;
  menu.style.left =
    Math.max(
      8,
      Math.min(point?.x ?? box.left, innerWidth - menu.offsetWidth - 8),
    ) + "px";
  menu.style.top =
    Math.max(
      8,
      Math.min(point?.y ?? box.bottom + 4, innerHeight - menu.offsetHeight - 8),
    ) + "px";
  $("[role=menuitem]", menu).focus({ preventScroll: true });
}
$("#machine-menu").addEventListener("beforetoggle", (event) => {
  if (event.newState !== "closed") return;
  document
    .querySelectorAll('.machine-menu-button[aria-expanded="true"]')
    .forEach((button) => button.setAttribute("aria-expanded", "false"));
});
// Native light-dismiss can close a context menu on its opening right-button
// release. Use a manual popover and dismiss only on a new primary press outside.
document.addEventListener(
  "pointerdown",
  (event) => {
    if (
      event.button !== 0 ||
      event.ctrlKey ||
      (event.target instanceof Element && event.target.closest("#machine-menu, .machine-menu-button"))
    )
      return;
    closeMachineMenu();
  },
  true,
);
$("#machine-menu").addEventListener("pointermove", () => {
  delete document.documentElement.dataset.keyboard;
});
$("#machine-menu").addEventListener("keydown", (event) => {
  const items = [.../** @type {NodeListOf<HTMLElement>} */ ($("#machine-menu").querySelectorAll("[role=menuitem]"))],
    index = document.activeElement instanceof HTMLElement ? items.indexOf(document.activeElement) : -1,
    moves = /** @type {Record<string, number>} */ ({
      ArrowDown: (index + 1) % items.length,
      ArrowUp: (index + items.length - 1) % items.length,
      Home: 0,
      End: items.length - 1,
    });
  if (event.key in moves) {
    event.preventDefault();
    items[moves[event.key]].focus();
  } else if (event.key === "Escape" || event.key === "Tab") {
    if (event.key === "Escape") event.preventDefault();
    closeMachineMenu(true);
  }
});
document.addEventListener("contextmenu", (event) => {
  if (!(event.target instanceof Element)) return;
  const row = event.target.closest(".machine-table tbody tr");
  if (!row || event.shiftKey) return;
  event.preventDefault();
  openMachineMenu($(".machine-name", row), {
    x: event.clientX,
    y: event.clientY,
  });
});
document.addEventListener("keydown", (event) => {
  if (!(event.target instanceof Element)) return;
  const source = event.target.closest(".machine-name, .machine-menu-button");
  if (!(source instanceof HTMLElement)) return;
  if (
    event.key === "ContextMenu" ||
    (event.shiftKey && event.key === "F10") ||
    (source.dataset.action === "machine-menu" &&
      ["ArrowDown", "ArrowUp"].includes(event.key))
  ) {
    event.preventDefault();
    openMachineMenu(source);
    if (event.key === "ArrowUp")
      $("#machine-menu [role=menuitem]:last-child").focus();
  }
});
document.addEventListener(
  "scroll",
  (event) => {
    if (!(event.target instanceof Node) || !$("#machine-menu").contains(event.target)) closeMachineMenu();
  },
  true,
);
window.addEventListener("resize", () => closeMachineMenu());
document.addEventListener("input", (event) => {
  if (!(event.target instanceof HTMLInputElement) || event.target.id !== "machine-search") return;
  state.search = event.target.value;
  const start = event.target.selectionStart;
  render();
  $("#machine-search").focus();
  $("#machine-search").setSelectionRange(start, start);
});
document.addEventListener("change", (event) => {
  if (event.target instanceof HTMLSelectElement && event.target.id === "audit-category") {
    state.auditCategory = event.target.value;
    state.auxiliary = null;
    render();
    if (state.busy) pendingRefresh = true;
    else refresh();
  }
});

// Styled listbox over a hidden native select. The select stays the source of
// truth for form data and filters; the button and popover only present it.
let selectCount = 0;
/** @param {ParentNode} root @returns {void} */
function enhanceSelects(root) {
  /** @type {NodeListOf<EnhancedSelect>} */ (root.querySelectorAll("select")).forEach((select) => {
    if (select.syncListbox) {
      select.syncListbox();
      return;
    }
    const id = "listbox-" + ++selectCount,
      options = [...select.options],
      name =
        select.getAttribute("aria-label") ||
        select.closest("label")?.firstChild?.textContent?.trim() ||
        "";
    const trigger = document.createElement("button");
    trigger.type = "button";
    trigger.className =
      "select-trigger" + (select.classList.contains("filter") ? " filter" : "");
    trigger.setAttribute("aria-haspopup", "listbox");
    trigger.setAttribute("aria-expanded", "false");
    trigger.setAttribute("aria-controls", id);
    trigger.innerHTML = `<span class="select-value"></span>${icon("chevron")}`;
    const list = document.createElement("div");
    list.id = id;
    list.className = "select-list";
    list.setAttribute("role", "listbox");
    list.setAttribute("aria-label", name);
    list.tabIndex = -1;
    list.popover = "auto";
    list.innerHTML = options
      .map(
        (option, i) =>
          `<div role="option" id="${id}-${i}" class="select-option" data-value="${esc(option.value)}">${esc(option.textContent)}${icon("check")}</div>`,
      )
      .join("");
    select.classList.add("native-select");
    select.tabIndex = -1;
    select.setAttribute("aria-hidden", "true");
    select.before(trigger);
    select.after(list);
    let active = 0,
      closedAt = 0;
    const items = [...list.children];
    /** @param {number} index @returns {void} */
    const highlight = (index) => {
      active = Math.max(0, Math.min(items.length - 1, index));
      items.forEach((item, i) => item.classList.toggle("active", i === active));
      list.setAttribute("aria-activedescendant", items[active].id);
      items[active].scrollIntoView({ block: "nearest" });
    };
    select.syncListbox = () => {
      const current = select.selectedIndex;
      $(".select-value", trigger).textContent = options[current]?.textContent || "";
      trigger.setAttribute(
        "aria-label",
        name + ": " + (options[current]?.textContent || ""),
      );
      items.forEach((item, i) =>
        item.setAttribute("aria-selected", String(i === current)),
      );
    };
    /** @param {number} index @returns {void} */
    const choose = (index) => {
      if (select.selectedIndex !== index) {
        select.selectedIndex = index;
        select.dispatchEvent(new Event("change", { bubbles: true }));
      }
      select.syncListbox?.();
      list.hidePopover();
      trigger.focus();
    };
    trigger.addEventListener("click", () => {
      // A click on the trigger first light-dismisses an open list; don't reopen it.
      if (performance.now() - closedAt > 250) open();
    });
    trigger.addEventListener("keydown", (event) => {
      if (!["ArrowDown", "ArrowUp"].includes(event.key)) return;
      event.preventDefault();
      open();
    });
    // Measure the shown list before paint. It scrolls only when neither side
    // of the trigger has room, and opens upward when that side has more.
    const open = () => {
      list.showPopover();
      const box = trigger.getBoundingClientRect();
      list.style.maxHeight = "none";
      list.style.minWidth = box.width + "px";
      const natural = list.offsetHeight,
        below = innerHeight - box.bottom - 16,
        above = box.top - 16,
        up = natural > below && above > below,
        height = Math.min(natural, up ? above : below);
      list.style.maxHeight = height + "px";
      list.style.top = (up ? box.top - 4 - height : box.bottom + 4) + "px";
      list.style.left =
        Math.max(12, Math.min(box.left, innerWidth - list.offsetWidth - 12)) +
        "px";
    };
    list.addEventListener("beforetoggle", (event) => {
      trigger.setAttribute("aria-expanded", String(event.newState === "open"));
      if (event.newState === "closed") closedAt = performance.now();
    });
    list.addEventListener("toggle", (event) => {
      if (event.newState !== "open") return;
      highlight(select.selectedIndex);
      list.focus({ preventScroll: true });
    });
    list.addEventListener("click", (event) => {
      if (!(event.target instanceof Element)) return;
      const item = event.target.closest("[role=option]");
      if (item) choose(items.indexOf(item));
    });
    list.addEventListener("pointermove", (event) => {
      if (!(event.target instanceof Element)) return;
      const item = event.target.closest("[role=option]");
      if (item && items.indexOf(item) !== active)
        highlight(items.indexOf(item));
    });
    list.addEventListener("keydown", (event) => {
      const moves = /** @type {Record<string, number>} */ ({
        ArrowDown: active + 1,
        ArrowUp: active - 1,
        Home: 0,
        End: items.length - 1,
      });
      if (event.key in moves) highlight(moves[event.key]);
      else if (event.key === "Enter" || event.key === " ") choose(active);
      else if (event.key === "Tab") list.hidePopover();
      else if (event.key.length === 1) {
        // Type-ahead jumps to the next option starting with the typed letter.
        const letter = event.key.toLowerCase(),
          next = items.findIndex(
            (item, i) =>
              i > active && (item.textContent || "").toLowerCase().startsWith(letter),
          );
        const first = items.findIndex((item) =>
          (item.textContent || "").toLowerCase().startsWith(letter),
        );
        if (next >= 0 || first >= 0) highlight(next >= 0 ? next : first);
      } else return;
      if (event.key !== "Tab") event.preventDefault();
    });
    select.syncListbox();
  });
}

/** @type {ReturnType<typeof setTimeout> | undefined} */
let toastTimer;
/** @param {string} message @returns {void} */
function toast(message) {
  clearTimeout(toastTimer);
  $("#toast").textContent = message;
  $("#toast").hidden = false;
  toastTimer = setTimeout(() => {
    $("#toast").hidden = true;
  }, 5000);
}
/** @returns {void} */
function closeDialog() {
  closeMachineMenu();
  state.invitation = null;
  state.startupChange = null;
  $("#dialog").close();
  $("#dialog-body").replaceChildren();
}
$("#dialog").addEventListener("cancel", (event) => {
  event.preventDefault();
  closeDialog();
});
/** @param {string} title @param {string} html @param {string} [className] @returns {void} */
function dialog(title, html, className = "") {
  closeMachineMenu();
  $("#dialog").className = className;
  $("#dialog-title").textContent = title;
  $("#dialog-body").innerHTML = html;
  enhanceSelects($("#dialog-body"));
  if (!$("#dialog").open) $("#dialog").showModal();
  const first = find(
    "#dialog-body input:not([readonly]):not([disabled]), #dialog-body .select-trigger, #dialog-body textarea:not([readonly]):not([disabled])",
  );
  (first || $("#dialog-title")).focus({ preventScroll: true });
}
/** @param {string} title @param {string} html @param {(fields: Record<string, string>) => Promise<boolean | void>} handler @param {string} [label] @param {boolean} [danger] @param {string} [className] @returns {void} */
function formDialog(title, html, handler, label = "Save", danger = false, className = "") {
  dialog(
    title,
    `<form id="modal-form">${html}<p class="form-error" role="alert"></p><div class="form-actions"><button type="button" data-action="close" class="secondary">Cancel</button><button type="submit" class="${danger ? "danger" : "primary"}">${esc(label)}</button></div></form>`,
    className,
  );
  $("#modal-form").addEventListener("submit", async (event) => {
    event.preventDefault();
    const form = $("#modal-form"),
      button = $("button[type=submit]", form),
      errorBox = $(".form-error", form);
    const fields = formFields(form);
    button.disabled = true;
    errorBox.textContent = "";
    try {
      const keep = await handler(fields);
      if (keep !== false) closeDialog();
      refresh();
    } catch (error) {
      errorBox.textContent = errorMessage(error);
      if (
        error instanceof Error && "status" in error && error.status === 409 &&
        error.message === "name has an existing invitation" &&
        !find("#force-confirm", form)
      ) {
        const confirmation = document.createElement("label");
        confirmation.className = "force-confirm";
        confirmation.innerHTML =
          '<input id="force-confirm" name="force" type="checkbox">Retract the existing pending invitation. Its installation URL will stop working immediately.';
        errorBox.before(confirmation);
      }
    } finally {
      button.disabled = false;
    }
  });
}

// Encrypt in the browser. Neither plaintext values nor private keys are sent to
// the gateway, persisted in browser storage, or placed in HTML attributes.
/** @param {Machine} node @param {string} name @param {string} value @param {string} publicKey @returns {Promise<SecretEnvelope>} */
async function encryptWorkerSecret(node, name, value, publicKey) {
  const encode = /** @param {ArrayBuffer | Uint8Array<ArrayBuffer>} bytes */ (bytes) => btoa(String.fromCharCode(...new Uint8Array(bytes))),
    decode = /** @param {string} text */ (text) => Uint8Array.from(atob(text), (c) => c.charCodeAt(0)),
    text = new TextEncoder(),
    plain = text.encode(value);
  if (!plain.length || plain.length > 4096 || value.includes("\0"))
    throw new Error("The value must contain 1 to 4096 UTF-8 bytes without NUL.");
  if (!crypto.subtle)
    throw new Error("Encrypted secret entry requires HTTPS and Web Crypto support.");
  try {
    const pair = await crypto.subtle.generateKey({ name: "ECDH", namedCurve: "P-256" }, false, ["deriveBits"]),
      recipient = await crypto.subtle.importKey("raw", decode(publicKey), { name: "ECDH", namedCurve: "P-256" }, false, []),
      shared = new Uint8Array(await crypto.subtle.deriveBits({ name: "ECDH", public: recipient }, pair.privateKey, 256));
    /** @type {CryptoKey} */
    let material;
    try {
      material = await crypto.subtle.importKey("raw", shared, "HKDF", false, ["deriveKey"]);
    } finally {
      shared.fill(0);
    }
    const key = await crypto.subtle.deriveKey(
        { name: "HKDF", hash: "SHA-256", salt: new Uint8Array(), info: text.encode("control-secrets-v1\0" + node.id) },
        material, { name: "AES-GCM", length: 256 }, false, ["encrypt"],
      ),
      id = [...crypto.getRandomValues(new Uint8Array(16))].map((b) => b.toString(16).padStart(2, "0")).join(""),
      expiresAt = Math.floor(Date.now() / 1000) + 30,
      nonce = crypto.getRandomValues(new Uint8Array(12)),
      additionalData = text.encode(["control-secrets-v1", node.id, name, id, expiresAt].join("\0")),
      ciphertext = await crypto.subtle.encrypt({ name: "AES-GCM", iv: nonce, additionalData }, key, plain);
    return { id, expiresAt, key: encode(await crypto.subtle.exportKey("raw", pair.publicKey)), nonce: encode(nonce), ciphertext: encode(ciphertext) };
  } finally {
    plain.fill(0);
  }
}

/** @param {Machine} node @param {Element} [source] @returns {Promise<void>} */
async function workerSecrets(node, source) {
  const session = state.session;
  const result = await api(`fleet/nodes/${encodeURIComponent(node.id)}/secrets`);
  if (state.session !== session || (source && !source.isConnected)) return;
  dialog(
    "Secrets on " + node.name,
    `<p>Values cannot be viewed. New and replacement values are encrypted to this worker before leaving your browser. Updated records the last local secret change, not application-side rotation.</p><div class="secrets-toolbar"><span class="muted">${result.secrets.length} ${result.secrets.length === 1 ? "secret" : "secrets"}</span><button class="icon-button" data-action="secret-set" data-id="${esc(node.id)}" aria-label="Add secret" title="Add secret">${icon("plus")}</button></div><div class="table-wrap" tabindex="0" role="region" aria-label="Secret metadata"><table class="secret-table"><thead><tr><th scope="col">Name</th><th scope="col">Updated</th><th scope="col">Actions</th></tr></thead><tbody>${result.secrets.map((entry) => `<tr><td class="mono">${esc(entry.name)}</td><td>${esc(date(entry.updatedAt))}</td><td><div class="secret-row-actions"><button class="icon-button" data-action="secret-set" data-id="${esc(node.id)}" data-name="${esc(entry.name)}" aria-label="Replace ${esc(entry.name)}" title="Replace secret">${icon("edit")}</button><button class="icon-button destructive" data-action="secret-delete" data-id="${esc(node.id)}" data-name="${esc(entry.name)}" aria-label="Delete ${esc(entry.name)}" title="Delete secret">${icon("trash")}</button></div></td></tr>`).join("") || '<tr><td colspan="3">No secrets defined.</td></tr>'}</tbody></table></div>`,
    "machine-drawer secrets-drawer",
  );
}

/** @param {Machine} node @param {string} [name] @returns {Promise<void>} */
async function setWorkerSecret(node, name = "") {
  // Check access and fetch the current process's key before asking for a value.
  const session = state.session;
  const result = await api(`fleet/nodes/${encodeURIComponent(node.id)}/secrets`);
  if (state.session !== session) return;
  if (!CSS.supports("-webkit-text-security", "disc"))
    throw new Error("This browser cannot mask secret entry safely. Use control secrets set NAME --node WORKER in your terminal.");
  dialog(
    (name ? "Replace secret on " : "Add secret on ") + node.name,
    `<div id="secret-editor" role="group" aria-label="Worker secret entry" data-1p-ignore data-lpignore="true" data-bwignore><p>The value is write-only and encrypted to this worker. A failed or interrupted request is never retried automatically. Replacing a name takes effect on its next use.</p><label for="entry-reference">Secret name</label><textarea id="entry-reference" data-entry="reference" rows="1" wrap="off" aria-multiline="false" required maxlength="128" autocomplete="off" autocapitalize="off" autocorrect="off" spellcheck="false" data-1p-ignore data-lpignore="true" data-bwignore${name ? " readonly" : ""}>${esc(name)}</textarea><label for="entry-content">Secret value</label><textarea id="entry-content" data-entry="content" rows="1" wrap="off" aria-multiline="false" class="masked-secret" required maxlength="4096" autocomplete="off" autocapitalize="off" autocorrect="off" spellcheck="false" data-1p-ignore data-lpignore="true" data-bwignore></textarea><p class="form-error" role="alert"></p><div class="form-actions"><button type="button" data-cancel-entry class="secondary">Cancel</button><button type="button" class="primary" data-save-entry>${name ? "Replace secret" : "Save secret"}</button></div></div>`,
    "machine-drawer secrets-drawer",
  );
  // Safari treats CSS-masked HTML inputs, including search inputs, as password
  // fields. These textarea-backed editors remain strictly one line high, without
  // wrapping or resize handles. No login form or name attributes are present.
  const editor = $("#secret-editor"),
    reference = $("#entry-reference", editor),
    content = $("#entry-content", editor),
    save = $("button[data-save-entry]", editor),
    errorBox = $(".form-error", editor);
  $("[data-cancel-entry]", editor).addEventListener("click", async () => {
    reference.value = "";
    content.value = "";
    // Remove entry controls immediately so late encryption/write completions
    // cannot submit or replace the view after cancellation.
    dialog("Secrets on " + node.name, '<p id="secret-list-loading" role="status">Loading secrets…</p>', "machine-drawer secrets-drawer");
    const loading = $("#secret-list-loading");
    try {
      await workerSecrets(node, loading);
    } catch {
      if (!loading.isConnected || state.session !== session) return;
      dialog("Secrets on " + node.name,
        `<p>The secrets list could not be refreshed.</p><div class="form-actions"><button type="button" class="secondary" data-action="worker-secrets" data-id="${esc(node.id)}">Reload secrets</button></div>`,
        "machine-drawer secrets-drawer");
    }
  });
  for (const field of [reference, content]) {
    field.addEventListener("beforeinput", (event) => {
      if (event.inputType === "insertLineBreak" || event.inputType === "insertParagraph") event.preventDefault();
    });
    field.addEventListener("keydown", (event) => {
      if (event.key === "Enter" && !event.isComposing) event.preventDefault();
    });
  }
  reference.addEventListener("input", () => reference.setCustomValidity(""));
  content.addEventListener("input", () => content.setCustomValidity(""));
  save.addEventListener("click", async () => {
    if (save.disabled) return;
    reference.setCustomValidity(/^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$/.test(reference.value)
      ? "" : "Use 1 to 128 letters, digits, dots, underscores or hyphens, starting with a letter or digit.");
    content.setCustomValidity(/[\r\n]/.test(content.value) ? "Use a single-line value here. For multiline credentials, use the CLI." : "");
    if (!reference.reportValidity() || !content.reportValidity()) return;
    const entryName = reference.value;
    let value = content.value;
    content.value = "";
    save.disabled = true;
    errorBox.textContent = "";
    let saved = false;
    try {
      const envelope = await encryptWorkerSecret(node, entryName, value, result.key);
      value = "";
      if (!editor.isConnected || state.session !== session) return;
      await api(`fleet/nodes/${encodeURIComponent(node.id)}/secrets/${encodeURIComponent(entryName)}`, "PUT", envelope);
      if (!editor.isConnected || state.session !== session) return;
      toast("Secret saved on " + node.name + ". Its value cannot be retrieved.");
      saved = true;
      await workerSecrets(node, editor);
      refresh();
    } catch (error) {
      if (!editor.isConnected || state.session !== session) return;
      if (saved) {
        // The write succeeded. Offer only a read retry, never a second write.
        dialog("Secrets on " + node.name,
          `<p>The secret was saved, but the list could not be refreshed. Reload the list to inspect its metadata.</p><div class="form-actions"><button type="button" class="secondary" data-action="worker-secrets" data-id="${esc(node.id)}">Reload secrets</button></div>`,
          "machine-drawer secrets-drawer");
      } else {
        errorBox.textContent = errorMessage(error);
      }
    } finally {
      value = "";
      save.disabled = false;
    }
  });
}
/** @param {string} title @param {string} value @param {string} description @returns {void} */
function showSecret(title, value, description) {
  dialog(
    title,
    `<p>${esc(description)}</p><pre class="secret-area" id="secret-value"></pre><div class="form-actions"><button data-action="close" class="secondary">Done</button><button data-action="copy" class="primary">Copy</button></div>`,
  );
  $("#secret-value").textContent = value;
}
/** @param {string} title @param {string} message @param {() => Promise<unknown>} handler @param {string} [label] @param {string} [className] @returns {void} */
function confirmAction(title, message, handler, label = "Confirm", className = "") {
  formDialog(
    title,
    `<p>${esc(message)}</p>`,
    async () => {
      await handler();
      toast("Action completed");
    },
    label,
    true,
    className,
  );
}
/** @param {Machine} node @returns {boolean} */
function canChangeStartup(node) {
  return (
    node.runAs === "user" && ["linux", "darwin", "windows"].includes(node.os)
  );
}
/** @param {string} value @param {boolean} windows @returns {string} */
function commandQuote(value, windows) {
  return "'" + value.replaceAll("'", windows ? "''" : "'\\''") + "'";
}
/** @param {Machine} node @returns {void} */
function changeStartup(node) {
  state.invitation = null;
  state.startupChange = null;
  const windows = node.os === "windows";
  /** @type {Map<string, StartupResult>} */
  const attempts = new Map();
  formDialog(
    "Change startup context for " + node.name,
    `<p>Request a startup change on ${esc(node.name)} while it is idle. If the node lacks permission or needs a local installer, the console shows a command to run on that machine. No UAC or sudo prompt is opened remotely.</p><label>Startup context<select name="serviceMode"><option value="system">System context</option><option value="hybrid">Hybrid</option></select></label><div id="startup-guidance" class="notice"></div><label id="system-name-field"${windows ? " hidden" : ""}>New System routing name<input name="systemName" value="${esc(node.name.slice(0, 56) + "-system")}" maxlength="63" pattern="[a-zA-Z0-9]([a-zA-Z0-9._]|-){0,62}"></label><label>Existing node configuration, optional<input name="config" placeholder="Absolute path on the target machine" autocomplete="off"></label><p class="field-help">The remote attempt always uses the running node's profile. This path applies only to the local fallback command. Leave blank for the installing user's default profile. System startup has no interactive desktop access.</p>`,
    async (fields) => {
      const current = state.snapshot?.nodes.find((n) => n.id === node.id);
      if (
        !current ||
        !canChangeStartup(current) ||
        current.name !== node.name ||
        current.os !== node.os
      )
        throw new Error("The machine changed. Close this dialog and inspect its current state.");
      const mode = fields.serviceMode,
        config = fields.config.trim(),
        cli = "control" + (config ? " --config " + commandQuote(config, windows) : "");
      const name = mode === "hybrid" ? node.name : windows ? node.name : fields.systemName.trim();
      if (mode === "hybrid" && name.length > 56)
        throw new Error("Hybrid names allow at most 56 characters. Rename this machine first.");
      if (mode === "system" && !windows && (!name || state.snapshot?.nodes.some((n) => n.name === name)))
        throw new Error("Choose an unused System routing name. Unix System installation cannot reuse the User identity.");
      if (mode === "hybrid" && state.snapshot?.nodes.some((n) => n.name === name + "-system"))
        throw new Error("The System companion name is already registered. Manage that context separately.");
      if (!attempts.has(mode)) {
        try {
          attempts.set(mode, await api(`fleet/nodes/${encodeURIComponent(node.id)}/startup`, "POST", { mode }));
        } catch (error) {
          if (error instanceof Error && "status" in error && (error.status === 401 || error.status === 403)) throw error;
          attempts.set(mode, { status: "unknown", reason: errorMessage(error) });
        }
      }
      const attempt = attempts.get(mode);
      if (!attempt) throw new Error("Startup request has no recorded outcome.");
      const outcome = attempt.status === "accepted"
        ? "The machine accepted the change. Wait for System context to reconnect. The displayed command is for local recovery only; do not run it while the change is in progress. "
        : (attempt.reason || "The node could not apply the change remotely.") + " No automatic retry was made. ";
      if (mode === "system" && windows) {
        showSecret(
          attempt.status === "accepted" ? "Changing startup on " + node.name : "Run on " + node.name,
          cli + " service start --mode auto",
          outcome + "For local recovery, run in Administrator PowerShell under the same installing user, not through the node's exec provider. This switches to boot-time LocalService startup and retains the identity, profile, workspace, and task history. Use a current installed Control CLI.",
        );
        state.startupChange = { id: node.id, startedAt: Date.now() };
        const note = document.createElement("p");
        note.id = "startup-progress";
        note.className = "progress-note";
        note.setAttribute("role", "status");
        note.textContent = "Waiting for the machine to report System context…";
        $("#dialog-body").append(note);
        return false;
      }
      const link = await api("fleet/installations", "POST", {
        name,
        os: node.os,
        serviceMode: mode,
        ttlSeconds: 900,
        gateway: location.origin,
        force: fields.force === "on",
      });
      if (link.serviceMode !== mode)
        throw new Error("The gateway did not confirm the requested startup context. Inspect Invitations before creating another command.");
      let command = link.command;
      if (mode === "system") command = cli + " service uninstall --mode user &&\n" + command;
      else if (config) {
        command = windows
          ? `& { $previousConfig = $env:CONTROL_CONFIG; try { $env:CONTROL_CONFIG = ${commandQuote(config, true)}; ${command} } finally { $env:CONTROL_CONFIG = $previousConfig } }`
          : "CONTROL_CONFIG=" + commandQuote(config, false) + " bash -c " + commandQuote(command, false);
      }
      showInstallationCommand(
        link,
        command,
        outcome + (mode === "hybrid"
          ? `Run on ${node.name} ${windows ? "in Administrator PowerShell under the installing user's account" : "from the installing user's shell with sudo available, not sudo bash"}. This re-enrolls the existing User profile and adds ${name}-system as a separate protected companion. Explicitly target the companion for System work. Treat this command as a temporary credential.`
          : `Run on ${node.name} from the installing user's shell with sudo available. This first stops and removes User startup, then installs a new root-owned profile as ${name}. Identity, credentials, files, and task history are not migrated. The old registration and files remain; unregister the old machine separately only after verifying System startup. Treat this command as a temporary credential.`),
      );
      return false;
    },
    "Apply startup context",
  );
  const sync = () => {
    const hybrid = $('#modal-form select[name="serviceMode"]').value === "hybrid";
    $("#system-name-field").hidden = windows || hybrid;
    $('#modal-form input[name="systemName"]').required = !windows && !hybrid;
    $('#modal-form input[name="systemName"]').disabled = windows || hybrid;
    $("#startup-guidance").textContent = hybrid
      ? `Keep ${node.name} for User work and add ${node.name}-system for explicit System work. Both have independent state and startup. The gateway needs published installers. No automatic elevation or fallback occurs.`
      : windows
        ? "Try switching the running profile to boot-time LocalService startup. This requires the node itself to already have administrator rights. Otherwise, run the fallback in Administrator PowerShell. No invitation is needed."
        : "Unix System startup requires a separate root-owned profile and an unused routing name. The command removes User startup but keeps its files and registration. Use Hybrid instead to keep both contexts available.";
  };
  $('#modal-form select[name="serviceMode"]').addEventListener("change", sync);
  sync();
}
/** @param {InstallationLink} link @param {string} [command] @param {string} [description] @returns {void} */
function showInstallationCommand(
  link,
  command = link.command,
  description = "Run this command on the intended machine. Treat it as a temporary credential. It is shown only here and cannot be retrieved later.",
) {
  state.invitation = link;
  showSecret("Install on the target machine", command, description);
  const note = document.createElement("p");
  note.id = "enrollment-progress";
  note.className = "progress-note";
  note.setAttribute("role", "status");
  note.textContent = "Waiting for installation · Expires " + date(link.expiresAt);
  $("#dialog-body").append(note);
}
/** @returns {void} */
function addMachine() {
  state.invitation = null;
  formDialog(
    "Add a machine",
    `<p>Create a one-time installation command for the target machine.</p><label>Machine name<input name="name" placeholder="Use target hostname" maxlength="63" pattern="[a-zA-Z0-9]([a-zA-Z0-9._]|-){0,62}"></label><p class="field-help">Leave blank to use the target's hostname. Existing names create a replacement enrollment.</p><div class="form-grid"><label>Platform<select name="os"><option value="linux">Linux</option><option value="darwin">macOS</option><option value="windows">Windows</option></select></label><label>Startup context<select name="serviceMode"><option value="user">User context</option><option value="system">System context</option><option value="hybrid">Hybrid</option></select></label></div><p class="field-help">User uses your files and desktop session and may stop at logout. System starts at boot as root on Unix or LocalService on Windows. Hybrid installs both as isolated profiles, with a NAME-system companion running as root or Windows LocalSystem. Explicit System work requires an instruction grant; failures never trigger elevation. Hybrid names allow 56 characters. Run Hybrid as your user on Unix, or in Administrator PowerShell on Windows.</p><div class="notice">The gateway needs published amd64 and arm64 installers for the selected platform. This invitation expires after 15 minutes.</div>`,
    async (fields) => {
      const name = fields.name.trim(),
        automatic = !name || name === "auto";
      const link = await api("fleet/installations", "POST", {
        name: automatic ? "" : name,
        autoName: automatic,
        os: fields.os,
        serviceMode: fields.serviceMode,
        ttlSeconds: 900,
        gateway: location.origin,
        force: fields.force === "on",
      });
      showInstallationCommand(link);
      return false;
    },
    "Create installation command",
  );
}
/** @returns {Promise<void>} */
async function updateInvitation() {
  const link = state.invitation;
  if (!link) return;
  const records = await api("fleet/installations");
  if (state.invitation !== link) return;
  const record = records.find((i) => i.id === link.id),
    note = find("#enrollment-progress");
  if (!record || !note) return;
  if (record.revoked)
    note.textContent = "Invitation revoked. This command is no longer valid.";
  else if (record.redeemedId) {
    // This status fetch starts after observing redemption, not before it.
    const current = await api("status");
    if (state.invitation !== link || !find("#enrollment-progress")) return;
    const node = current.nodes.find(
      (n) => n.id === record.redeemedId && n.online,
    );
    if (link.serviceMode === "hybrid") {
      const companion = records.find(
          (i) => i.hybridId === link.hybridId && i.hybridRole === "system",
        ),
        system = current.nodes.find(
          (n) => n.id === companion?.redeemedId && n.online && n.runAs === "system",
        );
      note.textContent = node?.runAs === "user" && system
        ? node.name + " and " + system.name + " are online in User and System contexts. You can close this dialog."
        : "Invitation redeemed. Waiting for both User and System contexts to connect…";
      return;
    }
    const ready = node && (!link.serviceMode || node.runAs === link.serviceMode);
    note.textContent = ready
      ? node.name + " is online. You can close this dialog."
      : "Invitation redeemed. Waiting for the machine to connect…";
  } else if (Date.now() > new Date(record.expiresAt).getTime())
    note.textContent =
      "Invitation expired. Close this dialog and create a new command.";
}
// Owner health only: counts and times, never operation names or peers.
/** @param {Machine} n @returns {string} */
function activitySection(n) {
  if (!n.online || !["summary", "ready"].includes(n.status)) return "";
  const peers = peerCount(n),
    rows = [
      ["Work", n.activeCount > 0 ? workSummary(n) : "Idle"],
      n.leased && [
        "Reserved",
        known(n.leaseExpires) ? "Until " + clock(n.leaseExpires) : "Yes",
      ],
      known(n.lastActive) && ["Last active", date(n.lastActive)],
      peers != null && [
        "Connections",
        peers ? plural(peers, "peer") + " · " + routeText(n) : "No peers",
      ],
      tunnelCount(n) > 0 && ["Tunnels", tunnelText(n)],
    ].filter((row) => Array.isArray(row));
  return `<section class="drawer-section"><h3>Activity</h3><dl class="metadata">${rows.map(([label, value]) => `<dt>${label}</dt><dd>${esc(value)}</dd>`).join("")}</dl></section>`;
}
/** @param {string | undefined} id @returns {void} */
function machineDetails(id) {
  const n = state.snapshot?.nodes.find((node) => node.id === id);
  if (!n) return;
  const sys = n.system || /** @type {Partial<SystemInfo>} */ ({}),
    live = n.online ? sys : /** @type {Partial<SystemInfo>} */ ({}),
    memory = percent(live.memoryUsedBytes, live.memoryTotalBytes),
    detail = statusDetail(n),
    labels = Object.entries(n.labels || {});
  dialog(
    n.name,
    `<div class="drawer-status">${badge(machineState(n))}${detail ? `<span class="muted">${esc(detail)}</span>` : ""}</div><section class="drawer-section"><h3>Resources</h3><dl class="metadata"><dt>CPU utilization</dt><dd>${meter(live.cpuUsagePercent, "CPU utilization")}</dd>${visibleGPUs(sys.gpus).length ? gpuResources(sys.gpus, n.online, true) : ""}<dt>Memory usage</dt><dd>${meter(memory, "Memory utilization")}${sys.memoryTotalBytes ? `<small class="muted">${memory == null ? bytes(sys.memoryTotalBytes) + " total" : bytes(live.memoryUsedBytes) + " of " + bytes(live.memoryTotalBytes)}</small>` : ""}</dd><dt>Disk usage</dt><dd>${
      (live.disks || [])
        .filter((disk) => disk.totalBytes > 0)
        .map(
          (disk) =>
            `<div class="disk-usage">${meter(percent(disk.usedBytes, disk.totalBytes), "Disk utilization for " + disk.path)}<small class="muted mono">${esc(disk.path)}</small><small class="muted">${bytes(disk.usedBytes)} of ${bytes(disk.totalBytes)}</small></div>`,
        )
        .join("") || meter(null, "Disk utilization")
    }</dd><dt>Processor</dt><dd>${esc(sys.cpuModel || "Unknown")}${sys.logicalCPUs ? " · " + esc(sys.logicalCPUs) + " cores" : ""}</dd><dt>Sampled</dt><dd>${esc(date(sys.sampledAt))}${n.online ? "" : `<small class="muted">Cached while offline</small>`}</dd></dl></section>${activitySection(n)}<section class="drawer-section"><h3>System</h3><dl class="metadata"><dt>Platform</dt><dd>${esc(platformText(n) || "Unknown")}</dd>${n.runAs ? `<dt>Runs as</dt><dd>${runAsLabels[n.runAs][1]}</dd>` : ""}<dt>Version</dt><dd>${esc(n.software?.version || "Unknown")}</dd><dt>Labels</dt><dd>${labels.length ? `<span class="labels">${labels.map(([k, v]) => `<span class="label-chip">${esc(k)}=${esc(v)}</span>`).join("")}</span>` : "None"}</dd>${n.online ? "" : `<dt>Last seen</dt><dd>${esc(date(n.lastSeen))}</dd>`}<dt>Identity</dt><dd class="mono">${esc(n.id)}</dd></dl></section>${!n.online && n.wakeVia ? `<p class="drawer-note">Wake sends a Wake-on-LAN packet from ${esc(n.wakeVia)}, an online machine on the same LAN. The machine appears online once its node starts.</p>` : ""}<div class="drawer-actions">${!n.online && n.wakeVia ? `<button class="primary" data-action="wake" data-id="${esc(n.id)}">Wake</button>` : ""}<button class="secondary" data-action="rename" data-id="${esc(n.id)}">Rename</button><button class="secondary" data-action="toggle" data-id="${esc(n.id)}">${n.disabled ? "Enable" : "Disable"}</button><button class="secondary square machine-menu-button" data-action="machine-menu" data-id="${esc(n.id)}" aria-label="More actions for ${esc(n.name)}" aria-haspopup="menu" aria-controls="machine-menu" aria-expanded="false">${icon("more")}</button></div>`,
    "machine-drawer",
  );
}
document.addEventListener("submit", async (event) => {
  if (!(event.target instanceof HTMLFormElement) || event.target.id !== "account-form") return;
  event.preventDefault();
  const form = event.target,
    fields = formFields(form),
    button = $("button[type=submit]", form),
    errorBox = $(".form-error", form);
  errorBox.textContent = "";
  if (fields.password !== fields.confirm) {
    errorBox.textContent = "Passwords do not match.";
    return;
  }
  button.disabled = true;
  try {
    await api("console/password", "PUT", {
      currentPassword: fields.currentPassword,
      password: fields.password,
    });
    showLogin();
    toast("Password changed. Sign in with your new password.");
  } catch (error) {
    errorBox.textContent = errorMessage(error);
  } finally {
    button.disabled = false;
  }
});
document.addEventListener("click", async (event) => {
  if (!(event.target instanceof Element)) return;
  const target = event.target;
  const button = target.closest("[data-action]");
  if (button && !(button instanceof HTMLElement)) return;
  // The whole row opens details; its name button remains the keyboard target.
  const row = !button && /** @type {HTMLElement | null} */ (target.closest(".machine-table tbody tr"));
  if (row) machineDetails(row.dataset.id);
  const menuLink = target.closest("[popover] a");
  const popover = menuLink?.closest("[popover]");
  if (popover instanceof HTMLElement) popover.hidePopover();
  if (!button) return;
  const action = button.dataset.action,
    id = button.dataset.id || "",
    name = button.dataset.name || "";
  const node = state.snapshot?.nodes.find((n) => n.id === id);
  try {
    if (action === "machine-menu") {
      if (
        machineMenuTarget?.id === id &&
        $("#machine-menu").matches(":popover-open")
      )
        closeMachineMenu(true);
      else openMachineMenu(button);
      return;
    }
    if (button.closest("#machine-menu")) closeMachineMenu(true);
    if (action === "theme") {
      // The sign-in page has a single toggle instead of the menu's choices.
      themeChoice =
        button.dataset.themeChoice ||
        (document.documentElement.dataset.theme === "light" ? "dark" : "light");
      try {
        if (themeChoice === "system") localStorage.removeItem("control-theme");
        else localStorage.setItem("control-theme", themeChoice);
      } catch {
        /* Optional. */
      }
      applyTheme();
    }
    if (action === "close") closeDialog();
    if (action === "sort") {
      state.descending =
        state.sort === button.dataset.sort
          ? !state.descending
          : !["name", "status"].includes(button.dataset.sort || "");
      state.sort = button.dataset.sort || "name";
      render();
    }
    if (action === "filter-machines") {
      state.search = "";
      state.filter = button.dataset.filter || "all";
    }
    if (action === "filter-state") {
      state.filter = button.dataset.filter || "all";
      render();
    }
    if (action === "clear-filters") {
      state.search = "";
      state.filter = "all";
      $("#machine-search").value = "";
      render();
      $("#machine-search").focus();
    }
    if (action === "refresh") await refresh(true);
    if (action === "audit-more" && button instanceof HTMLButtonElement && state.auxiliary?.page === "activity" && state.auxiliary.data.next) {
      button.disabled = true;
      const older = await api(auditPath(state.auxiliary.data.next));
      if (state.page === "activity" && state.auxiliary?.page === "activity") {
        state.auxiliary = {
          page: "activity",
          data: {events: [...state.auxiliary.data.events, ...older.events], next: older.next},
        };
        render();
      }
    }
    if (action === "logout") {
      $("#account-menu").hidePopover();
      formDialog(
        "Sign out?",
        "<p>You will need your browser password to sign in again. CLI keys and sessions on other browsers are not affected.</p>",
        async () => {
          await api("logout", "POST");
          showLogin();
        },
        "Sign out",
      );
    }
    if (action === "add") addMachine();
    if (action === "copy") {
      try {
        await navigator.clipboard.writeText(
          $(button.dataset.copy || "#secret-value").textContent || "",
        );
        toast("Copied to clipboard");
      } catch {
        toast("Clipboard unavailable. Select and copy the displayed value.");
      }
    }
    if (action === "details") machineDetails(id);
    if (action === "context" && node?.hybridId) {
      state.contexts.set(node.hybridId, node.id);
      render();
      find(`.context-choice[data-id="${CSS.escape(node.id)}"]`)?.focus({ preventScroll: true });
    }
    if (action === "startup" && node && canChangeStartup(node))
      changeStartup(node);
    if (action === "worker-secrets" && node) await workerSecrets(node);
    if (action === "secret-set" && node) await setWorkerSecret(node, name || "");
    if (action === "secret-delete" && node)
      confirmAction(
        "Delete secret from " + node.name + "?",
        "Delete " + name + ". Future operations using this reference will fail until it is defined again.",
        async () => {
          await api(`fleet/nodes/${encodeURIComponent(node.id)}/secrets/${encodeURIComponent(name)}`, "DELETE");
          toast("Secret deleted");
        },
        "Delete secret",
        "machine-drawer secrets-drawer",
      );
    if (action === "wake" && node && button instanceof HTMLButtonElement) {
      button.disabled = true;
      try {
        const result = await api(
          `fleet/nodes/${encodeURIComponent(id)}/wake`,
          "POST",
        );
        toast(
          `Wake packet sent from ${result.source}. ${node.name} appears online once it starts.`,
        );
      } finally {
        button.disabled = false;
      }
    }
    if (action === "rename" && node)
      formDialog(
        "Rename " + node.name,
        `<p>This changes its routing name without restarting the node. Project-local scenario files on CLI hosts are not updated.</p><label>New name<input name="name" value="${esc(node.name)}" required maxlength="63" pattern="[a-zA-Z0-9]([a-zA-Z0-9._]|-){0,62}"></label>`,
        async (fields) => {
          await api(`fleet/nodes/${encodeURIComponent(id)}`, "PATCH", {
            name: fields.name,
            force: fields.force === "on",
          });
          toast("Machine renamed");
        },
      );
    if (action === "toggle" && node)
      confirmAction(
        (node.disabled ? "Enable " : "Disable ") + node.name,
        node.disabled
          ? "Allow new work after the node acknowledges this change."
          : "Block new work. Already accepted tasks can finish.",
        () =>
          api(`fleet/nodes/${encodeURIComponent(id)}`, "PATCH", {
            disabled: !node.disabled,
          }),
        node.disabled ? "Enable machine" : "Disable machine",
      );
    if (action === "unregister" && node) {
      const companion = node.hybridId
        ? (state.snapshot?.nodes || []).find((n) => n.hybridId === node.hybridId && n.id !== node.id)
        : null;
      const companionField = companion
        ? `<p class="notice">${esc(companion.name)} is this host's other Hybrid context${companion.runAs === "system" ? ", running with full system privileges" : ""}. It stays registered and running unless you also remove it.</p><label class="force-confirm"><input name="companion" type="checkbox">Also unregister ${esc(companion.name)}</label>`
        : "";
      formDialog(
        "Unregister " + node.name,
        `<p>This permanently retires this identity. Current installations cancel work and uninstall startup and their unshared launcher when they receive the policy. Local data is retained. Older nodes may need local cleanup.</p>${companionField}<label>Type ${esc(node.name)} to confirm<input name="confirmation" required autocomplete="off"></label>`,
        async (fields) => {
          if (fields.confirmation !== node.name)
            throw new Error("The machine name does not match.");
          await api(
            `fleet/nodes/${encodeURIComponent(id)}?stop=true`,
            "DELETE",
          );
          if (companion && fields.companion) {
            try {
              await api(
                `fleet/nodes/${encodeURIComponent(companion.id)}?stop=true`,
                "DELETE",
              );
            } catch (error) {
              throw new Error(`${node.name} was unregistered, but ${companion.name} was not: ${errorMessage(error)}`);
            }
          }
          toast(companion && fields.companion ? "Both contexts unregistered" : "Machine unregistered");
        },
        "Unregister machine",
        true,
      );
    }
    if (action === "clear-audit") {
      const operator = state.session?.role === "superuser";
      formDialog(
        "Clear activity log",
        `<p>Remove events from ${operator ? "every fleet" : "this fleet"}. A record of the clearing stays in each fleet it affects.</p><label>Remove<select name="keep"><option value="">All events</option><option value="7">Events older than 7 days</option><option value="30">Events older than 30 days</option></select></label>`,
        async (fields) => {
          const days = Number(fields.keep);
          const result = await api(
            `audit${days
                ? "?before=" +
                  encodeURIComponent(
                    new Date(Date.now() - days * 86400000).toISOString(),
                  )
                : ""}`,
            "DELETE",
          );
          state.auxiliary = null;
          toast("Removed " + plural(result.removed, "event"));
        },
        "Clear log",
        true,
      );
    }
    if (action === "clear-invites") {
      const count = clearable(state.auxiliary?.page === "invitations" ? state.auxiliary.data : []).length;
      formDialog(
        "Clear invitation history",
        `<p>Clear ${esc(plural(count, "invitation"))} from this list. Revoked and expired invitations are deleted. Redeemed ones are hidden, and their machines keep working; manage them from Machines. Pending invitations stay.</p>`,
        async () => {
          const result = await api("fleet/installations", "DELETE");
          toast(
            "Cleared " +
              plural(result.removed + (result.hidden || 0), "invitation"),
          );
        },
        "Clear history",
        true,
      );
    }
    if (action === "revoke-invite")
      confirmAction(
        "Revoke invitation",
        "This also revokes an already redeemed installation credential and disconnects its gateway session. Existing direct sessions and accepted work may continue.",
        () => api(`fleet/installations/${encodeURIComponent(id)}`, "DELETE"),
        "Revoke invitation",
      );
    if (action === "create-key")
      formDialog(
        "Create fleet key",
        '<p>Common keys permit enrollment and discovery only. The new secret is shown once.</p><label>Name<input name="name" required maxlength="128"></label>',
        async (fields) => {
          const issued = await api("fleet/keys", "POST", { name: fields.name });
          showSecret(
            "Save your new key",
            issued.token,
            "Store this credential securely. It cannot be retrieved after closing this dialog.",
          );
          return false;
        },
        "Create key",
      );
    if (action === "rename-key")
      formDialog(
        "Rename key",
        `<p>The name only labels this key. Its secret and access are unchanged.</p><label>Name<input name="name" value="${esc(name)}" required maxlength="128"></label>`,
        async (fields) => {
          await api(`fleet/keys/${encodeURIComponent(id)}`, "PATCH", {
            name: fields.name,
          });
          toast("Key renamed");
        },
        "Rename key",
      );
    if (action === "revoke-key")
      confirmAction(
        "Revoke fleet key",
        "New authentication with this key will fail and its gateway sessions will disconnect. Existing direct sessions and accepted work are not cancelled.",
        () => api(`fleet/keys/${encodeURIComponent(id)}`, "DELETE"),
        "Revoke key",
      );
    if (action === "create-user")
      formDialog(
        "Create private fleet",
        '<p>Provision an account with its own isolated fleet. Its CLI account key is shown once. Set a separate browser password from the accounts table.</p><label>Account name<input name="name" required maxlength="63" pattern="[a-zA-Z0-9]([a-zA-Z0-9._]|-){0,62}"></label>',
        async (fields) => {
          const issued = await api("admin/users", "POST", {
            name: fields.name,
          });
          showSecret(
            "Save " + fields.name + "'s account key",
            issued.token,
            "Give this account key only to its owner for CLI login. It is not a browser password.",
          );
          return false;
        },
        "Create account",
      );
    if (action === "disable-user")
      confirmAction(
        "Disable " + name,
        "Disable this account, invalidate browser access, and disconnect its gateway sessions. Accepted work and established direct sessions may continue.",
        () => api(`admin/users/${encodeURIComponent(id)}`, "DELETE"),
        "Disable account",
      );
    if (action === "reset-password")
      formDialog(
        "Browser password for " + name,
        '<p>Setting a password revokes all existing browser sessions for this account. The CLI key stays unchanged.</p><label>New password<input name="password" type="password" autocomplete="new-password" required minlength="12" maxlength="1024"></label><label>Confirm password<input name="confirm" type="password" autocomplete="new-password" required minlength="12" maxlength="1024"></label>',
        async (fields) => {
          if (fields.password !== fields.confirm)
            throw new Error("Passwords do not match.");
          await api("console/password", "PUT", {
            username: name,
            password: fields.password,
          });
          toast("Browser password saved");
        },
        "Set password",
      );
    if (action === "check-release")
      confirmAction(
        "Check for a stable release",
        "A newer verified release can start a gateway-wide managed rollout across every fleet. Nodes wait for idle reservations before restarting.",
        () => api("admin/updates/check", "POST"),
        "Check and apply release",
      );
  } catch (error) {
    toast(errorMessage(error));
  }
});
setInterval(() => {
  if (pendingRefresh && !state.busy) {
    pendingRefresh = false;
    refresh();
  } else refresh();
}, 5000);
document.addEventListener("visibilitychange", () => {
  if (!document.hidden) refresh();
});
(async () => {
  try {
    showApp(await api("session"));
  } catch {
    showLogin();
  }
})();
