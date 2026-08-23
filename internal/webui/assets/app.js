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
  routeKey: "",
  metricsHistory: [],
  metricsMode: "hierarchy",
  metricsFrame: undefined,
  metricsTopOrder: [],
  inspectorRoute: false,
  inspectorRequest: 0,
  operationsIndex: [],
  loadedFile: undefined,
  loadedSource: undefined,
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

function showToast(message) {
  const toast = document.createElement("div");
  toast.className = "toast";
  toast.textContent = String(message);
  toast.tabIndex = 0;
  toast.title = "Activate to dismiss";
  toast.addEventListener("click", () => toast.remove());
  toast.addEventListener("keydown", (event) => {
    if (event.key === "Enter" || event.key === " ") toast.remove();
  });
  $("#toast-region").prepend(toast);
}

function showLoading(
  title = "Loading current state",
  detail = "Reading live facts from the connected Agent.",
) {
  view.innerHTML = `<div class="state-panel"><span class="spinner" aria-hidden="true"></span><h1>${text(title)}</h1><p>${text(detail)}</p></div>`;
}

function showError(error, title = "This view is unavailable") {
  view.innerHTML = `<div class="state-panel"><span class="badge bad">Unavailable</span><h1>${text(title)}</h1><p>${text(error.message || error)}</p><button class="quiet-button" type="button" data-action="retry-route">Try again</button></div>`;
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
function table(headers, rows, emptyMessage = "No rows to show") {
  const rendered = rows.slice(0, MAX_RENDERED_ROWS).join("");
  return `<div class="table-wrap"><table><thead><tr>${headers.map((header) => `<th scope="col">${text(header)}</th>`).join("")}</tr></thead><tbody>${rendered || emptyRow(headers.length, emptyMessage)}</tbody></table></div>`;
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
    return { kind: "operations", key: "operations" };
  return { kind: "home", key: "home" };
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
        <span class="host-dot ${available ? "online" : ""}" aria-hidden="true"></span><span>${text(host.display_name || host.id)}</span>
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
    ["metrics", "Live Metrics"],
    ["audit", "Audit"],
  ]);
