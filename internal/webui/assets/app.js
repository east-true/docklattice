"use strict";

const MAX_STATS_SAMPLES = 120;
const MAX_LOG_CHARACTERS = 262144;
const MAX_SSE_BUFFER = 2097152;
const MAX_RENDERED_ROWS = 1000;
const utf8Decoder = new TextDecoder();

const state = {
  dashboard: undefined,
  routeController: undefined,
  streamController: undefined,
  autoRefreshTimer: undefined,
  autoRefreshInterval: 0,
  refreshInFlight: false,
  routeKey: "",
  metricsHistory: [],
  metricsMode: "hierarchy",
  metricsFrame: undefined,
  metricsTopOrder: [],
  summaryWorkloadSnapshots: new Map(),
  inspectorRoute: false,
  inspectorRequest: 0,
  operationToastControllers: new Map(),
  operationsIndex: [],
  loadedLogs: "",
  loadedFile: undefined,
  loadedSource: undefined,
  serviceActionsTrigger: undefined,
  containerActionsTrigger: undefined,
};

const $ = (selector) => document.querySelector(selector);
const view = $("#view");
const escapeHTML = (value) =>
  String(value ?? "").replace(
    /[&<>"']/g,
    (character) =>
      ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[
        character
      ],
  );
const text = (value) => escapeHTML(value);
const shortID = (value) => (value ? String(value).slice(0, 12) : "—");
const formatBytes = (value) => {
  let number = Number(value || 0);
  if (!Number.isFinite(number)) return "—";
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let unit = 0;
  while (Math.abs(number) >= 1024 && unit < units.length - 1) {
    number /= 1024;
    unit += 1;
  }
  return `${number.toFixed(unit ? 1 : 0)} ${units[unit]}`;
};
const formatTime = (value) =>
  value
    ? new Intl.DateTimeFormat(undefined, {
        dateStyle: "medium",
        timeStyle: "medium",
      }).format(new Date(value))
    : "Not observed";
const formatDuration = (nanos) => {
  let seconds = Math.floor(Number(nanos || 0) / 1e9);
  if (!seconds) return "—";
  const days = Math.floor(seconds / 86400);
  seconds %= 86400;
  const hours = Math.floor(seconds / 3600);
  seconds %= 3600;
  const minutes = Math.floor(seconds / 60);
  return (
    [days && `${days}d`, hours && `${hours}h`, minutes && `${minutes}m`]
      .filter(Boolean)
      .join(" ") || "<1m"
  );
};

async function jsonRequest(url, options = {}) {
  const response = await fetch(url, {
    cache: "no-store",
    headers: {
      Accept: "application/json",
      ...(options.body ? { "Content-Type": "application/json" } : {}),
    },
    ...options,
  });
  const value = await response.json().catch(() => ({}));
  if (!response.ok)
    throw new Error(value.message || `Request failed (${response.status})`);
  return value;
}

const TOAST_ICONS = {
  info: "●",
  success: "✓",
  warning: "!",
  error: "×",
};

function dismissToast(toast) {
  toast.dispatchEvent(new CustomEvent("toastdismiss"));
  toast.remove();
}

function updateToast(toast, options) {
  const tone = ["info", "success", "warning", "error"].includes(options.tone)
    ? options.tone
    : "info";
  toast.className = `toast toast-${tone}`;
  toast.dataset.tone = tone;
  toast.setAttribute("role", tone === "error" ? "alert" : "status");
  toast.setAttribute("aria-live", tone === "error" ? "assertive" : "polite");
  toast.setAttribute("aria-atomic", "true");
  toast.replaceChildren();

  const icon = document.createElement("span");
  icon.className = "toast-icon";
  icon.setAttribute("aria-hidden", "true");
  icon.textContent = TOAST_ICONS[tone];

  const content = document.createElement("div");
  content.className = "toast-content";
  const title = document.createElement("p");
  title.className = "toast-title";
  title.textContent = String(options.title || "Notification");
  content.append(title);
  if (options.message) {
    const message = document.createElement("p");
    message.className = "toast-message";
    message.textContent = String(options.message);
    content.append(message);
  }
  if (options.operationID) {
    const action = document.createElement("a");
    action.className = "toast-action";
    action.href = `#/operations?inspect=${encodeURIComponent(options.operationID)}`;
    action.textContent = "View operation";
    action.addEventListener("click", () => dismissToast(toast));
    content.append(action);
  }

  const close = document.createElement("button");
  close.className = "toast-close";
  close.type = "button";
  close.setAttribute(
    "aria-label",
    `Dismiss ${String(options.title || "notification")}`,
  );
  close.textContent = "×";
  close.addEventListener("click", () => dismissToast(toast));

  toast.append(icon, content, close);
  return toast;
}

function showToast(options) {
  const normalized =
    typeof options === "string"
      ? { tone: "info", title: "Notice", message: options }
      : options;
  const toast = document.createElement("div");
  updateToast(toast, normalized);
  $("#toast-region").prepend(toast);
  return toast;
}

function showLoading(
  title = "Loading current state",
  detail = "Reading live facts from the connected Agent.",
) {
  view.innerHTML = `<div class="state-panel"><span class="spinner" aria-hidden="true"></span><h1>${text(title)}</h1><p>${text(detail)}</p></div>`;
}

function showError(error, title = "This view is unavailable") {
  view.innerHTML = `<div class="state-panel"><span class="badge bad">Unavailable</span><h1>${text(title)}</h1><p>${text(error.message || error)}</p><div class="form-actions"><button class="quiet-button" type="button" data-action="go-back">Back</button><button class="primary-button" type="button" data-action="retry-route">Try again</button></div></div>`;
}

function badge(label, tone = "") {
  return `<span class="badge ${tone}">${text(label)}</span>`;
}
function stateBadge(value) {
  const normalized = String(value || "unknown").toLowerCase();
  const tone = ["running", "active", "success", "healthy", "observed"].includes(
    normalized,
  )
    ? "good"
    : ["exited", "failed", "dead", "unhealthy", "rejected"].includes(normalized)
      ? "bad"
      : [
            "paused",
            "restarting",
            "starting",
            "unknown",
            "interrupted",
            "canceled",
          ].includes(normalized)
        ? "warn"
        : "";
  return badge(value || "Unknown", tone);
}
function emptyRow(columns, message) {
  return `<tr><td class="empty-cell" colspan="${columns}">${text(message)}</td></tr>`;
}

const TABLE_WIDTH_STORAGE_PREFIX = "dockpilot.table-widths.v2:";
const TABLE_COLUMN_ABSOLUTE_MIN_WIDTH = 24;
const TABLE_COLUMN_MIN_WIDTH = 72;
const TABLE_COLUMN_MAX_WIDTH = 1200;
const TABLE_COLUMN_KEYBOARD_STEP = 16;
const INSPECTOR_WIDTH_STORAGE_KEY = "dockpilot.inspector-width.v1";
const INSPECTOR_DEFAULT_WIDTH = 520;
const INSPECTOR_MIN_WIDTH = 420;
const INSPECTOR_MAX_VIEWPORT_RATIO = 0.7;
const INSPECTOR_MAIN_MIN_WIDTH = 420;
const INSPECTOR_DESKTOP_SIDEBAR_WIDTH = 252;
const INSPECTOR_PUSH_BREAKPOINT = 1280;
const INSPECTOR_FULL_PAGE_BREAKPOINT = 800;
const INSPECTOR_KEYBOARD_STEP = 16;
const OPERATION_POLL_INTERVAL = 1000;
const OPERATION_POLL_MAX_INTERVAL = 10000;
const OPERATION_POLL_MAX_DURATION = 10 * 60 * 1000;
const AUTO_REFRESH_STORAGE_KEY = "dockpilot.auto-refresh-interval.v1";
const AUTO_REFRESH_INTERVALS = new Set([0, 15000, 30000, 60000, 300000]);
const CAPABILITY_LABELS = {
  connection: "Agent connection",
  docker: "Docker Engine",
  compose: "Docker Compose",
  discovery: "Compose discovery",
  metrics: "Container stats",
  operation_recovery: "Operation recovery",
  fs_read: "File read",
  fs_write: "File write",
};

function tableWidthStorageKey(headers) {
  return `${TABLE_WIDTH_STORAGE_PREFIX}${headers.join("|")}`;
}

function storedTableRatios(storageKey, columnCount) {
  try {
    const ratios = JSON.parse(localStorage.getItem(storageKey) || "null");
    if (
      !Array.isArray(ratios) ||
      ratios.length !== columnCount ||
      ratios.some((ratio) => !Number.isFinite(ratio) || ratio <= 0) ||
      Math.abs(ratios.reduce((total, ratio) => total + ratio, 0) - 100) > 0.1
    ) {
      return [];
    }
    return ratios;
  } catch {
    return [];
  }
}

function table(headers, rows, emptyMessage = "No rows to show") {
  const rendered = rows.slice(0, MAX_RENDERED_ROWS).join("");
  const storageKey = tableWidthStorageKey(headers);
  const ratios = storedTableRatios(storageKey, headers.length);
  const resized = ratios.length === headers.length;
  const columns = headers
    .map((_, index) => {
      const width = resized ? ` style="width: ${ratios[index]}%"` : "";
      return `<col data-column-index="${index}"${width}>`;
    })
    .join("");
  const headingCells = headers
    .map((header, index) => {
      const resizeHandle =
        index < headers.length - 1
          ? `
            <span
              class="column-resize-handle"
              role="separator"
              tabindex="0"
              aria-label="Resize ${text(header)} column"
              aria-orientation="vertical"
              aria-valuemin="${TABLE_COLUMN_ABSOLUTE_MIN_WIDTH}"
              aria-valuemax="${TABLE_COLUMN_MAX_WIDTH}"
              data-column-index="${index}"
              title="Drag or use Left and Right Arrow keys to resize"
            ></span>
          `
          : "";
      return `
        <th scope="col" aria-label="${text(header)}">
          <span class="column-heading-label">${text(header)}</span>
          ${resizeHandle}
        </th>
      `;
    })
    .join("");

  return `
    <div class="table-wrap">
      <table
        class="resizable-table"
        data-table-width-key="${text(storageKey)}"
        ${resized ? 'data-columns-resized="true"' : ""}
      >
        <colgroup>${columns}</colgroup>
        <thead><tr>${headingCells}</tr></thead>
        <tbody>${rendered || emptyRow(headers.length, emptyMessage)}</tbody>
      </table>
    </div>
  `;
}

function capabilityButton(
  label,
  capability,
  attributes = "",
  className = "quiet-button",
) {
  const button = document.createElement("button");
  button.className = className;
  button.type = "button";
  button.textContent = label;
  const value = capability;
  const reason = value.reason || "";
  const warning = value.enabled && reason;
  button.disabled = !value.enabled;
  button.title = capability.reason || (warning ? "Available with warning" : "");
  for (const [key, value] of Object.entries(attributes))
    button.dataset[key] = value;
  return button;
}

function hostByID(id) {
  return state.dashboard?.hosts?.find((host) => host.id === id);
}
function projectByUID(uid) {
  return state.dashboard?.projects?.find((project) => project.uid === uid);
}
function hostForProject(project) {
  return hostByID(project?.agent_id);
}
function connectionAvailable(host) {
  return Boolean(host?.capabilities?.connection?.enabled);
}

function agentConnectionStatus(host) {
  return connectionAvailable(host) ? "Connected" : "Offline";
}

function updateIndexedHost(host) {
  const hosts = state.dashboard?.hosts;
  const index =
    hosts?.findIndex((candidate) => candidate.id === host?.id) ?? -1;
  if (index < 0) return;

  hosts[index] = {
    ...hosts[index],
    ...host,
  };
  renderSidebar();
}

function parsedRoute() {
  const raw = (location.hash || "#/home").replace(/^#\/?/, "");
  const [path, query = ""] = raw.split("?", 2);
  const parts = path
    .split("/")
    .filter(Boolean)
    .map((part) => decodeURIComponent(part));
  if (!parts.length) return { kind: "home", key: "home" };
  const params = new URLSearchParams(query);
  if (parts[0] === "search")
    return {
      kind: "search",
      query: params.get("q") || "",
      key: `search?${query}`,
    };
  if (parts[0] === "hosts" && parts[1])
    return {
      kind: "host",
      id: parts[1],
      tab: parts[2] || "summary",
      inspect: params.get("inspect") || "",
      key: `${parts.join("/")}?${query}`,
    };
  if (parts[0] === "projects" && parts[1])
    return {
      kind: "project",
      id: parts[1],
      tab: parts[2] || "summary",
      inspect: params.get("inspect") || "",
      key: `${parts.join("/")}?${query}`,
    };
  if (parts[0] === "operations")
    return {
      kind: "operations",
      inspect: params.get("inspect") || "",
      key: `operations?${query}`,
    };
  return { kind: "home", key: "home" };
}

function storedAutoRefreshInterval() {
  try {
    const interval = Number(localStorage.getItem(AUTO_REFRESH_STORAGE_KEY));
    return AUTO_REFRESH_INTERVALS.has(interval) ? interval : 0;
  } catch {
    return 0;
  }
}

function autoRefreshPauseReason() {
  if (document.hidden) return "Paused while this browser tab is hidden";
  if (document.querySelector("dialog[open]")) {
    return "Paused while a confirmation dialog is open";
  }

  const route = parsedRoute();
  if (route.kind === "host" && route.tab === "metrics") {
    return "Container stats already updates as a live stream";
  }
  if (route.kind === "host" && route.tab === "audit") {
    return "Paused while using Audit filters";
  }
  if (route.kind === "project" && route.tab === "logs") {
    return "Log output already updates as a live stream";
  }
  if (route.kind === "project" && route.tab === "files") {
    return "Paused to protect unsaved file edits";
  }
  if (route.kind === "project" && route.tab === "activity") {
    return "Paused while using Activity filters";
  }
  if (route.kind === "search") {
    return "Paused while entering a search query";
  }
  return "";
}

function clearAutoRefreshTimer() {
  if (state.autoRefreshTimer === undefined) return;
  window.clearTimeout(state.autoRefreshTimer);
  state.autoRefreshTimer = undefined;
}

function updateAutoRefreshControl() {
  const select = $("#refresh-interval");
  const status = $("#refresh-interval-state");
  const pauseReason = state.autoRefreshInterval ? autoRefreshPauseReason() : "";

  select.value = String(state.autoRefreshInterval);
  select.title = pauseReason || "Automatically refresh the current view";
  status.hidden = !pauseReason;
  status.textContent = pauseReason ? "Paused" : "";
  status.title = pauseReason;
}

function scheduleAutoRefresh() {
  clearAutoRefreshTimer();
  updateAutoRefreshControl();
  if (
    !state.autoRefreshInterval ||
    state.refreshInFlight ||
    autoRefreshPauseReason()
  ) {
    return;
  }

  state.autoRefreshTimer = window.setTimeout(() => {
    state.autoRefreshTimer = undefined;
    if (autoRefreshPauseReason()) {
      scheduleAutoRefresh();
      return;
    }
    void refreshDockpilot();
  }, state.autoRefreshInterval);
}

function renderSidebar() {
  const hosts = state.dashboard?.hosts || [];
  const projects = state.dashboard?.projects || [];
  const route = parsedRoute();
  $("#host-tree").innerHTML =
    hosts
      .map((host) => {
        const available = connectionAvailable(host);
        return `<div class="host-group">
      <a class="host-link ${route.kind === "host" && route.id === host.id ? "active" : ""}" href="#/hosts/${encodeURIComponent(host.id)}/summary" title="${text(host.capabilities?.connection?.reason || "")}">
        <span class="host-dot ${available ? "online" : "offline"}" aria-hidden="true"></span><span>${text(host.display_name || host.id)}</span>
      </a>
    </div>`;
      })
      .join("") || `<p class="nav-empty">No Docker hosts registered.</p>`;
  document
    .querySelectorAll("[data-route]")
    .forEach((node) =>
      node.classList.toggle("active", node.dataset.route === route.kind),
    );
  $("#sidebar-summary").textContent =
    `${hosts.filter(connectionAvailable).length}/${hosts.length} connected · ${projects.length} projects`;
}

function breadcrumbs(items) {
  $("#breadcrumbs").innerHTML = items
    .map((item, index) => {
      const value = item.href
        ? `<a href="${item.href}">${text(item.label)}</a>`
        : `<span>${text(item.label)}</span>`;
      return `${index ? `<span class="crumb-separator" aria-hidden="true">/</span>` : ""}${value}`;
    })
    .join("");
}

function pageHeader(kicker, title, subtitle, actions = "") {
  return `<header class="page-header"><div><p class="eyebrow">${text(kicker)}</p><h1>${text(title)}</h1><p>${text(subtitle)}</p></div><div class="page-actions">${actions}</div></header>`;
}
function tabs(scope, id, active, entries) {
  return `<nav class="tabs" aria-label="${text(scope)} sections">${entries.map(([key, label]) => `<a class="tab ${active === key ? "active" : ""}" href="#/${scope}/${encodeURIComponent(id)}/${key}">${text(label)}</a>`).join("")}</nav>`;
}
const hostTabs = (id, active) =>
  tabs("hosts", id, active, [
    ["summary", "Summary"],
    ["compose", "Compose"],
    ["containers", "Containers"],
    ["images", "Images"],
    ["networks", "Networks"],
    ["volumes", "Volumes"],
    ["metrics", "Container stats"],
    ["audit", "Audit"],
  ]);
const projectTabs = (id, active) =>
  tabs("projects", id, active, [
    ["summary", "Summary"],
    ["services", "Services"],
    ["containers", "Containers"],
    ["files", "Files"],
    ["logs", "Logs"],
    ["backups", "Backups"],
    ["activity", "Activity"],
  ]);

async function loadDashboard(force = false) {
  if (!force && state.dashboard) return state.dashboard;
  const dashboard = await jsonRequest("/api/v1/dashboard");
  state.dashboard = dashboard;
  renderSidebar();
  jsonRequest("/api/v1/operations?limit=200")
    .then((page) => {
      const active = (page.operations || []).filter(
        (operation) => !operationTerminal(operation),
      );
      $("#operation-count").textContent = String(active.length);
    })
    .catch(() => {
      $("#operation-count").textContent = "?";
    });
  return dashboard;
}

function operationTerminal(operation) {
  return ["success", "failed", "canceled", "interrupted", "rejected"].includes(
    String(operation.status || "").toLowerCase(),
  );
}

async function renderHome(signal) {
  breadcrumbs([{ label: "Home" }]);
  const dashboard = await loadDashboard();
  const hosts = dashboard.hosts || [];
  const projects = dashboard.projects || [];
  const auditCoverage = await Promise.all(
    hosts.map((host) =>
      jsonRequest(
        `/api/v1/hosts/${encodeURIComponent(host.id)}/audit?limit=1`,
        { signal },
      )
        .then((page) => ({ host, coverage: page.coverage }))
        .catch((error) => ({ host, error })),
    ),
  );
  const dockerUnavailable = hosts.filter(
    (host) => !host.capabilities?.docker?.enabled,
  ).length;
  const composeUnavailable = hosts.filter(
    (host) => !host.capabilities?.compose?.enabled,
  ).length;
  const discoveryIncomplete = hosts.filter(
    (host) =>
      !host.capabilities?.discovery?.enabled || host.project_scan?.truncated,
  ).length;
  const attention = [];
  for (const host of hosts) {
    if (!connectionAvailable(host))
      attention.push({
        host,
        copy: host.capabilities?.connection?.reason || "Agent disconnected",
      });
    if (connectionAvailable(host) && !host.capabilities?.docker?.enabled)
      attention.push({
        host,
        copy: host.capabilities?.docker?.reason || "Docker Engine unavailable",
      });
    if (connectionAvailable(host) && !host.capabilities?.compose?.enabled)
      attention.push({
        host,
        copy:
          host.capabilities?.compose?.reason || "Docker Compose unavailable",
      });
    if (host.project_scan?.truncated || !host.capabilities?.discovery?.enabled)
      attention.push({
        host,
        copy:
          host.capabilities?.discovery?.reason ||
          host.project_scan?.stop_reason ||
          "Discovery incomplete",
      });
  }
  for (const result of auditCoverage) {
    if (result.error)
      attention.push({
        host: result.host,
        copy: "Audit coverage could not be checked",
      });
    else if ((result.coverage?.gaps || []).length)
      attention.push({
        host: result.host,
        copy: `Confirmed Audit gap (${result.coverage.gaps.length})`,
      });
    else if (
      !result.coverage?.established ||
      (result.coverage?.unknown_incarnations || []).length
    )
      attention.push({
        host: result.host,
        copy: "Audit continuity is uncertain",
      });
  }
  for (const project of projects) {
    const host = hostByID(project.agent_id);
    const copy = project.restore_recovery_required
      ? "Restore recovery required"
      : project.collision
        ? "Project name collision"
        : project.stale
          ? "Project state is stale"
          : !project.present
            ? "Project is missing"
            : project.drift === "changed"
              ? "Configuration changed"
              : project.drift === "no-baseline"
                ? "No configuration baseline"
                : "";
    if (copy) attention.push({ host, project, copy });
  }
  const rows = hosts.map((host) => {
    const states = [
      !host.capabilities?.docker?.enabled && "docker",
      !host.capabilities?.compose?.enabled && "compose",
      (host.project_scan?.truncated ||
        !host.capabilities?.discovery?.enabled) &&
        "discovery",
    ]
      .filter(Boolean)
      .join(" ");
    return `<tr data-home-state="${states}"><td><a class="primary" href="#/hosts/${encodeURIComponent(host.id)}/summary">${text(host.display_name || host.id)}</a><div class="secondary mono">${text(host.id)}</div></td><td>${connectionAvailable(host) ? "Connected" : "Unavailable"}<div class="secondary">${text(host.capabilities?.connection?.reason || "")}</div></td><td>${host.capabilities?.docker?.enabled ? "Available" : `Unavailable · ${text(host.capabilities?.docker?.reason || "")}`}</td><td>${host.capabilities?.compose?.enabled ? "Available" : `Unavailable · ${text(host.capabilities?.compose?.reason || "")}`}</td><td>${host.capabilities?.discovery?.enabled && !host.project_scan?.truncated ? `Completed · ${text(formatTime(host.project_scan?.scanned_at))}` : `Incomplete · ${text(host.capabilities?.discovery?.reason || host.project_scan?.stop_reason || "")}`}</td></tr>`;
  });
  view.innerHTML = `${pageHeader("Fleet", "Home", "Compare Docker hosts, inspect deterministic exceptions, and route to real host or Compose project context.")}<input id="home-search" class="search-field" type="search" placeholder="Search Docker hosts and Compose projects" aria-label="Search Docker hosts and Compose projects"><div class="compact-filters" aria-label="Docker host state filters"><button class="quiet-button compact-filter active" data-action="home-filter" data-filter="all">All hosts ${hosts.length}</button><button class="quiet-button compact-filter" data-action="home-filter" data-filter="docker">Docker Engine unavailable ${dockerUnavailable}</button><button class="quiet-button compact-filter" data-action="home-filter" data-filter="compose">Docker Compose unavailable ${composeUnavailable}</button><button class="quiet-button compact-filter" data-action="home-filter" data-filter="discovery">Discovery incomplete ${discoveryIncomplete}</button></div><section class="panel"><div class="panel-header"><div><h2>Needs attention</h2><p>Deterministic Dockpilot-known exceptions only.</p></div></div><ul class="attention-list">${attention.map((item) => `<li class="attention-item"><a class="primary attention-target" href="${item.project ? `#/projects/${encodeURIComponent(item.project.uid)}/summary` : `#/hosts/${encodeURIComponent(item.host.id)}/summary`}">${text(item.project?.name || item.host?.display_name || item.host?.id)}</a><span class="attention-reason" title="${text(item.copy)}">${text(item.copy)}</span></li>`).join("") || `<li class="muted">No deterministic exceptions reported.</li>`}</ul></section><section class="panel flush"><div class="panel-header inset"><div><h2>Docker hosts</h2><p>No registered host disappears when a live probe fails.</p></div></div>${table(["Docker host", "Agent", "Docker Engine", "Docker Compose", "Discovery"], rows, "No Docker hosts registered")}</section>`;
  $("#home-search").addEventListener("input", (event) => {
    const value = event.target.value.trim();
    if (value) location.hash = `#/search?q=${encodeURIComponent(value)}`;
  });
}

function renderSearch() {
  breadcrumbs([{ label: "Home", href: "#/home" }, { label: "Search" }]);
  const route = parsedRoute();
  view.innerHTML = `${pageHeader("Dockpilot index", "Search", "Immediate search covers registered Docker hosts and discovered Compose projects. It does not claim a global Container index.")}<input id="global-search" class="search-field" type="search" value="${text(route.query)}" placeholder="Host name, Agent ID, project, UID, or working directory" autofocus><div id="search-results"></div>`;
  const input = $("#global-search");
  const update = () => {
    const query = input.value.trim().toLowerCase();
    const hosts = (state.dashboard?.hosts || [])
      .filter(
        (host) =>
          !query ||
          [host.display_name, host.id].some((value) =>
            String(value || "")
              .toLowerCase()
              .includes(query),
          ),
      )
      .map(
        (host) =>
          `<tr><td><a class="primary" href="#/hosts/${encodeURIComponent(host.id)}/summary">${text(host.display_name || host.id)}</a><div class="secondary mono">${text(host.id)}</div></td><td>Docker host</td><td>${connectionAvailable(host) ? "Connected" : text(host.capabilities?.connection?.reason || "Unavailable")}</td></tr>`,
      );
    const projects = (state.dashboard?.projects || [])
      .filter(
        (project) =>
          !query ||
          [project.name, project.uid, project.working_dir].some((value) =>
            String(value || "")
              .toLowerCase()
              .includes(query),
          ),
      )
      .map(
        (project) =>
          `<tr><td><a class="primary" href="#/projects/${encodeURIComponent(project.uid)}/summary">${text(project.name)}</a><div class="secondary mono">${text(project.working_dir)}</div></td><td>Compose project</td><td>${text(hostByID(project.agent_id)?.display_name || project.agent_id)}</td></tr>`,
      );
    $("#search-results").innerHTML = table(
      ["Result", "Type", "Context"],
      [...hosts, ...projects],
      query
        ? "No indexed host or Compose project matches"
        : "No indexed objects",
    );
    restoreStoredTableWidths($("#search-results"));
  };
  input.addEventListener("input", update);
  update();
  input.focus();
}

function operationTable(operations, context = true) {
  return table(
    context
      ? ["Operation", "Context", "Status", "Phase", "Requested", "Cancel"]
      : ["Operation", "Status", "Requested"],
    operations.map((operation) => {
      const lead = `<td><button class="row-button" data-action="inspect-operation" data-operation="${text(operation.operation_id)}"><span class="primary">${text(operation.kind || operation.operation_id)}</span><div class="secondary mono">${text(operation.operation_id)}</div></button></td>`;
      if (!context)
        return `<tr>${lead}<td>${stateBadge(operation.status)}</td><td>${text(formatTime(operation.requested_at))}</td></tr>`;
      const host = hostByID(operation.agent_id);
      const project = projectByUID(operation.project_uid);
      const service = composeServiceTarget(operation);
      const projectName = project?.name || operation.project_uid;
      const contextName =
        service || projectName || host?.display_name || operation.agent_id;
      const contextDetail = service
        ? projectName
          ? `Compose project: ${projectName}`
          : "Service-scoped Compose operation"
        : operation.target
          ? `Target: ${operation.target}`
          : "";
      const reachable = connectionAvailable(host);
      const canCancel = operation.can_cancel && reachable;
      const cancelReason = reachable
        ? operation.cancelability_reason
        : host?.capabilities?.connection?.reason || "Agent is unavailable";
      return `<tr>${lead}<td>${text(contextName)}${contextDetail ? `<div class="secondary">${text(contextDetail)}</div>` : ""}</td><td>${stateBadge(operation.status)}</td><td>${text(operation.phase || "—")}</td><td>${text(formatTime(operation.requested_at))}</td><td>${canCancel ? `<button class="quiet-button" data-action="cancel-operation" data-agent="${text(operation.agent_id)}" data-operation="${text(operation.operation_id)}">Cancel</button>` : `<span class="secondary" title="${text(cancelReason || "Not cancelable")}">Unavailable</span>`}</td></tr>`;
    }),
    "No operations recorded",
  );
}

function engineStat(summary, key, format = (value) => value ?? "—") {
  return summary ? format(summary[key]) : "—";
}

async function renderHost(route, signal) {
  const host = hostByID(route.id);
  if (!host) throw new Error(`Host ${route.id} is not in the Server index.`);
  breadcrumbs([
    { label: "Home", href: "#/home" },
    {
      label: host.display_name || host.id,
      href: `#/hosts/${encodeURIComponent(host.id)}/summary`,
    },
    { label: route.tab },
  ]);
  const shell = (actions = "", currentHost = host) =>
    `${pageHeader(
      "Docker host",
      currentHost.display_name || currentHost.id,
      `Agent ${currentHost.id} · ${agentConnectionStatus(currentHost)}`,
      actions,
    )}${hostTabs(host.id, route.tab)}`;
  if (route.tab === "summary") {
    const detail = await jsonRequest(
      `/api/v1/hosts/${encodeURIComponent(host.id)}`,
      { signal },
    );
    updateIndexedHost(detail);
    const engine = detail.engine_summary;
    const unavailable = !engine
      ? `<div class="notice warning">
          <span class="primary">Agent connection: ${text(agentConnectionStatus(detail))}</span>
          <div class="secondary">
            ${text(
              connectionAvailable(detail)
                ? detail.engine_summary_reason ||
                    "Docker Engine summary is unavailable."
                : "Current Docker Engine data is unavailable until the Agent reconnects.",
            )}
          </div>
        </div>`
      : "";
    const stopped = engine
      ? Math.max(
          0,
          Number(engine.containers_total || 0) -
            Number(engine.containers_running || 0),
        )
      : "—";
    const exceptions = (state.dashboard?.projects || []).filter(
      (project) =>
        project.agent_id === host.id &&
        (project.restore_recovery_required ||
          project.collision ||
          project.stale ||
          !project.present ||
          (project.drift && project.drift !== "in-sync")),
    );
    const usageAvailable = Boolean(host.capabilities?.metrics?.enabled);
    const cpuCapacity = engine?.cpu_capacity;
    const memoryCapacity = engine?.memory_capacity_bytes;
    const cachedWorkload = usageAvailable
      ? state.summaryWorkloadSnapshots.get(host.id)
      : undefined;
    const cachedUsage = cachedWorkload
      ? summaryWorkloadValues(cachedWorkload, engine)
      : undefined;
    const engineOverview = definitionList(
      {
        "Engine version": engine?.version,
        Containers: engine
          ? `${engine.containers_total} total · ${engine.containers_running} running · ${stopped} stopped`
          : "—",
        Images: engine?.images,
        "CPU used / total": engine
          ? cachedUsage?.cpu ||
            `${usageAvailable ? "Loading" : "Unavailable"} / ${cpuCapacity} logical CPUs`
          : "—",
        "Memory used / total": engine
          ? cachedUsage?.memory ||
            `${usageAvailable ? "Loading" : "Unavailable"} / ${formatBytes(memoryCapacity)}`
          : "—",
        "Stats observed":
          cachedUsage?.observed || (usageAvailable ? "Loading" : "Unavailable"),
        "Storage driver": engine?.storage_driver,
      },
      {
        "CPU used / total": "engine-cpu-usage",
        "Memory used / total": "engine-memory-usage",
        "Stats observed": "engine-usage-observed",
      },
    );
    const engineTechnicalDetails = definitionList({
      "Engine API version": detail.docker_api_version,
      "Docker Compose version": detail.docker_compose_version,
      "Logging driver": engine?.logging_driver,
      "Cgroup driver / version": engine
        ? [engine.cgroup_driver, engine.cgroup_version]
            .filter(Boolean)
            .join(" · ")
        : "",
      "Default runtime": engine?.default_runtime,
      "Operating system": engine
        ? [engine.operating_system, engine.os_version]
            .filter(Boolean)
            .join(" · ")
        : "",
      "OS type": engine?.os_type,
      Architecture: engine?.architecture,
      Kernel: engine?.kernel_version,
      "Docker root directory": engine?.docker_root_dir,
    });
    const managementCapabilities = Object.entries(detail.capabilities || {})
      .map(
        ([name, cap]) => `
          <div class="capability-row">
            <span class="primary">${text(CAPABILITY_LABELS[name] || name)}</span> ·
            ${cap.enabled ? "Available" : "Unavailable"}
            <div class="secondary">${text(cap.reason || "")}</div>
          </div>
        `,
      )
      .join("");
    const managementFacts = definitionList({
      "Session source IP": detail.session_source_ip,
      "Session observed": formatTime(detail.session_observed_at),
      "Compose discovery": formatTime(detail.project_scan?.scanned_at),
    });

    view.innerHTML = `
      ${shell("", detail)}
      ${unavailable}
      <section class="panel host-summary-panel">
        <div class="panel-header">
          <div>
            <h2>Host</h2>
            <p>Current Agent session, discovery, and Dockpilot capability state.</p>
          </div>
        </div>
        ${managementFacts}
        <section class="management-capabilities" aria-labelledby="capabilities-heading">
          <h3 id="capabilities-heading">Capabilities</h3>
          <div class="capability-grid">
              ${managementCapabilities}
          </div>
        </section>
      </section>
      <section class="panel engine-summary-panel">
        <div class="panel-header">
          <div>
            <h2>Docker Engine</h2>
            <p>
              Resource usage by running Containers against Engine-reported
              capacity; Host processes outside Docker are excluded.
            </p>
          </div>
        </div>
        ${engineOverview}
        <section class="engine-technical-section" aria-labelledby="engine-technical-heading">
          <h3 id="engine-technical-heading">Engine technical details</h3>
          <p class="secondary">
            Engine-reported technical configuration from the same one-shot
            inspection.
          </p>
          ${engineTechnicalDetails}
        </section>
      </section>
      <section class="panel flush">
        <div class="panel-header inset">
          <div>
            <h2>Compose projects</h2>
            <p>Only projects with deterministic known exceptions are shown.</p>
          </div>
          <a href="#/hosts/${encodeURIComponent(host.id)}/compose">
            Open Compose
          </a>
        </div>
        ${table(
          ["Project", "Dockpilot condition", "Config drift"],
          exceptions.map(
            (project) =>
              `<tr><td><a class="primary" href="#/projects/${encodeURIComponent(project.uid)}/summary">${text(project.name)}</a><div class="secondary mono">${text(project.working_dir)}</div></td><td>${projectCondition(project)}</td><td>${text(composeConfigState(project.drift))}</td></tr>`,
          ),
          "No Compose project exceptions",
        )}
      </section>
    `;
    if (engine && usageAvailable) {
      loadSummaryWorkloadSnapshot(host, engine, signal);
    }
    return;
  }
  if (route.tab === "compose") {
    const projects = (state.dashboard?.projects || []).filter(
      (project) => project.agent_id === host.id,
    );
    const rows = projects.map((project) => {
      const attachmentsCurrent =
        project.present && !project.stale && project.last_observed_at;
      const containers = attachmentsCurrent
        ? String((project.container_ids || []).length)
        : "Unavailable";
      const lastObserved = project.last_observed_at
        ? `<time datetime="${text(project.last_observed_at)}">${text(formatTime(project.last_observed_at))}</time>`
        : "Never";
      const attention =
        [
          project.restore_recovery_required && "Recovery required",
          project.collision && "Collision",
          project.stale && "Stale",
          !project.present && "Missing",
          project.drift &&
            project.drift !== "in-sync" &&
            composeConfigState(project.drift),
        ]
          .filter(Boolean)
          .join(" · ") || "—";
      return `
        <tr>
          <td>
            <a class="primary" href="#/projects/${encodeURIComponent(project.uid)}/summary">
              ${text(project.name)}
            </a>
            <div class="secondary mono">${text(project.working_dir)}</div>
          </td>
          <td>${text((project.defined_services || []).length)}</td>
          <td>${text(containers)}</td>
          <td>${lastObserved}</td>
          <td>${text(composeConfigState(project.drift))}</td>
          <td>${text(attention)}</td>
        </tr>
      `;
    });
    view.innerHTML = `
      ${shell(
        `<button class="primary-button" data-action="project-operation" data-kind="discovery.rescan" data-agent="${text(host.id)}">Rescan Compose projects</button>`,
      )}
      <section class="panel flush">
        <div class="panel-header inset">
          <div>
            <h2>Compose projects</h2>
            <p>Compose projects discovered on this Docker host.</p>
          </div>
        </div>
        ${table(
          [
            "Project",
            "Services",
            "Containers",
            "Last observed",
            "Config drift",
            "Needs attention",
          ],
          rows,
          "No Compose projects discovered on this Docker host",
        )}
      </section>
    `;
    return;
  }
  if (["containers", "images", "networks", "volumes"].includes(route.tab)) {
    if (!connectionAvailable(host)) {
      view.innerHTML = `${shell()}<div class="notice warning"><span class="primary">Agent connection: ${text(agentConnectionStatus(host))}</span><div class="secondary">Current Docker data is unavailable until the Agent reconnects.</div></div>`;
      return;
    }
    const items = await jsonRequest(
      `/api/v1/hosts/${encodeURIComponent(host.id)}/${route.tab}`,
      { signal },
    );
    view.innerHTML = shell() + renderInventory(route.tab, items, host.id);
    if (route.inspect) {
      state.inspectorRoute = true;
      if (route.tab === "containers") inspectContainer(host.id, route.inspect);
      if (route.tab === "images") inspectImage(host.id, route.inspect);
      if (route.tab === "networks") inspectNetwork(host.id, route.inspect);
      if (route.tab === "volumes") inspectVolume(host.id, route.inspect);
    }
    return;
  }
  if (route.tab === "metrics") {
    view.innerHTML = shell();
    renderMetrics(host);
    return;
  }
  if (route.tab === "audit") {
    view.innerHTML = shell() + auditScaffold("host", host.id);
    bindAudit("host", host.id);
    return;
  }
  throw new Error("Unknown host section.");
}

function portsCell(ports) {
  return `<div class="ports">${(ports || []).map((port) => badge(`${port.host_ip ? `${port.host_ip}:` : ""}${port.published_port} → ${port.target_port}/${port.protocol}`)).join("") || "—"}</div>`;
}
function containerActionsButton(agentID, projectUID, container) {
  const name = (container.names || []).join(", ") || shortID(container.id);
  return `
    <button
      class="row-menu-trigger"
      type="button"
      data-action="open-container-actions"
      data-agent="${text(agentID)}"
      data-project="${text(projectUID || "")}"
      data-container="${text(container.id)}"
      data-container-name="${text(name)}"
      data-container-state="${text(container.state || "unknown")}"
      data-protected="${container.protected ? "true" : "false"}"
      data-protection-reason="${text(container.protection_reason || "")}"
      aria-label="Actions for ${text(name)}"
      aria-controls="container-actions-menu"
      aria-expanded="false"
      aria-haspopup="menu"
      title="Container actions"
    >…</button>
  `;
}
function composeContainerRole(container) {
  if (container.orphan) return badge("Orphan", "warn");
  if (container.one_off) return badge("One-off", "info");
  return "Service";
}
function renderInventory(kind, items, agentID) {
  const headers = {
    containers: [
      "Compose project",
      "Service",
      "Compose role",
      "Container",
      "State",
      "Health",
      "Image",
      "Published ports",
      "Protection",
      "",
    ],
    images: [
      "Repository / tags",
      "Image ID",
      "Created",
      "Size",
      "Container references",
    ],
    networks: ["Network", "Driver", "Scope", "Flags"],
    volumes: ["Volume", "Driver", "Scope", "Created"],
  }[kind];
  const rows = (items || []).map((item) => {
    if (kind === "containers")
      return `<tr><td>${text(item.compose_project || "—")}</td><td>${text(item.compose_service || "—")}</td><td>${composeContainerRole(item)}</td><td><a class="row-button" href="#/hosts/${encodeURIComponent(agentID)}/containers?inspect=${encodeURIComponent(item.id)}"><span class="primary">${text((item.names || []).join(", ") || shortID(item.id))}</span><div class="secondary mono">${text(shortID(item.id))}</div></a></td><td>${stateBadge(item.state)}</td><td>${item.health ? stateBadge(item.health) : "—"}</td><td>${text(item.image || "—")}</td><td>${portsCell(item.ports)}</td><td>${item.protected ? `${badge("Protected", "warn")}<div class="secondary">${text(item.protection_reason)}</div>` : "—"}</td><td>${containerActionsButton(agentID, "", item)}</td></tr>`;
    if (kind === "images")
      return `<tr><td><a class="row-button" href="#/hosts/${encodeURIComponent(agentID)}/images?inspect=${encodeURIComponent(item.id)}"><span class="primary">${text((item.repo_tags || []).join(", ") || "Untagged")}</span><div class="secondary">${text((item.repo_digests || []).join(", ") || "No digest references")}</div></a></td><td class="mono">${text(shortID(item.id))}</td><td>${text(item.created_unix ? formatTime(item.created_unix * 1000) : "—")}</td><td>${text(formatBytes(item.size_bytes))}</td><td title="${text(item.containers < 0 ? "Docker did not calculate the Container reference count" : "Running and stopped Containers that reference this Image")}">${item.containers < 0 ? "Unavailable" : text(item.containers)}</td></tr>`;
    if (kind === "networks")
      return `<tr><td><a class="row-button" href="#/hosts/${encodeURIComponent(agentID)}/networks?inspect=${encodeURIComponent(item.id)}"><span class="primary">${text(item.name)}</span><div class="secondary mono">${text(shortID(item.id))}</div></a></td><td>${text(item.driver)}</td><td>${text(item.scope)}</td><td>${[item.internal && badge("Internal"), item.attachable && badge("Attachable"), item.ingress && badge("Ingress")].filter(Boolean).join(" ") || "—"}</td></tr>`;
    return `<tr><td><a class="row-button" href="#/hosts/${encodeURIComponent(agentID)}/volumes?inspect=${encodeURIComponent(item.name)}"><span class="primary">${text(item.name)}</span></a></td><td>${text(item.driver)}</td><td>${text(item.scope)}</td><td>${text(item.created_at || "—")}</td></tr>`;
  });
  return `<section class="panel flush"><div class="panel-header inset"><div><h2>${text(kind[0].toUpperCase() + kind.slice(1))}</h2><p>${items.length} current Docker objects. This list is not stored by the Server.</p></div></div>${table(headers, rows, `No ${kind} reported by Docker Engine`)}</section>`;
}

function definitionList(values, valueIDs = {}) {
  return `<dl class="definition-list">${Object.entries(values)
    .map(([key, value]) => {
      const display =
        value === false
          ? "No"
          : value === true
            ? "Yes"
            : value === undefined || value === null || value === ""
              ? "—"
              : value;
      const valueID = valueIDs[key] ? ` id="${text(valueIDs[key])}"` : "";
      return `<div class="definition-row"><dt class="muted">${text(key)}</dt><dd${valueID}>${text(display)}</dd></div>`;
    })
    .join("")}</dl>`;
}

function summaryWorkloadValues(frame, engine) {
  const totals = frame.host?.totals || {};
  const pending = Number(totals.pending_count || 0);
  const qualifier = pending > 0 ? "Partial · " : "";
  const cpuCapacity = Number(
    frame.host?.cpu_capacity || engine.cpu_capacity || 0,
  );
  const memoryCapacity = Number(
    frame.host?.memory_capacity || engine.memory_capacity_bytes || 0,
  );
  const usedCPUs = Number(totals.cpu_percent || 0) / 100;
  const usedMemory = Number(totals.memory_usage || 0);
  const memoryPercent = memoryCapacity
    ? ` (${((usedMemory / memoryCapacity) * 100).toFixed(1)}%)`
    : "";

  return {
    cpu: `${qualifier}${usedCPUs.toFixed(2)} / ${cpuCapacity} logical CPUs`,
    memory: `${qualifier}${formatBytes(usedMemory)} / ${formatBytes(memoryCapacity)}${memoryPercent}`,
    observed: formatTime(frame.observed_at),
  };
}

function renderSummaryWorkloadSnapshot(frame, engine) {
  const cpuUsage = $("#engine-cpu-usage");
  const memoryUsage = $("#engine-memory-usage");
  const observed = $("#engine-usage-observed");
  if (!cpuUsage || !memoryUsage || !observed) return;

  const values = summaryWorkloadValues(frame, engine);

  cpuUsage.textContent = values.cpu;
  memoryUsage.textContent = values.memory;
  observed.textContent = values.observed;
}

function markSummaryWorkloadUnavailable(engine) {
  const cpuUsage = $("#engine-cpu-usage");
  const memoryUsage = $("#engine-memory-usage");
  const observed = $("#engine-usage-observed");
  if (cpuUsage) {
    cpuUsage.textContent = `Unavailable / ${engine.cpu_capacity} logical CPUs`;
  }
  if (memoryUsage) {
    memoryUsage.textContent = `Unavailable / ${formatBytes(engine.memory_capacity_bytes)}`;
  }
  if (observed) observed.textContent = "Unavailable";
}

function loadSummaryWorkloadSnapshot(host, engine, routeSignal) {
  state.streamController?.abort();
  const controller = new AbortController();
  state.streamController = controller;
  let receivedFrame = false;
  routeSignal.addEventListener("abort", () => controller.abort(), {
    once: true,
  });
  const timeout = window.setTimeout(() => controller.abort(), 8000);

  streamSSE(
    `/api/v1/live/matrix?agent_id=${encodeURIComponent(host.id)}`,
    controller.signal,
    (kind, frame) => {
      if (kind !== "matrix") return;
      receivedFrame = true;
      state.summaryWorkloadSnapshots.set(host.id, frame);
      renderSummaryWorkloadSnapshot(frame, engine);
      if (!Number(frame.host?.totals?.pending_count || 0)) {
        controller.abort();
      }
    },
  )
    .catch((error) => {
      if (error.name !== "AbortError" || !receivedFrame) {
        markSummaryWorkloadUnavailable(engine);
      }
    })
    .finally(() => window.clearTimeout(timeout));
}

// This is a Dockpilot condition, not the runtime status printed by
// `docker compose ls`. It summarizes whether Dockpilot can safely manage the
// discovered project and gives exceptional conditions precedence.
function projectCondition(project) {
  if (project.restore_recovery_required)
    return badge("Recovery required", "bad");
  if (project.collision) return badge("Collision", "bad");
  if (project.stale) return badge("Stale", "warn");
  if (!project.present) return badge("Missing", "warn");
  if (project.read_only) return badge("Read-only", "warn");
  return badge("Managed", "good");
}

function composeConfigState(value) {
  return (
    {
      "in-sync": "In sync",
      changed: "Changed",
      "no-baseline": "No baseline",
    }[String(value || "").toLowerCase()] || "Unknown"
  );
}

// A Compose Service is a model entry, not a Docker runtime object with its own
// state. This value is a Dockpilot summary of the Service's Container states.
function serviceRuntimeSummary(runtimeService) {
  const states = [
    ...new Set(
      (runtimeService?.containers || [])
        .filter((container) => !container.one_off)
        .map((container) => String(container.state || "").toLowerCase())
        .filter(Boolean),
    ),
  ];
  if (states.length === 1) return states[0];
  if (states.length > 1) return "Mixed";
  if (runtimeService?.profile_inactive) return "Excluded by profile";
  return runtimeService?.status || "No container";
}

function serviceActionsButton(
  project,
  service,
  runtimeService,
  pullAvailable,
  upAvailable,
  runtimeAvailable,
  buildUnavailableReason,
  runtimeUnavailableReason,
) {
  const serviceRuntime = serviceRuntimeSummary(runtimeService);
  return `
    <button
      class="row-menu-trigger"
      type="button"
      data-action="open-service-actions"
      data-project="${text(project.uid)}"
      data-agent="${text(project.agent_id)}"
      data-target="${text(service.name)}"
      data-service-runtime="${text(serviceRuntime)}"
      data-pull-available="${pullAvailable ? "true" : "false"}"
      data-up-available="${upAvailable ? "true" : "false"}"
      data-runtime-available="${runtimeAvailable ? "true" : "false"}"
      data-build-reason="${text(buildUnavailableReason)}"
      data-runtime-reason="${text(runtimeUnavailableReason)}"
      aria-label="Actions for ${text(service.name)} Service"
      aria-controls="service-actions-menu"
      aria-expanded="false"
      aria-haspopup="menu"
      title="Service actions"
    >…</button>
  `;
}

function renderProjectServicesPanel(
  project,
  runtime,
  mutable,
  actionState,
  host,
) {
  const runtimeServices = runtime?.services || [];
  const runtimeServiceByName = new Map(
    runtimeServices.map((service) => [service.name, service]),
  );
  const mutationUnavailableReason = !actionState
    ? host?.capabilities?.compose?.reason || "Compose operations unavailable"
    : !project.compose_executable
      ? "Docker Compose unavailable"
      : project.read_only
        ? "Project is read-only"
        : project.restore_recovery_required
          ? "Restore recovery is required"
          : "Operation unavailable";

  const rows = (project.defined_services || []).map((service) => {
    const runtimeService = runtimeServiceByName.get(service.name);
    const existingContainers = (runtimeService?.containers || []).filter(
      (container) => !container.one_off,
    );
    const containerHealth = [
      ...new Set(existingContainers.map((container) => container.health)),
    ].filter(Boolean);
    const publishedPorts = [
      ...new Map(
        existingContainers
          .flatMap((container) => container.ports || [])
          .map((port) => [
            [
              port.host_ip,
              port.published_port,
              port.target_port,
              port.protocol,
            ].join(":"),
            port,
          ]),
      ).values(),
    ];
    const runtimeAvailable = mutable && existingContainers.length > 0;
    const runtimeUnavailableReason = !mutable
      ? mutationUnavailableReason
      : !runtime
        ? "Compose container state unavailable"
        : "No existing Container for this Service";
    const pullAvailable = mutable && service.pull_available;
    const upAvailable = mutable && service.up_available;
    const buildUnavailableReason = !mutable
      ? mutationUnavailableReason
      : service.unavailable_reason || "Unavailable under the no-build policy";
    const buildState = service.build_required
      ? badge("Required", "bad")
      : service.has_build
        ? badge("Configured", "info")
        : badge("Not set");
    const serviceRuntime = runtime
      ? serviceRuntimeSummary(runtimeService)
      : "Service runtime unavailable";
    const health = containerHealth.join(", ") || "—";
    const profiles = (service.profiles || []).join(", ") || "Default";
    const ports = publishedPorts.length ? portsCell(publishedPorts) : "—";
    const actions = serviceActionsButton(
      project,
      service,
      runtimeService,
      pullAvailable,
      upAvailable,
      runtimeAvailable,
      buildUnavailableReason,
      runtimeUnavailableReason,
    );

    return `
      <tr>
        <td data-label="Service" title="${text(service.name)}"><span class="primary">${text(service.name)}</span></td>
        <td data-label="Service runtime" title="${text(serviceRuntime)}">${runtime ? stateBadge(serviceRuntime) : "—"}</td>
        <td data-label="Containers" title="${text(runtime ? existingContainers.length : "Unavailable")}">${runtime ? text(existingContainers.length) : "—"}</td>
        <td data-label="Health" title="${text(health)}">${text(health)}</td>
        <td data-label="Image" title="${text(service.image || "No declared Image")}">${text(service.image || "—")}</td>
        <td data-label="Build" title="${text(service.build_required ? buildUnavailableReason : service.has_build ? "Compose build configuration is present" : "No Compose build configuration")}">${buildState}</td>
        <td data-label="Pull policy" title="${text(service.pull_policy || "Not declared")}">${text(service.pull_policy || "—")}</td>
        <td data-label="Profiles" title="${text(profiles)}">${text(profiles)}</td>
        <td data-label="Depends on" title="${text((service.depends_on || []).join(", ") || "None")}">${text((service.depends_on || []).join(", ") || "None")}</td>
        <td data-label="Ports" title="${text(publishedPorts.length ? "Published ports" : "None")}">${ports}</td>
        <td class="service-actions-cell" data-label="">${actions}</td>
      </tr>
    `;
  });

  return `
    <section class="panel flush project-services-panel">
      <div class="panel-header inset">
        <div>
          <h2>Services</h2>
          <p>
            One fact per column from the effective Compose model and current
            Docker state. Open <strong>…</strong> for Service-level operations.
            Pull never builds and Up always uses
            <code>--no-build</code>; Down remains project-wide.
          </p>
        </div>
      </div>
      ${table(
        [
          "Service",
          "Service runtime",
          "Containers",
          "Health",
          "Image",
          "Build",
          "Pull policy",
          "Profiles",
          "Depends on",
          "Ports",
          "",
        ],
        rows,
        "No Services in the effective Compose model",
      )}
    </section>
  `;
}

function renderProjectServiceAttention(project, runtime) {
  const runtimeByService = new Map(
    (runtime?.services || []).map((service) => [service.name, service]),
  );
  const abnormalStates = new Set([
    "created",
    "dead",
    "exited",
    "paused",
    "removing",
    "restarting",
  ]);
  const rows = [];

  for (const service of project.defined_services || []) {
    const runtimeService = runtimeByService.get(service.name);
    const containers = (runtimeService?.containers || []).filter(
      (container) => !container.one_off,
    );
    const findings = [];

    if (service.build_required) {
      findings.push({
        label: "Build required",
        tone: "bad",
        evidence:
          service.unavailable_reason ||
          "Dockpilot v1 cannot satisfy this Service without an Image build",
      });
    }
    if (runtime && service.active !== false && containers.length === 0) {
      findings.push({
        label: "No container",
        tone: "warn",
        evidence: "No Container currently exists for this Service",
      });
    }
    const unhealthy = containers.filter(
      (container) => String(container.health).toLowerCase() === "unhealthy",
    );
    if (unhealthy.length) {
      findings.push({
        label: "unhealthy",
        tone: "bad",
        evidence: `${unhealthy.length} Container${unhealthy.length === 1 ? " is" : "s are"} unhealthy`,
      });
    }
    const stateExceptions = [
      ...new Set(
        containers
          .map((container) => String(container.state || "").toLowerCase())
          .filter((containerState) => abnormalStates.has(containerState)),
      ),
    ];
    for (const containerState of stateExceptions) {
      const matchingContainers = containers.filter((container) => {
        return String(container.state).toLowerCase() === containerState;
      });
      findings.push({
        label: containerState,
        tone: ["dead", "exited", "removing"].includes(containerState)
          ? "bad"
          : "warn",
        evidence:
          `${matchingContainers.length} Container` +
          `${matchingContainers.length === 1 ? " has" : "s have"} Docker state ` +
          containerState,
      });
    }
    if (!findings.length) continue;

    rows.push(`
      <tr>
        <td><a class="primary" href="#/projects/${encodeURIComponent(project.uid)}/services">${text(service.name)}</a></td>
        <td>${findings.map((finding) => badge(finding.label, finding.tone)).join(" ")}</td>
        <td>${text(findings.map((finding) => finding.evidence).join(" · "))}</td>
      </tr>
    `);
  }

  const runtimeUnavailable = runtime
    ? ""
    : `<div class="notice warning">Compose container state is unavailable. Only Compose model exceptions can be shown.</div>`;

  return `
    <section class="panel flush project-service-attention-panel">
      <div class="panel-header inset">
        <div>
          <h2>Services needing attention</h2>
          <p>
            Container-state exceptions and no-build policy blockers for active
            Services. Services excluded by inactive profiles are not treated
            as failures.
          </p>
        </div>
        <a class="quiet-button" href="#/projects/${encodeURIComponent(project.uid)}/services">View Services</a>
      </div>
      ${runtimeUnavailable}
      ${table(
        ["Service", "Condition", "Evidence"],
        rows,
        "No Service exceptions reported",
      )}
    </section>
  `;
}

async function renderProject(route, signal) {
  const project = projectByUID(route.id);
  if (!project)
    throw new Error(`Project ${route.id} is not in the Server index.`);
  const host = hostForProject(project);
  breadcrumbs([
    { label: "Home", href: "#/home" },
    {
      label: host?.display_name || project.agent_id,
      href: `#/hosts/${encodeURIComponent(project.agent_id)}/summary`,
    },
    {
      label: project.name,
      href: `#/projects/${encodeURIComponent(project.uid)}/summary`,
    },
    { label: route.tab },
  ]);
  const actionState =
    connectionAvailable(host) && host?.capabilities?.compose?.enabled;
  const mutable =
    actionState &&
    project.compose_executable &&
    !project.read_only &&
    !project.restore_recovery_required;
  const projectAction = (
    label,
    kind,
    className = "quiet-button",
    disabled = !mutable,
  ) =>
    `<button class="${className}" data-action="project-operation" data-kind="${kind}" data-project="${text(project.uid)}" data-agent="${text(project.agent_id)}" ${disabled ? "disabled" : ""}>${label}</button>`;
  const shell = () => {
    const projectLifecycleActions = [
      projectAction(
        "Pull",
        "compose.pull",
        "quiet-button",
        !mutable || !(project.pull_services || []).length,
      ),
      projectAction(
        "Up",
        "compose.up",
        "primary-button",
        !mutable || !project.project_up_available,
      ),
      projectAction("Down", "compose.down", "danger-button"),
    ].join("");
    const runtimeActions = [
      projectAction("Start", "compose.start"),
      projectAction("Stop", "compose.stop"),
      projectAction("Restart", "compose.restart"),
    ].join("");
    const actions = `
      <span title="Dockpilot condition">${projectCondition(project)}</span>
      <span class="project-action-group" role="group" aria-label="Apply or remove Compose project">
        ${projectLifecycleActions}
      </span>
      <span class="project-action-group" role="group" aria-label="Control existing Containers">
        ${runtimeActions}
      </span>
    `;

    return `${pageHeader(
      "Compose project",
      project.name,
      project.working_dir,
      actions,
    )}${projectTabs(project.uid, route.tab)}`;
  };
  if (route.tab === "summary") {
    let runtime;
    try {
      runtime = await jsonRequest(
        `/api/v1/projects/${encodeURIComponent(project.uid)}/runtime`,
        { signal },
      );
    } catch (_) {
      runtime = undefined;
    }
    const runtimeServices = runtime?.services || [];
    const noContainer = runtimeServices.filter(
      (service) => service.status === "No container",
    ).length;
    const inactive = runtimeServices.filter(
      (service) => service.profile_inactive,
    ).length;
    const ordinary = runtimeServices.reduce(
      (count, service) =>
        count +
        (service.containers || []).filter((container) => !container.one_off)
          .length,
      0,
    );
    const oneOff = runtimeServices.reduce(
      (count, service) =>
        count +
        (service.containers || []).filter((container) => container.one_off)
          .length,
      0,
    );
    const orphans = (runtime?.orphans || []).length;
    const composeOperations = actionState ? "Available" : "Unavailable";
    const fileAccess = !host?.capabilities?.fs_read?.enabled
      ? "Unavailable"
      : project.read_only || !host?.capabilities?.fs_write?.enabled
        ? "Read-only"
        : "Read and write";
    const projectRecord = !project.present
      ? "Missing"
      : project.stale
        ? "Stale"
        : project.last_observed_at
          ? `Current · ${formatTime(project.last_observed_at)}`
          : "Current · observation time unavailable";
    const projectOverview = definitionList({
      "Project directory": project.working_dir,
      "Compose files — merge order":
        (project.compose_files || []).join(" → ") || "None reported",
      "Included applications":
        (project.included_by || []).join(", ") || "None reported",
      "Active profiles":
        (project.active_profiles || []).join(", ") || "Default model",
      "Compose file graph": project.source_graph_complete
        ? "Complete"
        : "Incomplete",
    });
    const managementFacts = definitionList({
      Management: project.managed ? "Managed" : "Unmanaged",
      "Dockpilot discovery": projectRecord,
      "Compose operations": composeOperations,
      "File access": fileAccess,
      "Config drift": composeConfigState(project.drift),
      "Last verified": formatTime(project.last_verified_at),
    });
    const runtimeSummary = definitionList({
      "Services in model": (project.defined_services || []).length,
      Containers: runtime ? ordinary : "—",
      "Services with no container": runtime ? noContainer : "—",
      "Services excluded by profiles": runtime ? inactive : "—",
      "One-off containers": runtime ? oneOff : "—",
      "Orphan containers": runtime ? orphans : "—",
    });
    const upUnavailable = project.project_up_available
      ? ""
      : `<div class="notice warning"><strong>Project Up unavailable.</strong> ${text(project.project_up_reason || "The effective model requires a build.")}</div>`;

    view.innerHTML = `
      ${shell()}
      ${upUnavailable}
      <section class="panel project-summary-panel">
        <div class="panel-header">
          <div>
            <h2>Project</h2>
            <p>Effective Compose metadata and source relationships.</p>
          </div>
        </div>
        ${projectOverview}
        <section class="project-management-section" aria-labelledby="project-management-heading">
          <h3 id="project-management-heading">Dockpilot management</h3>
          ${managementFacts}
        </section>
      </section>
      <section class="panel project-runtime-panel">
        <div class="panel-header">
          <div>
            <h2>Containers</h2>
            <p>
              ${runtime ? `Compose containers observed ${text(formatTime(runtime.observed_at))}.` : "Compose container state unavailable."}
            </p>
          </div>
        </div>
        ${runtimeSummary}
      </section>
      ${renderProjectServiceAttention(project, runtime)}
    `;
    return;
  }
  if (route.tab === "services") {
    let runtime;
    try {
      runtime = await jsonRequest(
        `/api/v1/projects/${encodeURIComponent(project.uid)}/runtime`,
        { signal },
      );
    } catch (_) {
      runtime = undefined;
    }
    const upUnavailable = project.project_up_available
      ? ""
      : `<div class="notice warning"><strong>Project Up unavailable.</strong> ${text(project.project_up_reason || "The effective model requires a build.")}</div>`;

    view.innerHTML = `
      ${shell()}
      ${upUnavailable}
      ${renderProjectServicesPanel(project, runtime, mutable, actionState, host)}
    `;
    return;
  }
  if (route.tab === "containers") {
    const runtime = await jsonRequest(
      `/api/v1/projects/${encodeURIComponent(project.uid)}/runtime`,
      { signal },
    );
    const rows = [];
    for (const service of runtime.services || []) {
      for (const container of service.containers || [])
        rows.push(
          projectContainerRow(
            project.uid,
            project.agent_id,
            service.name,
            container,
          ),
        );
    }
    for (const orphan of runtime.orphans || [])
      rows.push(
        projectContainerRow(
          project.uid,
          project.agent_id,
          orphan.compose_service || "Unknown",
          orphan,
        ),
      );
    view.innerHTML = `${shell()}${runtime.observed_at ? `<div class="notice">Compose containers observed ${text(formatTime(runtime.observed_at))}.</div>` : `<div class="notice warning">Compose container observation time is unavailable.</div>`}<section class="panel flush">${table(["Service", "Compose role", "Container", "State", "Health", "Image", "Published ports", ""], rows, "No observed Compose Containers")}</section>`;
    if (route.inspect) {
      state.inspectorRoute = true;
      inspectContainer(project.agent_id, route.inspect);
    }
    return;
  }
  if (route.tab === "files") {
    view.innerHTML = shell() + renderFileWorkspace(project);
    window.requestAnimationFrame(fitFileEditorToViewport);
    bindFiles(project);
    return;
  }
  if (route.tab === "logs") {
    view.innerHTML = shell();
    await renderProjectLogs(project, signal);
    return;
  }
  if (route.tab === "backups") {
    view.innerHTML = shell();
    await renderBackups(project, signal);
    return;
  }
  if (route.tab === "activity") {
    view.innerHTML = shell() + auditScaffold("project", project.uid);
    bindAudit("project", project.uid);
    return;
  }
  throw new Error("Unknown project section.");
}

function projectContainerRow(projectUID, agentID, service, container) {
  return `<tr><td><span class="primary">${text(service)}</span></td><td>${composeContainerRole(container)}</td><td><a class="row-button" href="#/projects/${encodeURIComponent(projectUID)}/containers?inspect=${encodeURIComponent(container.id)}"><span class="primary">${text((container.names || []).join(", ") || shortID(container.id))}</span><div class="secondary mono">${text(shortID(container.id))}</div></a></td><td>${stateBadge(container.state)}</td><td>${container.health ? stateBadge(container.health) : "—"}</td><td>${text(container.image)}</td><td>${portsCell(container.ports)}</td><td>${containerActionsButton(agentID, projectUID, container)}</td></tr>`;
}

function sourceButton(kind, path, readable = true) {
  return `<button class="source-link" type="button" data-action="load-source" data-source-kind="${text(kind)}" data-path="${text(path)}" ${readable ? "" : "disabled"}>${text(path)}</button>`;
}
function renderFileWorkspace(project) {
  const includes = (project.source_references || []).filter(
    (item) => item.kind === "include",
  );
  const extendsRefs = (project.source_references || []).filter(
    (item) => item.kind === "extends",
  );
  return `<section class="panel flush"><div class="source-layout"><nav class="source-nav" aria-label="Compose sources">
    <div class="source-group"><h3>Compose files — merge order</h3>${(project.compose_files || []).map((path) => sourceButton("file", path)).join("") || `<p class="nav-empty">None reported</p>`}</div>
    <div class="source-group"><h3>Included applications</h3>${includes.map((item) => sourceButton("file", item.path, item.accessible)).join("") || `<p class="nav-empty">None reported</p>`}</div>
    <div class="source-group"><h3>Extends references</h3>${extendsRefs.map((item) => sourceButton("file", item.path, item.accessible)).join("") || `<p class="nav-empty">None reported</p>`}</div>
    <div class="source-group"><h3>Interpolation environment</h3>${sourceButton("file", ".env")}</div>
    <div class="source-group"><h3>Service environment files</h3>${(project.env_files || []).map((item) => sourceButton("file", item.path, item.readable)).join("") || `<p class="nav-empty">None reported</p>`}</div>
    <div class="source-group"><h3>Compose model</h3>${sourceButton("config", "docker compose config (masked)")}</div>
    <div class="source-group"><h3>Compose secrets</h3>${(project.secrets || []).map((item) => `<div class="source-link" title="${text(item.source || item.source_type)}">${text(item.name)} <span class="secondary">${text(item.source_type || (item.external ? "external" : ""))}</span></div>`).join("") || `<p class="nav-empty">None reported</p>`}</div>
    <div class="source-group"><h3>Compose configs</h3>${(project.configs || []).map((item) => `<div class="source-link" title="${text(item.source || item.source_type)}">${text(item.name)} <span class="secondary">${text(item.source_type || (item.external ? "external" : ""))}</span></div>`).join("") || `<p class="nav-empty">None reported</p>`}</div>
  </nav><div class="editor-pane"><div class="panel-header"><div><h2 id="editor-title">Choose a source</h2><p id="file-status">File content is live and never stored by the Server.</p></div><div class="form-actions"><button id="reveal-file" class="quiet-button" type="button" hidden>Reveal sensitive file</button><button id="reveal-config" class="quiet-button" type="button" hidden>Reveal resolved values</button><button id="save-file" class="primary-button" type="button" disabled>Save</button></div></div><textarea id="file-editor" aria-label="Compose file editor" spellcheck="false" disabled></textarea></div></div></section>`;
}

function bindFiles(project) {
  $("#view")
    .querySelectorAll("[data-action=load-source]")
    .forEach((button) =>
      button.addEventListener("click", () =>
        loadSource(
          project,
          button.dataset.sourceKind,
          button.dataset.path,
          false,
        ),
      ),
    );
  $("#reveal-config").addEventListener("click", async () => {
    if (
      !(await confirmAction(
        "Reveal resolved Compose values?",
        "docker compose config output may contain resolved sensitive values. It remains transient in this browser.",
        "Reveal",
      ))
    )
      return;
    loadSource(project, "config", "docker compose config", true);
  });
  $("#reveal-file").addEventListener("click", async () => {
    const loaded = state.loadedSource;
    if (!loaded) return;
    if (
      !(await confirmAction(
        "Reveal sensitive file?",
        "The value remains transient in this browser and is not stored by the Server.",
        "Reveal",
      ))
    )
      return;
    loadSource(project, "file", loaded.path, true);
  });
  $("#save-file").addEventListener("click", () => saveFile(project));
}
async function loadSource(project, kind, path, reveal) {
  const editor = $("#file-editor"),
    status = $("#file-status"),
    title = $("#editor-title"),
    save = $("#save-file"),
    revealButton = $("#reveal-config"),
    revealFile = $("#reveal-file");
  status.textContent = "Loading live source…";
  editor.disabled = true;
  save.disabled = true;
  revealButton.hidden = kind !== "config" || reveal;
  revealFile.hidden = true;
  try {
    if (kind === "config") {
      const result = await jsonRequest(
        `/api/v1/projects/${encodeURIComponent(project.uid)}/compose/config?reveal=${reveal}`,
      );
      title.textContent = reveal
        ? "docker compose config — revealed"
        : "docker compose config — masked";
      editor.value = result.output || "";
      status.textContent = "Transient Compose output; editing is disabled.";
      state.loadedFile = undefined;
      return;
    }
    const result = await jsonRequest(
      `/api/v1/projects/${encodeURIComponent(project.uid)}/files?path=${encodeURIComponent(path)}&reveal=${reveal}`,
    );
    title.textContent = path;
    editor.value = result.content || "";
    editor.disabled = Boolean(result.secret);
    save.disabled =
      Boolean(result.secret) ||
      project.read_only ||
      !hostForProject(project)?.capabilities?.fs_write?.enabled;
    status.textContent = result.secret
      ? reveal
        ? "Sensitive file revealed in this transient browser view. Editing remains disabled."
        : "Sensitive file is masked until explicit reveal."
      : `Loaded ${path} · sha256 ${result.sha256}`;
    state.loadedSource = result.secret && !reveal ? { path } : undefined;
    revealFile.hidden = !result.secret || reveal;
    state.loadedFile = { projectUID: project.uid, path, sha256: result.sha256 };
  } catch (error) {
    editor.value = "";
    status.textContent = error.message;
    state.loadedFile = undefined;
    state.loadedSource = undefined;
  }
}
async function saveFile(project) {
  const loaded = state.loadedFile;
  if (!loaded || loaded.projectUID !== project.uid) return;
  if (
    !(await confirmAction(
      `Save ${loaded.path}?`,
      "Dockpilot creates a pre-write backup and applies optimistic hash protection.",
      "Save",
    ))
  )
    return;
  try {
    const operation = await jsonRequest(
      `/api/v1/projects/${encodeURIComponent(project.uid)}/files`,
      {
        method: "PUT",
        body: JSON.stringify({
          operation_id: newOperationID("file-write"),
          relative_path: loaded.path,
          expected_sha256: loaded.sha256,
          content: $("#file-editor").value,
        }),
      },
    );
    void trackOperationToast(operation, "Write");
    $("#save-file").disabled = true;
    state.loadedFile = undefined;
  } catch (error) {
    showToast({
      tone: "error",
      title: "Write failed to start",
      message: error.message,
    });
  }
}

async function renderBackups(project, signal) {
  const backups = await jsonRequest(
    `/api/v1/projects/${encodeURIComponent(project.uid)}/backups`,
    { signal },
  );
  view.insertAdjacentHTML(
    "beforeend",
    `<div class="notice">Restore changes configuration files only. It never runs Compose Up.</div><section class="panel flush">${table(
      ["Created", "Trigger", "Exact restore scope", "Size", "Action"],
      backups.map(
        (item) =>
          `<tr><td><span class="primary">${text(formatTime(item.created_at))}</span><div class="secondary mono">${text(item.backup_id)}</div></td><td>${text(item.trigger)}</td><td>${item.paths_available ? (item.paths || []).map((path) => badge(path)).join(" ") || "No files" : `${badge("Unavailable", "warn")}<div class="secondary">Upgrade the Agent to enumerate this manifest.</div>`}</td><td>${text(formatBytes(item.size_bytes))}</td><td><button class="danger-button" data-action="restore-backup" data-project="${text(project.uid)}" data-backup="${text(item.backup_id)}" ${item.paths_available ? "" : "disabled"}>Restore</button></td></tr>`,
      ),
      "No configuration backups",
    )}</section>`,
  );
}

async function renderProjectLogs(project, signal) {
  const runtime = await jsonRequest(
    `/api/v1/projects/${encodeURIComponent(project.uid)}/runtime`,
    { signal },
  );
  view.append($("#logs-template").content.cloneNode(true));
  state.loadedLogs = "";
  window.requestAnimationFrame(fitLogsOutputToViewport);
  $("#logs-agent").value = project.agent_id;
  const services = runtime.services || [];
  const containers = [
    ...services.flatMap((service) =>
      (service.containers || []).map((container) => ({
        ...container,
        compose_service: container.compose_service || service.name,
      })),
    ),
    ...(runtime.orphans || []),
  ];
  $("#logs-services").insertAdjacentHTML(
    "beforeend",
    services
      .map(
        (service) =>
          `<option value="${text(service.name)}">${text(service.name)}</option>`,
      )
      .join(""),
  );
  const serviceSelect = $("#logs-services");
  const containerSelect = $("#logs-container");
  updateLogContainerOptions(containerSelect, containers, "");
  serviceSelect.addEventListener("change", () => {
    updateLogContainerOptions(containerSelect, containers, serviceSelect.value);
  });
  $("#project-logs-form").addEventListener("submit", (event) => {
    event.preventDefault();
    startProjectLogs(project);
  });
  $("#logs-stop").addEventListener("click", () =>
    state.streamController?.abort(),
  );
  const output = $("#logs-output"),
    follow = $("#logs-follow"),
    until = $("#logs-until"),
    latest = $("#logs-latest");
  output.dataset.autoFollow = "true";
  follow.addEventListener("change", () => {
    until.disabled = follow.checked;
    if (follow.checked) {
      until.value = "";
      output.dataset.autoFollow = "true";
      latest.hidden = true;
    }
  });
  until.disabled = true;
  until.addEventListener("input", () => {
    if (until.value) follow.checked = false;
  });
  output.addEventListener("scroll", () => {
    if (!follow.checked) return;
    const atBottom =
      output.scrollHeight - output.scrollTop - output.clientHeight < 28;
    output.dataset.autoFollow = String(atBottom);
    latest.hidden = atBottom;
  });
  latest.addEventListener("click", () => {
    output.dataset.autoFollow = "true";
    output.scrollTop = output.scrollHeight;
    latest.hidden = true;
  });
  $("#logs-clear").addEventListener("click", () => {
    state.loadedLogs = "";
    renderLoadedLogs(output);
    $("#logs-status").textContent =
      "Browser view cleared. Docker Engine logs were not deleted.";
  });
  $("#logs-find").addEventListener("input", (event) => {
    const query = event.target.value;
    const matchingLines = renderLoadedLogs(output, query);
    if (!query) {
      $("#logs-status").textContent = "Showing all loaded log lines.";
      return;
    }
    $("#logs-status").textContent =
      `${matchingLines} matching log line${matchingLines === 1 ? "" : "s"} shown.`;
  });
}

function updateLogContainerOptions(containerSelect, containers, service) {
  const matchingContainers = service
    ? containers.filter((container) => container.compose_service === service)
    : containers;
  const allLabel = service ? `All Containers in ${service}` : "All Containers";
  containerSelect.innerHTML = [
    `<option value="">${text(allLabel)}</option>`,
    ...matchingContainers.map(
      (container) =>
        `<option value="${text(container.id)}">${text((container.names || []).join(", ") || shortID(container.id))}</option>`,
    ),
  ].join("");
}

function renderLoadedLogs(output, query = $("#logs-find")?.value || "") {
  if (!query) {
    output.textContent = state.loadedLogs;
    return 0;
  }

  const normalizedQuery = query.toLocaleLowerCase();
  const matchingLines = state.loadedLogs
    .split("\n")
    .filter((line) => line.toLocaleLowerCase().includes(normalizedQuery));
  output.textContent = matchingLines.join("\n");
  return matchingLines.length;
}

function fitLogsOutputToViewport() {
  fitElementToViewport($("#logs-output"));
}

function fitFileEditorToViewport() {
  fitElementToViewport($("#file-editor"));
}

function fitElementToViewport(element) {
  if (!element) return;

  const main = $("#main");
  const mainBottomPadding = Number.parseFloat(
    window.getComputedStyle(main).paddingBottom,
  );
  const containerBottomPadding = Number.parseFloat(
    window.getComputedStyle(element.parentElement).paddingBottom,
  );
  const minimumHeight = Number.parseFloat(
    window.getComputedStyle(element).minHeight,
  );
  const elementDocumentTop =
    element.getBoundingClientRect().top + window.scrollY;
  const availableHeight =
    window.innerHeight -
    elementDocumentTop -
    mainBottomPadding -
    containerBottomPadding;

  element.style.height = `${Math.max(minimumHeight, availableHeight)}px`;
}
function localTimeToRFC3339(value) {
  return value ? new Date(value).toISOString() : "";
}
function projectLogURL(projectUID, form) {
  const params = new URLSearchParams({
    follow: form.get("follow") ? "true" : "false",
    tail: String(form.get("tail") || 0),
  });
  const container = String(form.get("container_id") || "");
  if (container) params.set("container_id", container);
  else
    for (const service of String(form.get("services") || "")
      .split(",")
      .map((value) => value.trim())
      .filter(Boolean))
      params.append("service", service);
  if (form.get("timestamps")) params.set("timestamps", "true");
  if (form.get("since"))
    params.set("since", localTimeToRFC3339(String(form.get("since"))));
  if (form.get("until"))
    params.set("until", localTimeToRFC3339(String(form.get("until"))));
  return `/api/v1/projects/${encodeURIComponent(projectUID)}/compose/logs?${params}`;
}
async function startProjectLogs(project) {
  state.streamController?.abort();
  state.streamController = new AbortController();
  const output = $("#logs-output"),
    status = $("#logs-status"),
    stop = $("#logs-stop");
  let terminalReason = "";
  const form = new FormData($("#project-logs-form"));
  if (form.get("follow") && form.get("until")) {
    status.textContent = "Follow and Until cannot be used together.";
    return;
  }
  state.loadedLogs = "";
  renderLoadedLogs(output);
  output.dataset.autoFollow = "true";
  status.textContent =
    "Streaming Docker Engine-retained logs; Dockpilot does not retain them.";
  stop.disabled = false;
  try {
    await streamSSE(
      projectLogURL(project.uid, form),
      state.streamController.signal,
      (kind, event) => {
        if (kind !== "log") return;
        appendLog(output, event);
        if (event.terminal)
          terminalReason = event.error || "Source ended normally";
      },
    );
    status.textContent = `Log stream ended: ${terminalReason || "connection closed"}. Start again to reconnect.`;
  } catch (error) {
    status.textContent =
      error.name === "AbortError" ? "Log stream stopped." : error.message;
  } finally {
    stop.disabled = true;
  }
}

function auditScaffold(scope, id) {
  return `<section class="panel flush"><div class="panel-inset"><form id="audit-filter" class="filter-bar"><input name="from" type="datetime-local" step="1" aria-label="From"><input name="until" type="datetime-local" step="1" aria-label="Until"><input name="resource" placeholder="Resource" aria-label="Resource"><input name="kind" placeholder="Kind" aria-label="Kind"><input name="actor" placeholder="Actor" aria-label="Actor"><button class="quiet-button" type="submit">Apply</button></form><div id="audit-coverage" class="notice">Coverage will load with the first page.</div></div><div id="audit-results">${table(["Time", "Kind", "Action", "Resource", "Actor"], [], "Apply filters to load stored Audit history")}</div><div class="panel-footer"><button id="audit-next" class="quiet-button" type="button" disabled>Load next page</button></div></section>`;
}
function bindAudit(scope, id) {
  const context = { scope, id, cursor: "" };
  $("#audit-filter").addEventListener("submit", (event) => {
    event.preventDefault();
    context.cursor = "";
    loadAudit(context, false);
  });
  $("#audit-next").addEventListener("click", () => loadAudit(context, true));
  loadAudit(context, false);
}
async function loadAudit(context, append) {
  const form = new FormData($("#audit-filter"));
  const params = new URLSearchParams({ limit: "100" });
  for (const key of ["from", "until"])
    if (form.get(key))
      params.set(key, localTimeToRFC3339(String(form.get(key))));
  for (const key of ["resource", "kind", "actor"])
    if (form.get(key)) params.set(key, String(form.get(key)));
  if (append && context.cursor) params.set("cursor", context.cursor);
  const path =
    context.scope === "host"
      ? `/api/v1/hosts/${encodeURIComponent(context.id)}/audit`
      : `/api/v1/projects/${encodeURIComponent(context.id)}/activity`;
  try {
    const page = await jsonRequest(`${path}?${params}`);
    const rows = (page.events || []).map(
      (event) =>
        `<tr><td>${text(formatTime(event.occurred_at))}</td><td>${stateBadge(event.kind)}</td><td>${text(event.action)}</td><td><span class="primary">${text(event.resource_type)}</span><div class="secondary mono">${text(event.resource_id || "")}</div></td><td>${text(event.actor || "unknown")}</td></tr>`,
    );
    const tableHTML = table(
      ["Time", "Kind", "Action", "Resource", "Actor"],
      rows,
      "No Audit events match these filters",
    );
    if (append) {
      const body = $("#audit-results tbody");
      body.insertAdjacentHTML("beforeend", rows.join(""));
    } else $("#audit-results").innerHTML = tableHTML;
    context.cursor = page.next_cursor
      ? `${page.next_cursor.incarnation}:${page.next_cursor.seq}`
      : "";
    $("#audit-next").disabled = !context.cursor;
    const coverage = page.coverage || {};
    const confirmed = (coverage.gaps || []).length,
      uncertain = (coverage.unknown_incarnations || []).length;
    $("#audit-coverage").className =
      `notice ${coverage.established && !confirmed && !uncertain ? "" : "warning"}`;
    $("#audit-coverage").textContent = !coverage.established
      ? "Audit continuity is uncertain because coverage has not been established."
      : confirmed
        ? `Confirmed Audit gap: ${confirmed} effective gap range(s).${uncertain ? ` Continuity is also uncertain across ${uncertain} incarnation(s).` : ""}`
        : uncertain
          ? `Audit continuity is uncertain across ${uncertain} incarnation(s); no confirmed gap range is currently recorded.`
          : "Audit coverage is established with no confirmed gaps or continuity uncertainty.";
  } catch (error) {
    $("#audit-results").innerHTML =
      `<div class="notice error">${text(error.message)}</div>`;
  }
}

async function renderOperations(signal, inspectID = "") {
  breadcrumbs([{ label: "Home", href: "#/home" }, { label: "Operations" }]);
  const page = await jsonRequest("/api/v1/operations?limit=200", { signal });
  state.operationsIndex = page.operations || [];
  view.innerHTML = `${pageHeader("Dockpilot control", "Operations", "Bounded Server index with request context and Agent-authoritative execution facts.")}<section class="panel flush">${operationTable(state.operationsIndex)}</section>`;
  if (inspectID) {
    state.inspectorRoute = true;
    inspectOperation(inspectID);
  }
}

function renderMetrics(host) {
  view.append($("#metrics-template").content.cloneNode(true));
  if (!host.capabilities?.metrics?.enabled) {
    $("#metrics-status").textContent =
      host.capabilities?.metrics?.reason || "Container stats unavailable";
    return;
  }
  state.streamController?.abort();
  state.streamController = new AbortController();
  state.metricsHistory = [];
  state.metricsMode = "hierarchy";
  state.metricsFrame = undefined;
  state.metricsTopOrder = [];
  $("#metrics-hierarchy").addEventListener("click", () => {
    state.metricsMode = "hierarchy";
    renderMatrixTable();
  });
  $("#metrics-top").addEventListener("click", () => {
    state.metricsMode = "top";
    renderMatrixTable();
  });
  streamSSE(
    `/api/v1/live/matrix?agent_id=${encodeURIComponent(host.id)}`,
    state.streamController.signal,
    (kind, frame) => {
      if (kind === "matrix") renderMatrix(frame);
    },
  ).catch((error) => {
    const status = $("#metrics-status");
    if (status) {
      status.textContent =
        error.name === "AbortError"
          ? "Container stats stopped."
          : error.message;
    }
  });
}
function renderMatrix(frame) {
  const status = $("#metrics-status");
  if (!status) return;

  const stale = [
    [frame.membership_stale, frame.membership_reason],
    [frame.workload_stale, frame.workload_reason],
    [frame.context_stale, frame.context_reason],
  ]
    .filter(([value]) => value)
    .map(([, reason]) => reason);
  status.textContent = stale.length
    ? `Partial stale frame: ${stale.join(" · ")}`
    : `Observed ${formatTime(frame.observed_at)} · Agent drops ${frame.agent_dropped_frames || 0} · Server drops ${frame.server_dropped_frames || 0}`;
  state.metricsFrame = frame;
  if (!state.metricsTopOrder.length) {
    const containers = [];
    for (const project of frame.projects || [])
      for (const service of project.services || [])
        for (const container of service.containers || [])
          containers.push(container);
    state.metricsTopOrder = containers
      .sort(
        (left, right) =>
          Number(right.sample?.cpu_percent || 0) -
          Number(left.sample?.cpu_percent || 0),
      )
      .map((container) => container.container_id);
  }
  renderMatrixTable();
}
function metricsValues(values, pending = false) {
  if (pending) return ["Waiting", "—", "—", "—", "Waiting"];
  const source = values.sample || values;
  const memoryUsage = values.memory_usage || source.memory_usage;
  const memoryLimit = values.memory_limit || source.memory_limit;
  const memory = values.memory_percent_known
    ? `${formatBytes(memoryUsage)} / ${formatBytes(memoryLimit)} · ${Number(values.memory_percent || 0).toFixed(1)}%`
    : values.memory_limit_unbounded
      ? `No container memory limit · ${formatBytes(memoryUsage)} used`
      : formatBytes(memoryUsage);
  return [
    `${Number(source.cpu_percent || 0).toFixed(1)}%`,
    memory,
    `${formatBytes(source.network_rx)} / ${formatBytes(source.network_tx)}`,
    `${formatBytes(source.block_read)} / ${formatBytes(source.block_write)}`,
    source.health || values.health || "—",
  ];
}
function matrixRow(name, depth, values, pending = false, detail = "") {
  const [cpu, memory, network, block, status] = metricsValues(values, pending);
  return `<tr><td><div class="hierarchy-name depth-${depth}"><span class="primary">${text(name)}</span>${detail ? `<div class="secondary">${text(detail)}</div>` : ""}</div></td><td>${text(cpu)}</td><td>${text(memory)}</td><td>${text(network)}</td><td>${text(block)}</td><td>${status === "—" ? "—" : stateBadge(status)}</td></tr>`;
}
function renderMatrixTable() {
  const frame = state.metricsFrame;
  const hierarchyButton = $("#metrics-hierarchy");
  const topButton = $("#metrics-top");
  const metricsTable = $("#metrics-table");
  if (!frame || !hierarchyButton || !topButton || !metricsTable) return;

  hierarchyButton.classList.toggle("active", state.metricsMode === "hierarchy");
  topButton.classList.toggle("active", state.metricsMode === "top");
  const rows = [];
  if (state.metricsMode === "hierarchy") {
    rows.push(
      matrixRow(
        "All containers",
        0,
        frame.host?.totals || {},
        false,
        "Container aggregate; shared network namespaces may be double-counted",
      ),
    );
    for (const project of frame.projects || []) {
      rows.push(
        matrixRow(
          project.project_name || "Standalone",
          1,
          project.totals || {},
          false,
          project.unmapped ? "Unmapped Docker Containers" : "Compose project",
        ),
      );
      for (const service of project.services || []) {
        rows.push(
          matrixRow(
            service.service || "Standalone",
            2,
            service.totals || {},
            false,
            "Service",
          ),
        );
        for (const container of service.containers || [])
          rows.push(
            matrixRow(
              shortID(container.container_id),
              3,
              container,
              container.pending,
              container.image || "Container",
            ),
          );
      }
    }
  } else {
    const byID = new Map();
    for (const project of frame.projects || [])
      for (const service of project.services || [])
        for (const container of service.containers || [])
          byID.set(container.container_id, { container, project, service });
    for (const id of state.metricsTopOrder) {
      const value = byID.get(id);
      if (value)
        rows.push(
          matrixRow(
            shortID(id),
            0,
            value.container,
            value.container.pending,
            [
              value.project.project_name || "Standalone",
              value.service.service || "Standalone",
              value.container.image,
            ]
              .filter(Boolean)
              .join(" · "),
          ),
        );
    }
  }
  metricsTable.innerHTML = table(
    [
      "Name",
      "CPU %",
      "Memory usage / limit",
      "Net I/O (RX / TX)",
      "Block I/O (read / write)",
      "State",
    ],
    rows,
    "Waiting for current Container samples",
  );
}

async function streamSSE(url, signal, onEvent) {
  const response = await fetch(url, {
    signal,
    cache: "no-store",
    headers: { Accept: "text/event-stream" },
  });
  if (!response.ok) throw new Error(`Live stream failed (${response.status})`);
  if (!response.body) throw new Error("Streaming response unavailable");
  const reader = response.body.getReader(),
    decoder = new TextDecoder();
  let buffer = "";
  while (true) {
    const { value, done } = await reader.read();
    if (done) break;
    buffer += decoder.decode(value, { stream: true }).replaceAll("\r\n", "\n");
    if (buffer.length > MAX_SSE_BUFFER)
      throw new Error("Live event exceeded the browser buffer limit");
    let boundary;
    while ((boundary = buffer.indexOf("\n\n")) >= 0) {
      const block = buffer.slice(0, boundary);
      buffer = buffer.slice(boundary + 2);
      if (!block || block.startsWith(":")) continue;
      let event = "message";
      const data = [];
      for (const line of block.split("\n")) {
        if (line.startsWith("event:")) event = line.slice(6).trim();
        if (line.startsWith("data:")) data.push(line.slice(5).trimStart());
      }
      if (data.length) onEvent(event, JSON.parse(data.join("\n")));
    }
  }
}
function decodeLogData(encoded) {
  if (!encoded) return "";
  const binary = atob(encoded),
    bytes = new Uint8Array(binary.length);
  for (let index = 0; index < binary.length; index += 1)
    bytes[index] = binary.charCodeAt(index);
  return utf8Decoder.decode(bytes);
}
function appendLog(output, event) {
  let prefix = event.stream === "STDERR" ? "[stderr] " : "";
  if (event.dropped_bytes)
    prefix += `[dropped ${event.dropped_bytes} bytes / ${event.dropped_lines || 0} lines]\n`;
  state.loadedLogs += prefix + decodeLogData(event.data);
  if (state.loadedLogs.length > MAX_LOG_CHARACTERS)
    state.loadedLogs = `[older browser output removed]\n${state.loadedLogs.slice(-MAX_LOG_CHARACTERS)}`;
  renderLoadedLogs(output);
  if (output.dataset.autoFollow !== "false")
    output.scrollTop = output.scrollHeight;
}

function newOperationID(prefix) {
  return `${prefix}-${crypto.randomUUID()}`;
}

function operationDisplayLabel(kind) {
  const action = String(kind || "operation")
    .split(".")
    .pop();
  return `${action.slice(0, 1).toUpperCase()}${action.slice(1)}`;
}

const COMPOSE_SERVICE_OPERATION_KINDS = new Set([
  "compose.pull",
  "compose.up",
  "compose.start",
  "compose.stop",
  "compose.restart",
]);

function composeServiceTarget(operation) {
  if (!COMPOSE_SERVICE_OPERATION_KINDS.has(String(operation.kind || ""))) {
    return "";
  }
  return String(operation.target || "");
}

function operationToastContext(operation) {
  const service = composeServiceTarget(operation);
  if (service) return service;
  const project = projectByUID(operation.project_uid);
  const host = hostByID(operation.agent_id);
  return (
    project?.name ||
    operation.project_uid ||
    host?.display_name ||
    operation.agent_id ||
    "Dockpilot"
  );
}

function operationToastOptions(operation, label, context) {
  const status = String(operation.status || "").toLowerCase();
  const detail = operation.error || "";
  const partial = operation.partial_effects_possible
    ? "Partial effects may be possible."
    : "";
  const message = [
    context,
    detail || partial || (!operationTerminal(operation) && operation.phase),
  ]
    .filter(Boolean)
    .join(" · ");
  const base = {
    message,
    operationID: operation.operation_id,
  };

  if (status === "success" && operation.partial_effects_possible) {
    return {
      ...base,
      tone: "warning",
      title: `${label} completed with attention`,
    };
  }
  if (status === "success") {
    return { ...base, tone: "success", title: `${label} completed` };
  }
  if (status === "canceled") {
    return { ...base, tone: "warning", title: `${label} canceled` };
  }
  if (status === "interrupted") {
    return { ...base, tone: "warning", title: `${label} interrupted` };
  }
  if (status === "rejected") {
    return { ...base, tone: "error", title: `${label} rejected` };
  }
  if (status === "failed") {
    return { ...base, tone: "error", title: `${label} failed` };
  }
  return { ...base, tone: "info", title: `${label} started` };
}

function operationPollDelay(milliseconds, signal) {
  return new Promise((resolve, reject) => {
    if (signal.aborted) {
      reject(new DOMException("Operation tracking stopped", "AbortError"));
      return;
    }
    const aborted = () => {
      window.clearTimeout(timeout);
      reject(new DOMException("Operation tracking stopped", "AbortError"));
    };
    const timeout = window.setTimeout(() => {
      signal.removeEventListener("abort", aborted);
      resolve();
    }, milliseconds);
    signal.addEventListener("abort", aborted, { once: true });
  });
}

async function trackOperationToast(
  operation,
  explicitLabel = "",
  explicitContext = "",
) {
  const label = explicitLabel || operationDisplayLabel(operation.kind);
  const context = explicitContext || operationToastContext(operation);
  const toast = showToast(operationToastOptions(operation, label, context));
  if (!operation.operation_id || !operation.agent_id) return toast;

  state.operationToastControllers.get(operation.operation_id)?.abort();
  const controller = new AbortController();
  state.operationToastControllers.set(operation.operation_id, controller);
  toast.addEventListener("toastdismiss", () => controller.abort(), {
    once: true,
  });
  if (operationTerminal(operation)) {
    state.operationToastControllers.delete(operation.operation_id);
    return toast;
  }

  const startedAt = Date.now();
  let failures = 0;
  let current = operation;
  try {
    while (Date.now() - startedAt < OPERATION_POLL_MAX_DURATION) {
      const delay = Math.min(
        OPERATION_POLL_MAX_INTERVAL,
        OPERATION_POLL_INTERVAL * 2 ** Math.min(failures, 3),
      );
      await operationPollDelay(delay, controller.signal);
      try {
        current = await jsonRequest(
          `/api/v1/agents/${encodeURIComponent(operation.agent_id)}/operations/${encodeURIComponent(operation.operation_id)}`,
          { signal: controller.signal },
        );
        failures = 0;
        updateToast(toast, operationToastOptions(current, label, context));
        if (operationTerminal(current)) return toast;
      } catch (error) {
        if (error.name === "AbortError") throw error;
        failures += 1;
        if (failures >= 3) {
          updateToast(toast, {
            tone: "warning",
            title: `${label} status unavailable`,
            message: `${context} · ${error.message}`,
            operationID: operation.operation_id,
          });
        }
      }
    }
    updateToast(toast, {
      tone: "warning",
      title: `${label} still running`,
      message: `${context} · Automatic status tracking ended`,
      operationID: operation.operation_id,
    });
    return toast;
  } catch (error) {
    if (error.name !== "AbortError") {
      updateToast(toast, {
        tone: "warning",
        title: `${label} status unavailable`,
        message: `${context} · ${error.message}`,
        operationID: operation.operation_id,
      });
    }
    return toast;
  } finally {
    if (
      state.operationToastControllers.get(operation.operation_id) === controller
    ) {
      state.operationToastControllers.delete(operation.operation_id);
    }
  }
}

function closeServiceActionsMenu({ restoreFocus = false } = {}) {
  const menu = $("#service-actions-menu");
  const trigger = state.serviceActionsTrigger;

  menu.hidden = true;
  menu.style.removeProperty("left");
  menu.style.removeProperty("top");
  menu.style.removeProperty("visibility");
  trigger?.setAttribute("aria-expanded", "false");
  state.serviceActionsTrigger = undefined;
  if (restoreFocus && trigger?.isConnected) trigger.focus();
}

function positionRowActionsMenu(menu, button) {
  const triggerBounds = button.getBoundingClientRect();
  const viewportGap = 8;
  const menuGap = 4;

  menu.style.visibility = "hidden";
  menu.hidden = false;
  const menuBounds = menu.getBoundingClientRect();
  const left = Math.min(
    Math.max(viewportGap, triggerBounds.right - menuBounds.width),
    window.innerWidth - menuBounds.width - viewportGap,
  );
  const spaceBelow = window.innerHeight - triggerBounds.bottom - viewportGap;
  const top =
    spaceBelow >= menuBounds.height
      ? triggerBounds.bottom + menuGap
      : Math.max(viewportGap, triggerBounds.top - menuBounds.height - menuGap);

  menu.style.left = `${left}px`;
  menu.style.top = `${top}px`;
  menu.style.removeProperty("visibility");
}

function openServiceActions(button) {
  const menu = $("#service-actions-menu");
  if (state.serviceActionsTrigger === button && !menu.hidden) {
    closeServiceActionsMenu({ restoreFocus: true });
    return;
  }
  closeContainerActionsMenu();
  closeServiceActionsMenu();
  const pullAvailable = button.dataset.pullAvailable === "true";
  const upAvailable = button.dataset.upAvailable === "true";
  const runtimeAvailable = button.dataset.runtimeAvailable === "true";
  const availability = {
    "compose.pull": pullAvailable,
    "compose.up": upAvailable,
    "compose.start": runtimeAvailable,
    "compose.stop": runtimeAvailable,
    "compose.restart": runtimeAvailable,
  };
  const reasons = [
    !pullAvailable && !upAvailable ? button.dataset.buildReason : "",
    !runtimeAvailable ? button.dataset.runtimeReason : "",
  ].filter((reason, index, values) => {
    return reason && values.indexOf(reason) === index;
  });

  menu.setAttribute(
    "aria-label",
    `Actions for ${button.dataset.target} Service`,
  );
  $("#service-actions-menu-reason").textContent =
    reasons.join(" · ") ||
    "Pull never builds. Every Service Up includes --no-build.";
  menu
    .querySelectorAll("[data-action=service-operation]")
    .forEach((actionButton) => {
      actionButton.dataset.agent = button.dataset.agent;
      actionButton.dataset.project = button.dataset.project;
      actionButton.dataset.target = button.dataset.target;
      actionButton.disabled = !availability[actionButton.dataset.kind];
      actionButton.title = actionButton.disabled
        ? actionButton.dataset.kind === "compose.pull" ||
          actionButton.dataset.kind === "compose.up"
          ? button.dataset.buildReason
          : button.dataset.runtimeReason
        : "";
    });
  state.serviceActionsTrigger = button;
  button.setAttribute("aria-expanded", "true");
  positionRowActionsMenu(menu, button);
  const firstAvailable = menu.querySelector(
    "[data-action=service-operation]:not(:disabled)",
  );
  (
    firstAvailable || menu.querySelector("[data-action=service-operation]")
  ).focus({ preventScroll: true });
}

function closeContainerActionsMenu({ restoreFocus = false } = {}) {
  const menu = $("#container-actions-menu");
  const trigger = state.containerActionsTrigger;

  menu.hidden = true;
  menu.style.removeProperty("left");
  menu.style.removeProperty("top");
  menu.style.removeProperty("visibility");
  trigger?.setAttribute("aria-expanded", "false");
  state.containerActionsTrigger = undefined;
  if (restoreFocus && trigger?.isConnected) trigger.focus();
}

function openContainerActions(button) {
  const menu = $("#container-actions-menu");
  if (state.containerActionsTrigger === button && !menu.hidden) {
    closeContainerActionsMenu({ restoreFocus: true });
    return;
  }
  closeServiceActionsMenu();
  closeContainerActionsMenu();
  const stateName = String(button.dataset.containerState || "unknown");
  const normalizedState = stateName.toLowerCase();
  const protectedContainer = button.dataset.protected === "true";
  const startAvailable = ["created", "exited"].includes(normalizedState);
  const stopAvailable = ["running", "paused", "restarting"].includes(
    normalizedState,
  );
  const restartAvailable = [
    "running",
    "paused",
    "restarting",
    "exited",
  ].includes(normalizedState);
  const removeAvailable = ["created", "exited", "dead"].includes(
    normalizedState,
  );
  const availability = {
    "container.start": startAvailable,
    "container.stop": stopAvailable && !protectedContainer,
    "container.restart": restartAvailable && !protectedContainer,
    "container.remove": removeAvailable && !protectedContainer,
  };

  menu.setAttribute(
    "aria-label",
    `Actions for ${button.dataset.containerName}`,
  );
  $("#container-actions-menu-reason").textContent = protectedContainer
    ? `${stateName} · Direct Docker actions target only this Container. ${button.dataset.protectionReason || "This Container is protected from Stop, Restart, and Remove."}`
    : `${stateName} · Direct Docker actions target only this Container. Start is for created or exited Containers. Remove requires a stopped Container and retains Volumes.`;

  menu
    .querySelectorAll("[data-action=container-operation]")
    .forEach((actionButton) => {
      actionButton.dataset.agent = button.dataset.agent;
      actionButton.dataset.project = button.dataset.project || "";
      actionButton.dataset.container = button.dataset.container;
      actionButton.dataset.containerName = button.dataset.containerName;
      actionButton.disabled = !availability[actionButton.dataset.kind];
      actionButton.title = actionButton.disabled
        ? protectedContainer && actionButton.dataset.kind !== "container.start"
          ? button.dataset.protectionReason || "Protected Container"
          : `Unavailable while Container is ${stateName}`
        : "";
    });
  state.containerActionsTrigger = button;
  button.setAttribute("aria-expanded", "true");
  positionRowActionsMenu(menu, button);
  const firstAvailable = menu.querySelector(
    "[data-action=container-operation]:not(:disabled)",
  );
  (
    firstAvailable || menu.querySelector("[data-action=container-operation]")
  ).focus({ preventScroll: true });
}

async function startContainerOperation(button) {
  const kind = button.dataset.kind;
  const name = button.dataset.containerName;
  closeContainerActionsMenu();

  if (
    kind === "container.restart" &&
    !(await confirmAction(
      `Restart ${name}?`,
      "This interrupts only the selected Container. Restart does not apply Compose configuration or environment changes.",
      "Restart",
    ))
  ) {
    return;
  }
  if (
    kind === "container.remove" &&
    !(await confirmAction(
      `Remove ${name}?`,
      "The stopped Container and its writable layer will be removed. Dockpilot does not force-stop it and does not remove attached Volumes.",
      "Remove",
    ))
  ) {
    return;
  }

  button.disabled = true;
  try {
    const routeKey = state.routeKey;
    const operation = await jsonRequest("/api/v1/operations", {
      method: "POST",
      body: JSON.stringify({
        operation_id: newOperationID(kind.replace(".", "-")),
        agent_id: button.dataset.agent,
        project_uid: button.dataset.project || "",
        kind,
        target: button.dataset.container,
      }),
    });
    void trackOperationToast(operation, operationDisplayLabel(kind), name).then(
      () => {
        if (state.routeKey === routeKey) renderRoute();
      },
    );
  } catch (error) {
    showToast({
      tone: "error",
      title: `${operationDisplayLabel(kind)} failed to start`,
      message: error.message,
    });
  }
}

async function startProjectOperation(button) {
  const kind = button.dataset.kind;
  const target = button.dataset.target || "";
  const project = button.dataset.project;
  const agent = button.dataset.agent;
  const projectName = projectByUID(project)?.name || project;
  const actionContext = target || projectName;

  if (
    !target &&
    kind === "compose.pull" &&
    !(await confirmAction(
      `Pull declared Images for ${actionContext}?`,
      "Dockpilot will download Images declared by eligible Services. It will not start Containers, build Images, or fall back to a build.",
      "Pull",
    ))
  ) {
    return;
  }
  if (
    !target &&
    kind === "compose.up" &&
    !(await confirmAction(
      `Apply ${actionContext}?`,
      "Dockpilot may create, recreate, and start Containers from declared Images. It always uses --no-build and never builds Images.",
      "Up",
    ))
  ) {
    return;
  }
  if (
    kind === "compose.down" &&
    !(await confirmAction(
      `Run Compose Down for ${actionContext}?`,
      "Containers for Services in the current Compose model and unused Compose-created Networks will be removed. Observed one-off and orphan Containers may remain. Named Volumes and external Networks or Volumes will be retained.",
      "Down",
    ))
  ) {
    return;
  }
  if (
    kind === "compose.restart" &&
    !(await confirmAction(
      target
        ? `Restart ${target} Service?`
        : `Restart existing Containers for ${actionContext}?`,
      "Restart does not apply Compose configuration or environment changes. Use Up to apply configuration.",
      "Restart",
    ))
  ) {
    return;
  }

  button.disabled = true;
  try {
    const operation = await jsonRequest("/api/v1/operations", {
      method: "POST",
      body: JSON.stringify({
        operation_id: newOperationID(kind.replace(".", "-")),
        agent_id: agent,
        project_uid: project,
        kind,
        target,
      }),
    });
    void trackOperationToast(
      operation,
      operationDisplayLabel(kind),
      actionContext,
    );
  } catch (error) {
    showToast({
      tone: "error",
      title: `${operationDisplayLabel(kind)} failed to start`,
      message: `${actionContext} · ${error.message}`,
    });
  } finally {
    button.disabled = false;
  }
}
async function cancelOperation(button) {
  if (
    !(await confirmAction(
      "Cancel this operation?",
      "Cancellation semantics depend on its current phase. Partial effects may be possible.",
      "Cancel operation",
    ))
  )
    return;
  try {
    const result = await jsonRequest(
      `/api/v1/agents/${encodeURIComponent(button.dataset.agent)}/operations/${encodeURIComponent(button.dataset.operation)}/cancel`,
      { method: "POST", body: "{}" },
    );
    const accepted = result.outcome === "ACCEPTED";
    showToast({
      tone: accepted ? "info" : "warning",
      title: accepted ? "Cancellation requested" : "Cancellation not applied",
      message: result.outcome,
      operationID: button.dataset.operation,
    });
    renderRoute();
  } catch (error) {
    showToast({
      tone: "error",
      title: "Cancellation failed",
      message: error.message,
      operationID: button.dataset.operation,
    });
  }
}
async function restoreBackup(button) {
  if (
    !(await confirmAction(
      "Restore this configuration backup?",
      "Current configuration is snapshotted first. Restore never runs Compose Up.",
      "Restore",
    ))
  )
    return;
  try {
    const operation = await jsonRequest(
      `/api/v1/projects/${encodeURIComponent(button.dataset.project)}/backups/${encodeURIComponent(button.dataset.backup)}/restore`,
      {
        method: "POST",
        body: JSON.stringify({
          operation_id: newOperationID("backup-restore"),
        }),
      },
    );
    void trackOperationToast(operation, "Restore");
  } catch (error) {
    showToast({
      tone: "error",
      title: "Restore failed to start",
      message: error.message,
    });
  }
}

let activeInspectorResize;

function inspectorWidthBounds() {
  const viewportWidth = window.innerWidth;
  if (viewportWidth <= INSPECTOR_FULL_PAGE_BREAKPOINT) {
    return {
      minimum: viewportWidth,
      maximum: viewportWidth,
      resizable: false,
    };
  }

  let maximum = Math.floor(viewportWidth * INSPECTOR_MAX_VIEWPORT_RATIO);
  if (viewportWidth >= INSPECTOR_PUSH_BREAKPOINT) {
    maximum = Math.min(
      maximum,
      viewportWidth -
        INSPECTOR_DESKTOP_SIDEBAR_WIDTH -
        INSPECTOR_MAIN_MIN_WIDTH,
    );
  }
  const minimum = Math.min(INSPECTOR_MIN_WIDTH, maximum);
  return {
    minimum,
    maximum: Math.max(minimum, maximum),
    resizable: true,
  };
}

function storedInspectorWidth() {
  try {
    const width = Number(localStorage.getItem(INSPECTOR_WIDTH_STORAGE_KEY));
    return Number.isFinite(width) && width > 0
      ? width
      : INSPECTOR_DEFAULT_WIDTH;
  } catch {
    return INSPECTOR_DEFAULT_WIDTH;
  }
}

function applyInspectorWidth(requestedWidth, persist = false) {
  const bounds = inspectorWidthBounds();
  const width = bounds.resizable
    ? Math.round(
        Math.min(bounds.maximum, Math.max(bounds.minimum, requestedWidth)),
      )
    : bounds.maximum;
  document.documentElement.style.setProperty("--inspector-width", `${width}px`);

  const handle = $("#inspector-resize-handle");
  handle.tabIndex = bounds.resizable ? 0 : -1;
  if (bounds.resizable) handle.removeAttribute("aria-hidden");
  else handle.setAttribute("aria-hidden", "true");
  handle.setAttribute("aria-valuemin", String(bounds.minimum));
  handle.setAttribute("aria-valuemax", String(bounds.maximum));
  handle.setAttribute("aria-valuenow", String(width));
  handle.setAttribute("aria-valuetext", `${width} pixels wide`);

  if (persist && bounds.resizable) {
    try {
      localStorage.setItem(INSPECTOR_WIDTH_STORAGE_KEY, String(width));
    } catch {
      // Inspector resizing remains usable when browser storage is disabled.
    }
  }
  return width;
}

function restoreInspectorWidth() {
  applyInspectorWidth(storedInspectorWidth());
}

function beginInspectorResize(event) {
  if (event.button !== 0) return;
  const bounds = inspectorWidthBounds();
  if (!bounds.resizable) return;

  const handle = event.currentTarget;
  handle.setPointerCapture?.(event.pointerId);
  activeInspectorResize = {
    handle,
    pointerID: event.pointerId,
    startWidth: $("#inspector").getBoundingClientRect().width,
    startX: event.clientX,
  };
  document.body.classList.add("inspector-resizing");
  event.preventDefault();
}

function moveInspectorResize(event) {
  if (
    !activeInspectorResize ||
    event.pointerId !== activeInspectorResize.pointerID
  ) {
    return;
  }
  const requestedWidth =
    activeInspectorResize.startWidth +
    activeInspectorResize.startX -
    event.clientX;
  applyInspectorWidth(requestedWidth);
  event.preventDefault();
}

function finishInspectorResize(event) {
  if (
    !activeInspectorResize ||
    event.pointerId !== activeInspectorResize.pointerID
  ) {
    return;
  }
  const width = $("#inspector").getBoundingClientRect().width;
  applyInspectorWidth(width, true);
  if (activeInspectorResize.handle.hasPointerCapture?.(event.pointerId)) {
    activeInspectorResize.handle.releasePointerCapture(event.pointerId);
  }
  activeInspectorResize = undefined;
  document.body.classList.remove("inspector-resizing");
}

function resizeInspectorWithKeyboard(event) {
  if (!["ArrowLeft", "ArrowRight"].includes(event.key)) return;
  const bounds = inspectorWidthBounds();
  if (!bounds.resizable) return;
  const direction = event.key === "ArrowLeft" ? 1 : -1;
  const step = event.shiftKey
    ? INSPECTOR_KEYBOARD_STEP * 4
    : INSPECTOR_KEYBOARD_STEP;
  const width = $("#inspector").getBoundingClientRect().width;
  applyInspectorWidth(width + direction * step, true);
  event.preventDefault();
}

function openInspectorHTML(title, html) {
  restoreInspectorWidth();
  $("#inspector-title").textContent = title;
  $("#inspector-body").innerHTML = html;
  $("#inspector").hidden = false;
  $("#scrim").hidden = false;
  document.body.classList.add("inspector-open");
  $("#inspector-close").focus();
}
function openInspector(title, values) {
  openInspectorHTML(title, definitionList(values));
}
function closeInspector() {
  state.inspectorRequest += 1;
  const inspector = $("#inspector");
  inspector.hidden = true;
  $("#scrim").hidden = true;
  document.body.classList.remove("inspector-open");
  if (state.inspectorRoute) {
    state.inspectorRoute = false;
    history.back();
  }
}
function beginInspectorRequest() {
  return {
    token: ++state.inspectorRequest,
    routeKey: state.routeKey,
    signal: state.routeController?.signal,
  };
}
function inspectorRequestCurrent(request) {
  return (
    request.token === state.inspectorRequest &&
    request.routeKey === state.routeKey &&
    !request.signal?.aborted
  );
}
function operationInspector(operation, freshness) {
  const phases = ["PREPARING", "EXECUTING", "COMMITTING", "FINALIZING"],
    current = String(operation.phase || "").toUpperCase(),
    currentIndex = phases.indexOf(current);
  const steps = `<ol class="phase-list">${phases.map((phase, index) => `<li><span class="primary">${phase}</span><span>${index < currentIndex ? "completed" : index === currentIndex ? "active" : operationTerminal(operation) ? "not reached" : "pending"}</span></li>`).join("")}</ol>`;
  const output = `<pre class="terminal output-tail">${text(operation.output_tail || "No output captured")}</pre>${operation.output_truncated ? `<p class="muted">Earlier output was truncated.</p>` : ""}`;
  let notice = "";
  if (freshness === "indexed") {
    notice = `<div class="notice warning">Last synchronized Server index. Current Agent detail is unavailable.</div>`;
  } else if (freshness === "refreshing") {
    notice = `<div class="notice">Showing the last synchronized Server index while current Agent detail loads.</div>`;
  }
  const project = projectByUID(operation.project_uid);
  const service = composeServiceTarget(operation);
  openInspectorHTML(
    operation.kind || "Operation",
    notice +
      definitionList({
        "Operation ID": operation.operation_id,
        Agent: operation.agent_id,
        "Compose project": project?.name || operation.project_uid,
        Service: service,
        Target: service ? "" : operation.target,
        Status: operation.status,
        Phase: operation.phase,
        Revision: operation.revision,
        "Partial effects": operation.partial_effects_possible,
        "Cancel mode": operation.cancel_mode,
        Cancelability: operation.can_cancel
          ? "Cancelable"
          : operation.cancelability_reason,
        Requested: formatTime(operation.requested_at),
        Started: formatTime(operation.started_at),
        Finished: formatTime(operation.finished_at),
        Error: operation.error,
      }) +
      inspectorSection("Phase", steps) +
      inspectorSection("Output tail", output),
  );
}
async function inspectOperation(id) {
  const request = beginInspectorRequest();
  try {
    let indexed = state.operationsIndex.find(
      (item) => item.operation_id === id,
    );
    if (!indexed) {
      const page = await jsonRequest("/api/v1/operations?limit=200", {
        signal: request.signal,
      });
      indexed = (page.operations || []).find(
        (item) => item.operation_id === id,
      );
    }
    if (!indexed)
      throw new Error("Operation is no longer in the bounded result.");
    if (!inspectorRequestCurrent(request)) return;
    const reachable = connectionAvailable(hostByID(indexed.agent_id));
    operationInspector(indexed, reachable ? "refreshing" : "indexed");
    if (!reachable) return;
    try {
      const operation = await jsonRequest(
        `/api/v1/agents/${encodeURIComponent(indexed.agent_id)}/operations/${encodeURIComponent(id)}`,
        { signal: request.signal },
      );
      if (inspectorRequestCurrent(request))
        operationInspector(operation, "live");
    } catch (error) {
      if (inspectorRequestCurrent(request) && error.name !== "AbortError")
        showToast({
          tone: "warning",
          title: "Current operation detail unavailable",
          message: `Showing the synchronized index · ${error.message}`,
          operationID: id,
        });
    }
  } catch (error) {
    if (inspectorRequestCurrent(request) && error.name !== "AbortError")
      showToast({
        tone: "error",
        title: "Operation unavailable",
        message: error.message,
        operationID: id,
      });
  }
}
function inspectorSection(title, content) {
  return `<section class="inspector-section"><h3>${text(title)}</h3>${content}</section>`;
}
function mapDetails(values, emptyMessage = "None reported") {
  const entries = Object.entries(values || {}).sort(([left], [right]) =>
    left.localeCompare(right),
  );
  return entries.length
    ? definitionList(Object.fromEntries(entries))
    : `<p class="muted">${text(emptyMessage)}</p>`;
}
function referenceDetails(values, emptyMessage = "No Container references") {
  return table(
    ["Container", "State", "Compose project / service", "Mount destination"],
    (values || []).map(
      (item) =>
        `<tr><td><span class="primary">${text(item.container_name || shortID(item.container_id))}</span><div class="secondary mono">${text(shortID(item.container_id))}</div></td><td>${item.state ? stateBadge(item.state) : "—"}</td><td>${text(item.compose_project || "—")}<div class="secondary">${text(item.compose_service || "")}</div></td><td class="mono">${text(item.destination || "—")}</td></tr>`,
    ),
    emptyMessage,
  );
}
function networkAttachmentDetails(values) {
  return table(
    ["Container", "Compose project / service", "IP addresses", "MAC address"],
    (values || []).map(
      (item) =>
        `<tr><td><span class="primary">${text(item.container_name || shortID(item.container_id))}</span><div class="secondary mono">${text(shortID(item.container_id))}</div></td><td>${text(item.compose_project || "—")}<div class="secondary">${text(item.compose_service || "")}</div></td><td class="mono">${text([item.ipv4, item.ipv6].filter(Boolean).join(" · ") || "—")}</td><td class="mono">${text(item.mac || "—")}</td></tr>`,
    ),
    "No connected Containers",
  );
}
async function inspectContainer(agentID, id) {
  const request = beginInspectorRequest();
  openInspector("Container", {
    "Container ID": id,
    Loading: "Current Docker details…",
  });
  try {
    const item = await jsonRequest(
      `/api/v1/hosts/${encodeURIComponent(agentID)}/containers/${encodeURIComponent(id)}`,
      { signal: request.signal },
    );
    if (!inspectorRequestCurrent(request)) return;
    const restart = item.restart_policy
      ? `${item.restart_policy}${item.restart_maximum_retry ? ` (max ${item.restart_maximum_retry})` : ""}`
      : "—";
    const mounts = table(
      ["Type", "Source", "Destination", "Access"],
      (item.mounts || []).map(
        (mount) =>
          `<tr><td>${text(mount.type)}</td><td class="mono">${text(mount.source || "—")}</td><td class="mono">${text(mount.destination)}</td><td>${mount.read_write ? "Read/write" : "Read-only"}</td></tr>`,
      ),
      "No mounts",
    );
    const networks = table(
      ["Network", "Addresses", "MAC", "Aliases"],
      (item.networks || []).map(
        (network) =>
          `<tr><td><span class="primary">${text(network.name)}</span><div class="secondary mono">${text(shortID(network.network_id))}</div></td><td class="mono">${text([network.ipv4, network.ipv6].filter(Boolean).join(" · ") || "—")}</td><td class="mono">${text(network.mac || "—")}</td><td>${text((network.aliases || []).join(", ") || "—")}</td></tr>`,
      ),
      "No connected Networks",
    );
    openInspectorHTML(
      (item.names || []).join(", ") || "Container",
      definitionList({
        "Container ID": item.id,
        Image: item.image,
        "Image ID": item.image_id,
        State: item.state,
        Health: item.health,
        "Exit code": item.exit_code,
        "OOM killed": item.oom_killed,
        Created: item.created_at,
        Started: item.started_at,
        Finished: item.finished_at,
        "Restart count": item.restart_count,
        "Restart policy": restart,
        "Stop signal": item.stop_signal,
        "Stop timeout":
          item.stop_timeout_seconds === undefined
            ? "—"
            : `${item.stop_timeout_seconds}s`,
        "Logging driver": item.logging_driver,
        Entrypoint: (item.entrypoint || []).join(" "),
        Command: (item.command || []).join(" "),
        "Published ports": (item.ports || [])
          .map(
            (port) =>
              `${port.host_ip ? `${port.host_ip}:` : ""}${port.published_port} → ${port.target_port}/${port.protocol}`,
          )
          .join(", "),
        "Exposed ports (image config)": (item.exposed_ports || []).join(", "),
        "Compose project": item.compose_project,
        "Compose service": item.compose_service,
        "One-off": item.one_off,
        Protection: item.protected
          ? item.protection_reason || "Protected"
          : "No",
      }) +
        inspectorSection("Mounts", mounts) +
        inspectorSection("Networks", networks) +
        inspectorSection("Labels", mapDetails(item.labels)),
    );
  } catch (error) {
    if (inspectorRequestCurrent(request) && error.name !== "AbortError")
      openInspector("Container unavailable", {
        "Container ID": id,
        Reason: error.message,
      });
  }
}
async function inspectImage(agentID, id) {
  const request = beginInspectorRequest();
  openInspector("Image", {
    "Image ID": id,
    Loading: "Current Docker details…",
  });
  try {
    const item = await jsonRequest(
      `/api/v1/hosts/${encodeURIComponent(agentID)}/images/${encodeURIComponent(id)}`,
      { signal: request.signal },
    );
    if (!inspectorRequestCurrent(request)) return;
    openInspectorHTML(
      (item.repo_tags || [])[0] || "Untagged Image",
      definitionList({
        "Image ID": item.id,
        Tags: (item.repo_tags || []).join(", ") || "Untagged",
        Digests: (item.repo_digests || []).join(", "),
        Created: item.created,
        Author: item.author,
        Platform: [item.os, item.os_version, item.architecture, item.variant]
          .filter(Boolean)
          .join(" · "),
        Size: formatBytes(item.size_bytes),
        Layers: item.layer_count,
        User: item.user,
        "Working directory": item.working_dir,
        Entrypoint: (item.entrypoint || []).join(" "),
        Command: (item.command || []).join(" "),
        "Exposed ports": (item.exposed_ports || []).join(", "),
      }) +
        inspectorSection(
          "Containers using this Image",
          referenceDetails(
            item.used_by,
            "Unused — no Containers reference this Image",
          ),
        ) +
        inspectorSection("Labels", mapDetails(item.labels)),
    );
  } catch (error) {
    if (inspectorRequestCurrent(request) && error.name !== "AbortError")
      openInspector("Image unavailable", {
        "Image ID": id,
        Reason: error.message,
      });
  }
}
async function inspectNetwork(agentID, id) {
  const request = beginInspectorRequest();
  openInspector("Network", {
    "Network ID": id,
    Loading: "Current Docker details…",
  });
  try {
    const item = await jsonRequest(
      `/api/v1/hosts/${encodeURIComponent(agentID)}/networks/${encodeURIComponent(id)}`,
      { signal: request.signal },
    );
    if (!inspectorRequestCurrent(request)) return;
    const ipam =
      (item.ipam || [])
        .map((config, index) =>
          inspectorSection(
            `IPAM configuration ${index + 1}`,
            definitionList({
              Subnet: config.subnet,
              "IP range": config.ip_range,
              Gateway: config.gateway,
            }) + mapDetails(config.aux_addresses, "No auxiliary addresses"),
          ),
        )
        .join("") || `<p class="muted">No IPAM configurations</p>`;
    openInspectorHTML(
      item.name || "Network",
      definitionList({
        "Network ID": item.id,
        Created: item.created,
        Driver: item.driver,
        Scope: item.scope,
        "Compose project": item.compose_project,
        "Compose network": item.compose_network,
        "IPv4 enabled": item.enable_ipv4,
        "IPv6 enabled": item.enable_ipv6,
        Internal: item.internal,
        Attachable: item.attachable,
        Ingress: item.ingress,
        "Config-only": item.config_only,
        "IPAM driver": item.ipam_driver,
      }) +
        inspectorSection("IPAM", ipam) +
        inspectorSection(
          "Containers",
          networkAttachmentDetails(item.attachments),
        ) +
        inspectorSection("Driver options", mapDetails(item.options)) +
        inspectorSection("Labels", mapDetails(item.labels)),
    );
  } catch (error) {
    if (inspectorRequestCurrent(request) && error.name !== "AbortError")
      openInspector("Network unavailable", {
        "Network ID": id,
        Reason: error.message,
      });
  }
}
async function inspectVolume(agentID, name) {
  const request = beginInspectorRequest();
  openInspector("Volume", {
    Volume: name,
    Loading: "Current Docker details…",
  });
  try {
    const item = await jsonRequest(
      `/api/v1/hosts/${encodeURIComponent(agentID)}/volumes/${encodeURIComponent(name)}`,
      { signal: request.signal },
    );
    if (!inspectorRequestCurrent(request)) return;
    openInspectorHTML(
      item.name || "Volume",
      definitionList({
        Driver: item.driver,
        Scope: item.scope,
        Created: item.created_at,
        Mountpoint: item.mountpoint,
        "Compose project": item.compose_project,
        "Compose volume": item.compose_volume,
      }) +
        inspectorSection(
          "Containers using this Volume",
          referenceDetails(
            item.references,
            "No Containers reference this Volume",
          ),
        ) +
        inspectorSection("Driver options", mapDetails(item.options)) +
        inspectorSection("Labels", mapDetails(item.labels)),
    );
  } catch (error) {
    if (inspectorRequestCurrent(request) && error.name !== "AbortError")
      openInspector("Volume unavailable", {
        Volume: name,
        Reason: error.message,
      });
  }
}

function confirmAction(title, copy, label) {
  return new Promise((resolve) => {
    const dialog = $("#confirm-dialog");
    $("#confirm-title").textContent = title;
    $("#confirm-copy").textContent = copy;
    $("#confirm-submit").textContent = label;
    const listener = () => {
      dialog.removeEventListener("close", listener);
      scheduleAutoRefresh();
      resolve(dialog.returnValue === "confirm");
    };
    dialog.addEventListener("close", listener);
    dialog.showModal();
    scheduleAutoRefresh();
  });
}

let activeColumnResize;

function currentTableColumnWidths(tableElement) {
  return Array.from(tableElement.querySelectorAll("thead th"), (heading) => {
    return Math.round(heading.getBoundingClientRect().width);
  });
}

function minimumTableColumnWidth(tableElement) {
  const columnCount = tableElement.querySelectorAll("thead th").length;
  return Math.min(
    TABLE_COLUMN_MIN_WIDTH,
    Math.max(
      TABLE_COLUMN_ABSOLUTE_MIN_WIDTH,
      tableElement.getBoundingClientRect().width / columnCount / 2,
    ),
  );
}

function tableColumnRatios(widths) {
  const totalWidth = widths.reduce((total, width) => total + width, 0);
  return widths.map((width) => (width / totalWidth) * 100);
}

function updateTableResizeHandles(tableElement) {
  const minimumWidth = Math.round(minimumTableColumnWidth(tableElement));
  tableElement.querySelectorAll(".column-resize-handle").forEach((handle) => {
    const heading = handle.closest("th");
    handle.setAttribute(
      "aria-valuenow",
      String(Math.round(heading.getBoundingClientRect().width)),
    );
    handle.setAttribute("aria-valuemin", String(minimumWidth));
    handle.setAttribute(
      "aria-valuemax",
      String(
        Math.round(tableElement.getBoundingClientRect().width - minimumWidth),
      ),
    );
  });
}

function applyTableColumnRatios(tableElement, ratios) {
  const columns = tableElement.querySelectorAll("colgroup col");
  if (columns.length !== ratios.length) return;

  ratios.forEach((ratio, index) => {
    columns[index].style.width = `${ratio}%`;
  });
  tableElement.style.width = "100%";
  tableElement.dataset.columnsResized = "true";
  tableElement.closest(".table-wrap").scrollLeft = 0;
  updateTableResizeHandles(tableElement);
}

function applyTableColumnWidths(tableElement, widths) {
  applyTableColumnRatios(tableElement, tableColumnRatios(widths));
}

function persistTableColumnWidths(tableElement, widths) {
  try {
    localStorage.setItem(
      tableElement.dataset.tableWidthKey,
      JSON.stringify(
        tableColumnRatios(widths).map((ratio) => Number(ratio.toFixed(4))),
      ),
    );
  } catch {
    // Column resizing remains usable for this page when browser storage is off.
  }
}

function restoreStoredTableWidths(rootElement = view) {
  rootElement
    .querySelectorAll("table.resizable-table")
    .forEach((tableElement) => {
      const ratios = storedTableRatios(
        tableElement.dataset.tableWidthKey,
        tableElement.querySelectorAll("colgroup col").length,
      );
      if (ratios.length) applyTableColumnRatios(tableElement, ratios);
    });
}

function beginColumnResize(event, handle) {
  if (event.button !== 0) return;
  const tableElement = handle.closest("table.resizable-table");
  if (!tableElement) return;

  const index = Number(handle.dataset.columnIndex);
  if (index >= tableElement.querySelectorAll("thead th").length - 1) return;

  handle.setPointerCapture?.(event.pointerId);
  applyTableColumnWidths(tableElement, currentTableColumnWidths(tableElement));
  const widths = currentTableColumnWidths(tableElement);
  activeColumnResize = {
    handle,
    index,
    pointerID: event.pointerId,
    startX: event.clientX,
    tableElement,
    widths,
  };
  document.body.classList.add("column-resizing");
  event.preventDefault();
}

function moveColumnResize(event) {
  if (!activeColumnResize || event.pointerId !== activeColumnResize.pointerID)
    return;

  const widths = [...activeColumnResize.widths];
  const index = activeColumnResize.index;
  const nextIndex = index + 1;
  const minimumWidth = minimumTableColumnWidth(activeColumnResize.tableElement);
  const requestedDelta = event.clientX - activeColumnResize.startX;
  const delta = Math.min(
    activeColumnResize.widths[nextIndex] - minimumWidth,
    Math.max(minimumWidth - activeColumnResize.widths[index], requestedDelta),
  );
  widths[index] = activeColumnResize.widths[index] + delta;
  widths[nextIndex] = activeColumnResize.widths[nextIndex] - delta;
  applyTableColumnWidths(activeColumnResize.tableElement, widths);
  event.preventDefault();
}

function finishColumnResize(event) {
  if (!activeColumnResize || event.pointerId !== activeColumnResize.pointerID)
    return;

  const widths = currentTableColumnWidths(activeColumnResize.tableElement);
  persistTableColumnWidths(activeColumnResize.tableElement, widths);
  if (activeColumnResize.handle.hasPointerCapture?.(event.pointerId)) {
    activeColumnResize.handle.releasePointerCapture(event.pointerId);
  }
  activeColumnResize = undefined;
  document.body.classList.remove("column-resizing");
}

function resizeColumnWithKeyboard(event, handle) {
  if (!["ArrowLeft", "ArrowRight"].includes(event.key)) return;
  const tableElement = handle.closest("table.resizable-table");
  if (!tableElement) return;

  const widths = currentTableColumnWidths(tableElement);
  const index = Number(handle.dataset.columnIndex);
  const nextIndex = index + 1;
  if (nextIndex >= widths.length) return;
  const direction = event.key === "ArrowLeft" ? -1 : 1;
  const step = event.shiftKey
    ? TABLE_COLUMN_KEYBOARD_STEP * 4
    : TABLE_COLUMN_KEYBOARD_STEP;
  const minimumWidth = minimumTableColumnWidth(tableElement);
  const delta = Math.min(
    widths[nextIndex] - minimumWidth,
    Math.max(minimumWidth - widths[index], direction * step),
  );
  widths[index] += delta;
  widths[nextIndex] -= delta;
  applyTableColumnWidths(tableElement, widths);
  persistTableColumnWidths(
    tableElement,
    currentTableColumnWidths(tableElement),
  );
  event.preventDefault();
}

function updateColumnResizeHandle(handle) {
  const tableElement = handle.closest("table.resizable-table");
  if (tableElement) updateTableResizeHandles(tableElement);
}

async function renderRoute({ showPending = true, throwOnError = false } = {}) {
  closeServiceActionsMenu();
  closeContainerActionsMenu();
  state.routeController?.abort();
  state.streamController?.abort();
  state.inspectorRequest += 1;
  state.routeController = new AbortController();
  const route = parsedRoute();
  state.routeKey = route.key;
  if (!route.inspect) {
    $("#inspector").hidden = true;
    $("#scrim").hidden = true;
    document.body.classList.remove("inspector-open");
    state.inspectorRoute = false;
  }
  renderSidebar();
  if (showPending) showLoading();
  try {
    await loadDashboard();
    if (route.kind === "home") await renderHome(state.routeController.signal);
    else if (route.kind === "search") renderSearch();
    else if (route.kind === "host")
      await renderHost(route, state.routeController.signal);
    else if (route.kind === "project")
      await renderProject(route, state.routeController.signal);
    else await renderOperations(state.routeController.signal, route.inspect);
    if (state.routeKey === route.key) {
      restoreStoredTableWidths();
      const focusTarget =
        route.kind === "search" ? $("#global-search") : $("#main");
      focusTarget.focus({ preventScroll: true });
      if (route.kind === "search") {
        const end = focusTarget.value.length;
        focusTarget.setSelectionRange(end, end);
      }
    }
  } catch (error) {
    if (error.name === "AbortError") return;
    if (throwOnError) throw error;
    showError(error);
  } finally {
    scheduleAutoRefresh();
  }
}

async function refreshDockpilot() {
  if (state.refreshInFlight) return;

  state.refreshInFlight = true;
  clearAutoRefreshTimer();
  const refreshButton = $("#refresh");
  refreshButton.disabled = true;
  refreshButton.setAttribute("aria-busy", "true");
  try {
    await loadDashboard(true);
    await renderRoute({ showPending: false, throwOnError: true });
  } catch (error) {
    if (error.name !== "AbortError") {
      showToast({
        tone: "error",
        title: "Refresh failed",
        message: error.message,
      });
    }
  } finally {
    state.refreshInFlight = false;
    refreshButton.disabled = false;
    refreshButton.removeAttribute("aria-busy");
    scheduleAutoRefresh();
  }
}

$("#view").addEventListener("click", (event) => {
  const button = event.target.closest("[data-action]");
  if (!button) return;
  const actions = {
    "go-back": () => history.back(),
    "retry-route": () => renderRoute(),
    "home-filter": () => {
      const filter = button.dataset.filter;
      $("#view")
        .querySelectorAll(".compact-filter")
        .forEach((item) => item.classList.toggle("active", item === button));
      $("#view")
        .querySelectorAll("[data-home-state]")
        .forEach((row) => {
          row.hidden =
            filter !== "all" &&
            !row.dataset.homeState.split(" ").includes(filter);
        });
    },
    "project-operation": () => startProjectOperation(button),
    "open-service-actions": () => openServiceActions(button),
    "open-container-actions": () => openContainerActions(button),
    "cancel-operation": () => cancelOperation(button),
    "restore-backup": () => restoreBackup(button),
    "inspect-operation": () => inspectOperation(button.dataset.operation),
    "inspect-container": () =>
      inspectContainer(button.dataset.agent, button.dataset.container),
    "inspect-image": () =>
      inspectImage(button.dataset.agent, button.dataset.object),
    "inspect-network": () =>
      inspectNetwork(button.dataset.agent, button.dataset.object),
    "inspect-volume": () =>
      inspectVolume(button.dataset.agent, button.dataset.object),
  };
  actions[button.dataset.action]?.();
});
$("#service-actions-menu").addEventListener("click", (event) => {
  const button = event.target.closest("[data-action=service-operation]");
  if (!button) return;
  closeServiceActionsMenu();
  startProjectOperation(button);
});
document.addEventListener("click", (event) => {
  if (
    !event.target.closest("#service-actions-menu") &&
    !event.target.closest("[data-action=open-service-actions]")
  ) {
    closeServiceActionsMenu();
  }
  if (
    !event.target.closest("#container-actions-menu") &&
    !event.target.closest("[data-action=open-container-actions]")
  ) {
    closeContainerActionsMenu();
  }
});
document.addEventListener("keydown", (event) => {
  if (event.key === "Escape" && !$("#service-actions-menu").hidden) {
    event.preventDefault();
    closeServiceActionsMenu({ restoreFocus: true });
  }
  if (event.key === "Escape" && !$("#container-actions-menu").hidden) {
    event.preventDefault();
    closeContainerActionsMenu({ restoreFocus: true });
  }
});
$("#container-actions-menu").addEventListener("click", (event) => {
  const button = event.target.closest("[data-action=container-operation]");
  if (!button) return;
  closeContainerActionsMenu();
  startContainerOperation(button);
});
$("#view").addEventListener("pointerdown", (event) => {
  const handle = event.target.closest(".column-resize-handle");
  if (handle) beginColumnResize(event, handle);
});
$("#view").addEventListener("keydown", (event) => {
  const handle = event.target.closest(".column-resize-handle");
  if (handle) resizeColumnWithKeyboard(event, handle);
});
$("#view").addEventListener("focusin", (event) => {
  const handle = event.target.closest(".column-resize-handle");
  if (handle) updateColumnResizeHandle(handle);
});
window.addEventListener("pointermove", moveColumnResize);
window.addEventListener("pointerup", finishColumnResize);
window.addEventListener("pointercancel", finishColumnResize);
const inspectorResizeHandle = $("#inspector-resize-handle");
inspectorResizeHandle.addEventListener("pointerdown", beginInspectorResize);
inspectorResizeHandle.addEventListener("keydown", resizeInspectorWithKeyboard);
window.addEventListener("pointermove", moveInspectorResize);
window.addEventListener("pointerup", finishInspectorResize);
window.addEventListener("pointercancel", finishInspectorResize);
window.addEventListener("resize", () => {
  closeServiceActionsMenu();
  closeContainerActionsMenu();
  if (!activeInspectorResize) restoreInspectorWidth();
  fitLogsOutputToViewport();
  fitFileEditorToViewport();
});
window.addEventListener(
  "scroll",
  () => {
    if (state.serviceActionsTrigger && !$("#service-actions-menu").hidden) {
      positionRowActionsMenu(
        $("#service-actions-menu"),
        state.serviceActionsTrigger,
      );
    }
    if (state.containerActionsTrigger && !$("#container-actions-menu").hidden) {
      positionRowActionsMenu(
        $("#container-actions-menu"),
        state.containerActionsTrigger,
      );
    }
  },
  true,
);
state.autoRefreshInterval = storedAutoRefreshInterval();
updateAutoRefreshControl();
$("#refresh-interval").addEventListener("change", (event) => {
  const interval = Number(event.target.value);
  state.autoRefreshInterval = AUTO_REFRESH_INTERVALS.has(interval)
    ? interval
    : 0;
  try {
    localStorage.setItem(
      AUTO_REFRESH_STORAGE_KEY,
      String(state.autoRefreshInterval),
    );
  } catch {
    // Auto-refresh still works for this tab when browser storage is disabled.
  }
  scheduleAutoRefresh();
});
$("#refresh").addEventListener("click", () => void refreshDockpilot());
$("#inspector-close").addEventListener("click", closeInspector);
$("#scrim").addEventListener("click", closeInspector);
$("#sidebar-toggle").addEventListener("click", () => {
  document.body.classList.toggle("nav-open");
  $("#sidebar-toggle").setAttribute(
    "aria-expanded",
    String(document.body.classList.contains("nav-open")),
  );
});
window.addEventListener("hashchange", () => {
  document.body.classList.remove("nav-open");
  renderRoute();
});
document.addEventListener("visibilitychange", scheduleAutoRefresh);
window.addEventListener("beforeunload", () => {
  clearAutoRefreshTimer();
  state.routeController?.abort();
  state.streamController?.abort();
  state.operationToastControllers.forEach((controller) => controller.abort());
});

loadDashboard().then(renderRoute).catch(showError);