const projectTabs = (id, active) =>
  tabs("projects", id, active, [
    ["summary", "Summary"],
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
  view.innerHTML = `${pageHeader("Fleet", "Home", "Compare Docker hosts, inspect deterministic exceptions, and route to real host or Compose project context.")}<input id="home-search" class="search-field" type="search" placeholder="Search Docker hosts and Compose projects" aria-label="Search Docker hosts and Compose projects"><div class="compact-filters" aria-label="Docker host state filters"><button class="quiet-button compact-filter active" data-action="home-filter" data-filter="all">All hosts ${hosts.length}</button><button class="quiet-button compact-filter" data-action="home-filter" data-filter="docker">Docker Engine unavailable ${dockerUnavailable}</button><button class="quiet-button compact-filter" data-action="home-filter" data-filter="compose">Docker Compose unavailable ${composeUnavailable}</button><button class="quiet-button compact-filter" data-action="home-filter" data-filter="discovery">Discovery incomplete ${discoveryIncomplete}</button></div><section class="panel"><div class="panel-header"><div><h2>Needs attention</h2><p>Deterministic Dockpilot-known exceptions only.</p></div></div><ul class="attention-list">${attention.map((item) => `<li><a class="primary" href="${item.project ? `#/projects/${encodeURIComponent(item.project.uid)}/summary` : `#/hosts/${encodeURIComponent(item.host.id)}/summary`}">${text(item.project?.name || item.host?.display_name || item.host?.id)}</a><div class="secondary">${text(item.copy)}</div></li>`).join("") || `<li class="muted">No deterministic exceptions reported.</li>`}</ul></section><section class="panel flush"><div class="panel-header inset"><div><h2>Docker hosts</h2><p>No registered host disappears when a live probe fails.</p></div></div>${table(["Docker host", "Agent", "Docker Engine", "Docker Compose", "Discovery"], rows, "No Docker hosts registered")}</section>`;
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
      const reachable = connectionAvailable(host);
      const canCancel = operation.can_cancel && reachable;
      const cancelReason = reachable
        ? operation.cancelability_reason
        : host?.capabilities?.connection?.reason || "Agent is unavailable";
      return `<tr>${lead}<td>${text(operation.project_uid || operation.agent_id)}${operation.target ? `<div class="secondary">Target: ${text(operation.target)}</div>` : ""}</td><td>${stateBadge(operation.status)}</td><td>${text(operation.phase || "—")}</td><td>${text(formatTime(operation.requested_at))}</td><td>${canCancel ? `<button class="quiet-button" data-action="cancel-operation" data-agent="${text(operation.agent_id)}" data-operation="${text(operation.operation_id)}">Cancel</button>` : `<span class="secondary" title="${text(cancelReason || "Not cancelable")}">Unavailable</span>`}</td></tr>`;
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
  const shell = () =>
    `${pageHeader("Docker host", host.display_name || host.id, `Agent ${host.id} · ${connectionAvailable(host) ? "connected" : "offline"}`)}${hostTabs(host.id, route.tab)}`;
  if (route.tab === "summary") {
    const detail = await jsonRequest(
      `/api/v1/hosts/${encodeURIComponent(host.id)}`,
      { signal },
    );
    const engine = detail.engine_summary;
    const unavailable = !engine
      ? `<div class="notice warning">${text(detail.engine_summary_reason || "Engine Summary is unavailable.")}</div>`
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
    view.innerHTML = `${shell()}${unavailable}<div class="split-grid"><section class="panel"><div class="panel-header"><div><h2>Docker Engine</h2><p>Engine-reported capacity and inventory counts, not Host OS utilization.</p></div></div>${definitionList({ "Engine version": engine?.version, Containers: engine ? `${engine.containers_total} total · ${engine.containers_running} running · ${stopped} stopped` : "—", Images: engine?.images, "CPU capacity": engine?.cpu_capacity, "Memory capacity": engine ? formatBytes(engine.memory_capacity_bytes) : "—", "Storage driver": engine?.storage_driver })}</section><section class="panel"><div class="panel-header"><div><h2>Dockpilot management</h2><p>Current session and capability facts.</p></div></div>${Object.entries(
      detail.capabilities || {},
    )
      .map(
        ([name, cap]) =>
          `<div class="capability-row"><span class="primary">${text(name)}</span> · ${cap.enabled ? "Available" : "Unavailable"}<div class="secondary">${text(cap.reason || "")}</div></div>`,
      )
      .join(
        "",
      )}${definitionList({ "Session source IP": detail.session_source_ip, Observed: formatTime(detail.session_observed_at), "Last discovery": formatTime(detail.project_scan?.scanned_at) })}</section></div><section class="panel"><div class="panel-header"><div><h2>Engine details</h2><p>Progressive one-shot Docker Engine inspection; Live Metrics is not opened.</p></div></div>${definitionList({ "Docker Engine version": engine?.version, "Engine API version": detail.docker_api_version, "Docker Compose version": detail.docker_compose_version, "Storage driver": engine?.storage_driver, "Logging driver": engine?.logging_driver, "Cgroup driver / version": engine ? [engine.cgroup_driver, engine.cgroup_version].filter(Boolean).join(" · ") : "", "Default runtime": engine?.default_runtime, "Operating system": engine ? [engine.operating_system, engine.os_version].filter(Boolean).join(" · ") : "", "OS type": engine?.os_type, Architecture: engine?.architecture, Kernel: engine?.kernel_version, "Docker root directory": engine?.docker_root_dir })}</section><section class="panel flush"><div class="panel-header inset"><div><h2>Compose exceptions</h2><p>Only projects with deterministic known exceptions are shown.</p></div><a href="#/hosts/${encodeURIComponent(host.id)}/compose">Open Compose</a></div>${table(
      ["Project", "State", "Configuration"],
      exceptions.map(
        (project) =>
          `<tr><td><a class="primary" href="#/projects/${encodeURIComponent(project.uid)}/summary">${text(project.name)}</a><div class="secondary mono">${text(project.working_dir)}</div></td><td>${projectStatus(project)}</td><td>${text(project.drift || "unknown")}</td></tr>`,
      ),
      "No Compose project exceptions",
    )}</section>`;
    return;
  }
  if (route.tab === "compose") {
    const projects = (state.dashboard?.projects || []).filter(
      (project) => project.agent_id === host.id,
    );
    const rows = projects.map((project) => {
      const observed =
        project.present && !project.stale && project.last_observed_at;
      const containers = observed
        ? String((project.container_ids || []).length)
        : `Last known · ${project.last_observed_at ? formatTime(project.last_observed_at) : "not observed"}`;
      const attention =
        [
          project.restore_recovery_required && "Recovery required",
          project.collision && "Collision",
          project.stale && "Stale",
          !project.present && "Missing",
          project.drift && project.drift !== "in-sync" && project.drift,
        ]
          .filter(Boolean)
          .join(" · ") || "—";
      return `<tr><td><a class="primary" href="#/projects/${encodeURIComponent(project.uid)}/summary">${text(project.name)}</a><div class="secondary mono">${text(project.working_dir)}</div></td><td>${text((project.defined_services || []).length)}</td><td>${text(containers)}</td><td>${text(project.drift || "unknown")}</td><td>${text(attention)}</td></tr>`;
    });
    view.innerHTML = `${pageHeader("Docker Compose", "Compose", `Compose projects discovered on ${host.display_name || host.id}.`, `<button class="primary-button" data-action="project-operation" data-kind="discovery.rescan" data-agent="${text(host.id)}">Rescan projects</button>`)}${hostTabs(host.id, route.tab)}<section class="panel flush">${table(["Project", "Services", "Containers", "Configuration", "Needs attention"], rows, "No Compose projects discovered on this Docker host")}</section>`;
    return;
  }
  if (["containers", "images", "networks", "volumes"].includes(route.tab)) {
    if (!connectionAvailable(host)) {
      view.innerHTML = `${shell()}<div class="notice warning">${text(host.capabilities?.connection?.reason || "Agent is offline")}</div>`;
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
function renderInventory(kind, items, agentID) {
  const headers = {
    containers: [
      "Compose project",
      "Service",
      "Container",
      "State",
      "Health",
      "Image",
      "Published ports",
      "Protection",
    ],
    images: [
      "Repository / tags",
      "Image ID",
      "Created",
      "Virtual size",
      "References",
    ],
    networks: ["Network", "Driver", "Scope", "Flags"],
    volumes: ["Volume", "Driver", "Scope", "Created"],
  }[kind];
  const rows = (items || []).map((item) => {
    if (kind === "containers")
      return `<tr><td>${text(item.compose_project || "—")}</td><td>${text(item.compose_service || "—")}${item.one_off ? `<div class="secondary">One-off</div>` : ""}${item.orphan ? `<div class="secondary">Orphan</div>` : ""}</td><td><a class="row-button" href="#/hosts/${encodeURIComponent(agentID)}/containers?inspect=${encodeURIComponent(item.id)}"><span class="primary">${text((item.names || []).join(", ") || shortID(item.id))}</span><div class="secondary mono">${text(shortID(item.id))}</div></a></td><td>${stateBadge(item.state)}</td><td>${item.health ? stateBadge(item.health) : "—"}</td><td>${text(item.image || "—")}</td><td>${portsCell(item.ports)}</td><td>${item.protected ? `${badge("Protected", "warn")}<div class="secondary">${text(item.protection_reason)}</div>` : "—"}</td></tr>`;
    if (kind === "images")
      return `<tr><td><a class="row-button" href="#/hosts/${encodeURIComponent(agentID)}/images?inspect=${encodeURIComponent(item.id)}"><span class="primary">${text((item.repo_tags || []).join(", ") || "Untagged")}</span><div class="secondary">${text((item.repo_digests || []).join(", ") || "No digest references")}</div></a></td><td class="mono">${text(shortID(item.id))}</td><td>${text(item.created_unix ? formatTime(item.created_unix * 1000) : "—")}</td><td>${text(formatBytes(item.size_bytes))}</td><td>${item.containers === 0 ? "Unused" : item.containers < 0 ? "Unknown" : `Referenced by ${text(item.containers)} Container(s)`}</td></tr>`;
    if (kind === "networks")
      return `<tr><td><a class="row-button" href="#/hosts/${encodeURIComponent(agentID)}/networks?inspect=${encodeURIComponent(item.id)}"><span class="primary">${text(item.name)}</span><div class="secondary mono">${text(shortID(item.id))}</div></a></td><td>${text(item.driver)}</td><td>${text(item.scope)}</td><td>${[item.internal && badge("Internal"), item.attachable && badge("Attachable"), item.ingress && badge("Ingress")].filter(Boolean).join(" ") || "—"}</td></tr>`;
    return `<tr><td><a class="row-button" href="#/hosts/${encodeURIComponent(agentID)}/volumes?inspect=${encodeURIComponent(item.name)}"><span class="primary">${text(item.name)}</span></a></td><td>${text(item.driver)}</td><td>${text(item.scope)}</td><td>${text(item.created_at || "—")}</td></tr>`;
  });
  return `<section class="panel flush"><div class="panel-header inset"><div><h2>${text(kind[0].toUpperCase() + kind.slice(1))}</h2><p>${items.length} current Docker objects. This list is not stored by the Server.</p></div></div>${table(headers, rows, `No ${kind} reported by Docker Engine`)}</section>`;
}

function definitionList(values) {
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
      return `<div class="definition-row"><dt class="muted">${text(key)}</dt><dd>${text(display)}</dd></div>`;
    })
    .join("")}</dl>`;
}

function projectStatus(project) {
  if (project.restore_recovery_required)
    return badge("Recovery required", "bad");
  if (project.collision) return badge("Collision", "bad");
  if (project.stale) return badge("Stale", "warn");
  if (!project.present) return badge("Missing", "warn");
  if (project.read_only) return badge("Read-only", "warn");
  return badge("Managed", "good");
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
  const shell = () =>
    `${pageHeader("Compose project", project.name, project.working_dir, `<span>${projectStatus(project)}</span>${projectAction("Pull", "compose.pull", "quiet-button", !mutable || !(project.pull_services || []).length)}${projectAction("Up", "compose.up", "primary-button", !mutable || !project.project_up_available)}${projectAction("Restart", "compose.restart")}${projectAction("Start", "compose.start")}${projectAction("Stop", "compose.stop")}${projectAction("Down", "compose.down", "danger-button")}`)}${projectTabs(project.uid, route.tab)}`;
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
    const serviceRows = (project.defined_services || []).map(
      (service) =>
        `<tr><td><span class="primary">${text(service.name)}</span><div class="secondary">${service.active ? "Default model" : `Profiles: ${(service.profiles || []).join(", ")}`}</div></td><td>${service.build_required ? badge("Build required", "bad") : service.has_build ? badge("Image + build metadata", "info") : badge("Image-backed", "good")}</td><td>${text(service.image || "—")}</td><td>${service.pull_available && actionState ? `<button class="quiet-button" data-action="project-operation" data-kind="compose.pull" data-project="${text(project.uid)}" data-agent="${text(project.agent_id)}" data-target="${text(service.name)}">Pull</button>` : `<span class="secondary" title="${text(service.unavailable_reason || "Unavailable")}">Unavailable</span>`}</td><td>${service.up_available && actionState ? `<button class="quiet-button" data-action="project-operation" data-kind="compose.up" data-project="${text(project.uid)}" data-agent="${text(project.agent_id)}" data-target="${text(service.name)}">Up</button>` : `<span class="secondary" title="${text(service.unavailable_reason || "Unavailable")}">Unavailable</span>`}</td></tr>`,
    );
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
    view.innerHTML = `${shell()}${project.project_up_available ? "" : `<div class="notice warning"><strong>Project Up unavailable.</strong> ${text(project.project_up_reason || "The effective model requires a build.")}</div>`}
      <div class="split-grid"><section class="panel"><div class="panel-header"><div><h2>Runtime summary</h2><p>${runtime ? `Observed ${text(formatTime(runtime.observed_at))}` : "Current runtime view unavailable"}</p></div></div>${definitionList({ "Defined Services": (project.defined_services || []).length, "Existing Containers": runtime ? ordinary : "—", "Services without Container": runtime ? noContainer : "—", "Profile inactive Services": runtime ? inactive : "—", "One-off Containers": runtime ? oneOff : "—", "Orphan Containers": runtime ? orphans : "—" })}</section><section class="panel"><div class="panel-header"><div><h2>Effective Compose context</h2><p>Content-free authoritative model metadata.</p></div></div>${definitionList({ "Project name": project.name, "Project directory": project.working_dir, "Compose files — merge order": (project.compose_files || []).join(" → "), "Included applications": (project.included_by || []).join(", "), "Active profiles": (project.active_profiles || []).join(", ") || "Default model", "Source graph": project.source_graph_complete ? "Complete" : "Incomplete", Configuration: project.drift, "Last verified": formatTime(project.last_verified_at), Management: project.managed ? "Managed" : "Unmanaged" })}</section></div>
      <section class="panel flush"><div class="panel-header inset"><div><h2>Services and v1 build policy</h2><p>Dockpilot never builds Images and every Up includes <span class="mono">--no-build</span>.</p></div><div class="page-actions"><button class="quiet-button" data-action="project-operation" data-kind="compose.pull" data-project="${text(project.uid)}" data-agent="${text(project.agent_id)}" ${!actionState || !(project.pull_services || []).length ? "disabled" : ""}>Pull Images</button><button class="primary-button" data-action="project-operation" data-kind="compose.up" data-project="${text(project.uid)}" data-agent="${text(project.agent_id)}" ${!actionState || !project.project_up_available ? "disabled" : ""}>Up Project</button></div></div>${table(["Service", "Classification", "Declared Image", "Pull", "Service Up"], serviceRows, "No Services in the effective Compose model")}</section>`;
    return;
  }
  if (route.tab === "containers") {
    const runtime = await jsonRequest(
      `/api/v1/projects/${encodeURIComponent(project.uid)}/runtime`,
      { signal },
    );
    const rows = [];
    for (const service of runtime.services || []) {
      if (!(service.containers || []).length)
        rows.push(
          `<tr><td class="primary">${text(service.name)}</td><td>—</td><td>${stateBadge(service.status)}</td><td>—</td><td>—</td><td>—</td></tr>`,
        );
      for (const container of service.containers || [])
        rows.push(projectContainerRow(project.uid, service.name, container));
    }
    for (const orphan of runtime.orphans || [])
      rows.push(
        projectContainerRow(
          project.uid,
          orphan.compose_service || "Unknown",
          orphan,
        ),
      );
    view.innerHTML = `${shell()}${runtime.observed_at ? `<div class="notice">Container attachments were last qualified at ${text(formatTime(runtime.observed_at))}.</div>` : `<div class="notice warning">Attachment freshness is unavailable.</div>`}<section class="panel flush">${table(["Service", "Container", "State", "Health", "Image", "Published ports"], rows, "No defined Services or observed Containers")}</section>`;
    if (route.inspect) {
      state.inspectorRoute = true;
      inspectContainer(project.agent_id, route.inspect);
    }
    return;
  }
  if (route.tab === "files") {
    view.innerHTML = shell() + renderFileWorkspace(project);
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

function projectContainerRow(projectUID, service, container) {
  const markers = [
    container.one_off && badge("One-off", "info"),
    container.orphan && badge("Orphan", "warn"),
  ]
    .filter(Boolean)
    .join(" ");
  return `<tr><td><span class="primary">${text(service)}</span>${markers ? `<div class="secondary">${markers}</div>` : ""}</td><td><a class="row-button" href="#/projects/${encodeURIComponent(projectUID)}/containers?inspect=${encodeURIComponent(container.id)}"><span class="primary">${text((container.names || []).join(", ") || shortID(container.id))}</span><div class="secondary mono">${text(shortID(container.id))}</div></a></td><td>${stateBadge(container.state)}</td><td>${container.health ? stateBadge(container.health) : "—"}</td><td>${text(container.image)}</td><td>${portsCell(container.ports)}</td></tr>`;
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
    <div class="source-group"><h3>Compose model</h3>${sourceButton("config", "Resolved config (masked)")}</div>
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
        "Resolved configuration may contain sensitive environment values. It remains transient in this browser.",
        "Reveal",
      ))
    )
      return;
    loadSource(project, "config", "Resolved config", true);
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
        ? "Resolved config — revealed"
        : "Resolved config — masked";
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
    showToast(`Write operation ${operation.operation_id} accepted.`);
    $("#save-file").disabled = true;
    state.loadedFile = undefined;
  } catch (error) {
    showToast(error.message);
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
  $("#logs-agent").value = project.agent_id;
  const containers = [
    ...(runtime.services || []).flatMap((service) => service.containers || []),
    ...(runtime.orphans || []),
  ];
  $("#logs-container").insertAdjacentHTML(
    "beforeend",
    containers
      .map(
        (container) =>
          `<option value="${text(container.id)}">${text((container.names || []).join(", ") || shortID(container.id))} · ${text(container.compose_service || "orphan")}</option>`,
      )
      .join(""),
  );
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
    output.textContent = "";
    $("#logs-status").textContent =
      "Browser view cleared. Docker Engine logs were not deleted.";
  });
  $("#logs-find").addEventListener("input", (event) => {
    const query = event.target.value.toLowerCase(),
      content = output.textContent.toLowerCase();
    if (!query) {
      $("#logs-status").textContent = "Find cleared.";
      return;
    }
    let count = 0,
      index = 0;
    while ((index = content.indexOf(query, index)) >= 0) {
      count += 1;
      index += query.length || 1;
    }
    $("#logs-status").textContent =
      `${count} match${count === 1 ? "" : "es"} in the loaded browser view.`;
  });
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
  output.textContent = "";
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

async function renderOperations(signal) {
  breadcrumbs([{ label: "Home", href: "#/home" }, { label: "Operations" }]);
  const page = await jsonRequest("/api/v1/operations?limit=200", { signal });
  state.operationsIndex = page.operations || [];
  view.innerHTML = `${pageHeader("Dockpilot control", "Operations", "Bounded Server index with request context and Agent-authoritative execution facts.")}<section class="panel flush">${operationTable(state.operationsIndex)}</section>`;
}

function renderMetrics(host) {
  view.append($("#metrics-template").content.cloneNode(true));
  if (!host.capabilities?.metrics?.enabled) {
    $("#metrics-status").textContent =
      host.capabilities?.metrics?.reason || "Live Metrics unavailable";
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
    $("#metrics-status").textContent =
      error.name === "AbortError" ? "Metrics stopped." : error.message;
  });
}
function renderMatrix(frame) {
  const stale = [
    [frame.membership_stale, frame.membership_reason],
    [frame.workload_stale, frame.workload_reason],
    [frame.context_stale, frame.context_reason],
  ]
    .filter(([value]) => value)
    .map(([, reason]) => reason);
  $("#metrics-status").textContent = stale.length
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
  const memory = values.memory_percent_known
    ? `${Number(values.memory_percent || 0).toFixed(1)}% · ${formatBytes(values.memory_usage || values.sample?.memory_usage)}`
    : values.memory_limit_unbounded
      ? `Unbounded · ${formatBytes(values.memory_usage || values.sample?.memory_usage)}`
      : formatBytes(values.memory_usage || values.sample?.memory_usage);
  const source = values.sample || values;
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
  if (!frame) return;
  $("#metrics-hierarchy").classList.toggle(
    "active",
    state.metricsMode === "hierarchy",
  );
  $("#metrics-top").classList.toggle("active", state.metricsMode === "top");
  const rows = [];
  if (state.metricsMode === "hierarchy") {
    rows.push(
      matrixRow(
        "Docker workload",
        0,
        frame.host?.totals || {},
        false,
        "Managed Container aggregate; shared network namespaces may be double-counted",
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
  $("#metrics-table").innerHTML = table(
    [
      "Name",
      "CPU",
      "Memory",
      "Network RX / TX cumulative",
      "Block R / W cumulative",
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
  output.textContent += prefix + decodeLogData(event.data);
  if (output.textContent.length > MAX_LOG_CHARACTERS)
    output.textContent = `[older browser output removed]\n${output.textContent.slice(-MAX_LOG_CHARACTERS)}`;
  if (output.dataset.autoFollow !== "false")
    output.scrollTop = output.scrollHeight;
}

function newOperationID(prefix) {
  return `${prefix}-${crypto.randomUUID()}`;
}
async function startProjectOperation(button) {
  const kind = button.dataset.kind,
    target = button.dataset.target || "",
    project = button.dataset.project,
    agent = button.dataset.agent;
  const destructive = kind === "compose.down";
  if (
    destructive &&
    !(await confirmAction(
      "Run Compose Down?",
      "Service Containers and Compose-created Networks will be removed. Named Volumes and external Networks or Volumes will be retained.",
      "Down",
    ))
  )
    return;
  if (
    kind === "compose.restart" &&
    !(await confirmAction(
      "Restart existing Containers?",
      "Restart does not apply Compose configuration or environment changes. Use Up to apply configuration.",
      "Restart",
    ))
  )
    return;
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
    showToast(`${kind} operation ${operation.operation_id} accepted.`);
  } catch (error) {
    showToast(error.message);
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
    showToast(`Cancellation outcome: ${result.outcome}`);
    renderRoute();
  } catch (error) {
    showToast(error.message);
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
    showToast(`Restore operation ${operation.operation_id} accepted.`);
  } catch (error) {
    showToast(error.message);
  }
}

function openInspectorHTML(title, html) {
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
  openInspectorHTML(
    operation.kind || "Operation",
    notice +
      definitionList({
        "Operation ID": operation.operation_id,
        Agent: operation.agent_id,
        Project: operation.project_uid,
        Target: operation.target,
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
        showToast(
          `Showing last synchronized operation detail: ${error.message}`,
        );
    }
  } catch (error) {
    if (inspectorRequestCurrent(request) && error.name !== "AbortError")
      showToast(error.message);
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
    ["Container", "State", "Compose context", "Destination"],
    (values || []).map(
      (item) =>
        `<tr><td><span class="primary">${text(item.container_name || shortID(item.container_id))}</span><div class="secondary mono">${text(shortID(item.container_id))}</div></td><td>${item.state ? stateBadge(item.state) : "—"}</td><td>${text(item.compose_project || "—")}<div class="secondary">${text(item.compose_service || "")}</div></td><td class="mono">${text(item.destination || "—")}</td></tr>`,
    ),
    emptyMessage,
  );
}
function networkAttachmentDetails(values) {
  return table(
    ["Container", "Compose context", "Docker addresses", "MAC"],
    (values || []).map(
      (item) =>
        `<tr><td><span class="primary">${text(item.container_name || shortID(item.container_id))}</span><div class="secondary mono">${text(shortID(item.container_id))}</div></td><td>${text(item.compose_project || "—")}<div class="secondary">${text(item.compose_service || "")}</div></td><td class="mono">${text([item.ipv4, item.ipv6].filter(Boolean).join(" · ") || "—")}</td><td class="mono">${text(item.mac || "—")}</td></tr>`,
    ),
    "No attached Containers",
  );
}
async function inspectContainer(agentID, id) {
  const request = beginInspectorRequest();
  openInspector("Container", {
    "Container ID": id,
    Status: "Loading current Docker details…",
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
      "No network attachments",
    );
    openInspectorHTML(
      (item.names || []).join(", ") || "Container",
      definitionList({
        "Container ID": item.id,
        Image: item.image,
        "Image ID": item.image_id,
        State: item.state,
        Status: item.status,
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
        "Image exposed ports": (item.exposed_ports || []).join(", "),
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
    Status: "Loading current Docker details…",
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
        "Virtual size": formatBytes(item.size_bytes),
        Layers: item.layer_count,
        User: item.user,
        "Working directory": item.working_dir,
        Entrypoint: (item.entrypoint || []).join(" "),
        Command: (item.command || []).join(" "),
        "Exposed ports": (item.exposed_ports || []).join(", "),
      }) +
        inspectorSection(
          "Container usage",
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
    Status: "Loading current Docker details…",
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
          "Container attachments",
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
    Status: "Loading current Docker details…",
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
          "Container references",
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
      resolve(dialog.returnValue === "confirm");
    };
    dialog.addEventListener("close", listener);
    dialog.showModal();
  });
}

async function renderRoute() {
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
  showLoading();
  try {
    await loadDashboard();
    if (route.kind === "home") await renderHome(state.routeController.signal);
    else if (route.kind === "search") renderSearch();
    else if (route.kind === "host")
      await renderHost(route, state.routeController.signal);
    else if (route.kind === "project")
      await renderProject(route, state.routeController.signal);
    else await renderOperations(state.routeController.signal);
    if (state.routeKey === route.key) $("#main").focus({ preventScroll: true });
  } catch (error) {
    if (error.name !== "AbortError") showError(error);
  }
}

$("#view").addEventListener("click", (event) => {
  const button = event.target.closest("[data-action]");
  if (!button) return;
  const actions = {
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
$("#refresh").addEventListener("click", async () => {
  state.dashboard = undefined;
  showLoading("Refreshing Dockpilot");
  try {
    await loadDashboard(true);
    renderRoute();
  } catch (error) {
    showError(error);
  }
});
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
window.addEventListener("beforeunload", () => {
  state.routeController?.abort();
  state.streamController?.abort();
});

loadDashboard().then(renderRoute).catch(showError);
