import {
  DESTRUCTIVE_ACTIONS,
  PROJECT_ACTIONS,
  SERVICE_ACTIONS,
  actionLabel,
  hostForProject,
  isOperationTerminal,
  operationTone,
  projectActionAvailability,
  projectMutation,
  serviceActionAvailability,
  serviceRuntime,
  serviceRuntimeLabel,
  sortProjects,
} from "./domain.js";

const SETTINGS_KEY = "docklattice.widget.connection.v1";
const REFRESH_KEY = "docklattice.widget.refresh.v1";
const POLL_INTERVAL_MS = 1_000;
const MAX_POLL_TIME_MS = 10 * 60 * 1_000;

const elements = {
  caPem: document.querySelector("#ca-pem"),
  cancelSettings: document.querySelector("#cancel-settings"),
  closeWindow: document.querySelector("#close-window"),
  confirmAction: document.querySelector("#confirm-action"),
  confirmDialog: document.querySelector("#confirm-dialog"),
  confirmMessage: document.querySelector("#confirm-message"),
  confirmTitle: document.querySelector("#confirm-title"),
  connectionLabel: document.querySelector("#connection-label"),
  connectionState: document.querySelector(".connection-state"),
  emptyMessage: document.querySelector("#empty-state span"),
  emptyState: document.querySelector("#empty-state"),
  emptyTitle: document.querySelector("#empty-state strong"),
  loadingState: document.querySelector("#loading-state"),
  minimizeWindow: document.querySelector("#minimize-window"),
  observedLabel: document.querySelector("#observed-label"),
  projectList: document.querySelector("#project-list"),
  projectsView: document.querySelector("#projects-view"),
  refreshButton: document.querySelector("#refresh-button"),
  refreshInterval: document.querySelector("#refresh-interval"),
  serverUrl: document.querySelector("#server-url"),
  settingsButton: document.querySelector("#settings-button"),
  settingsForm: document.querySelector("#settings-form"),
  settingsView: document.querySelector("#settings-view"),
  notice: document.querySelector("#notice"),
  toastRegion: document.querySelector("#toast-region"),
  windowDragRegion: document.querySelector("#window-drag-region"),
};

const state = {
  connection: undefined,
  dashboard: undefined,
  runtimes: new Map(),
  expandedProjects: new Set(),
  refreshTimer: undefined,
  refreshing: false,
};

function invoke(command, payload) {
  const tauriInvoke = window.__TAURI__?.core?.invoke;
  if (!tauriInvoke) {
    return Promise.reject(
      new Error("The desktop bridge is unavailable. Run this page with Tauri."),
    );
  }
  return tauriInvoke(command, payload);
}

function currentWindow() {
  return window.__TAURI__?.window?.getCurrentWindow?.();
}

function runWindowAction(action) {
  const appWindow = currentWindow();
  if (!appWindow || typeof appWindow[action] !== "function") return;
  appWindow[action]().catch((error) => {
    showToast("error", "Window action failed", String(error));
  });
}

function readConnection() {
  try {
    const value = JSON.parse(localStorage.getItem(SETTINGS_KEY));
    if (value?.serverUrl) return value;
  } catch (_) {
    // A missing or malformed local preference is equivalent to first run.
  }
  return undefined;
}

function saveConnection(connection) {
  localStorage.setItem(SETTINGS_KEY, JSON.stringify(connection));
}

function showSettings() {
  elements.serverUrl.value =
    state.connection?.serverUrl || "https://127.0.0.1:8080";
  elements.caPem.value = state.connection?.caPem || "";
  elements.projectsView.hidden = true;
  elements.settingsView.hidden = false;
  elements.serverUrl.focus();
}

function hideSettings() {
  elements.settingsView.hidden = true;
  elements.projectsView.hidden = false;
}

function showNotice(message) {
  elements.notice.textContent = message;
  elements.notice.hidden = !message;
}

function setConnectionState(label, condition) {
  elements.connectionLabel.textContent = label;
  elements.connectionState.dataset.state = condition;
  try {
    Promise.resolve(invoke("set_tray_status", { status: condition })).catch(
      () => {
        // The window status remains authoritative if a tray update fails.
      },
    );
  } catch (_) {
    // A missing desktop tray must not interrupt the primary interface.
  }
}

function setRefreshing(refreshing) {
  state.refreshing = refreshing;
  elements.refreshButton.disabled = refreshing || !state.connection;
  elements.refreshButton.classList.toggle("loading", refreshing);
  elements.projectsView.setAttribute("aria-busy", String(refreshing));
  elements.loadingState.hidden = !refreshing || Boolean(state.dashboard);
  if (refreshing && !state.dashboard) elements.emptyState.hidden = true;
}

function projectCondition(project, host) {
  const mutation = projectMutation(project, host);
  if (project.restore_recovery_required) return ["Recovery required", true];
  if (project.collision) return ["Collision", true];
  if (project.stale) return ["Stale", true];
  if (!project.present) return ["Missing", true];
  if (project.read_only) return ["Read-only", true];
  if (!mutation.available) return ["Unavailable", true];
  return ["Managed", false];
}

function createButton(kind, availability, project, target = "") {
  const button = document.createElement("button");
  button.className = "action-button";
  button.type = "button";
  button.dataset.kind = kind;
  button.dataset.project = project.uid;
  button.dataset.agent = project.agent_id;
  if (target) button.dataset.target = target;
  button.textContent = actionLabel(kind);
  button.disabled = !availability.available;
  button.title = availability.available ? "" : availability.reason;
  return button;
}

function createActionGroup(kinds, project, host, target = "", runtime) {
  const group = document.createElement("div");
  group.className = "action-group";
  group.setAttribute(
    "aria-label",
    target ? `${target} actions` : `${project.name || project.uid} actions`,
  );
  for (const kind of kinds) {
    const service = target
      ? (project.defined_services || []).find((item) => item.name === target)
      : undefined;
    const availability = service
      ? serviceActionAvailability(project, host, service, runtime, kind)
      : projectActionAvailability(project, host, kind);
    group.append(createButton(kind, availability, project, target));
  }
  return group;
}

function createServiceRow(project, host, service, runtime) {
  const row = document.createElement("div");
  row.className = "service-row";

  const observed = serviceRuntime(runtime, service.name);
  const status = serviceRuntimeLabel(observed);
  const heading = document.createElement("div");
  heading.className = "service-heading";
  const dot = document.createElement("span");
  dot.className = `state-dot ${status.toLowerCase().replaceAll(" ", "-")}`;
  dot.setAttribute("aria-hidden", "true");
  const title = document.createElement("div");
  title.className = "project-title";
  const name = document.createElement("h3");
  name.textContent = service.name;
  const meta = document.createElement("p");
  meta.className = "service-meta";
  meta.textContent = status;
  title.append(name, meta);
  heading.append(dot, title);

  const actions = createActionGroup(
    SERVICE_ACTIONS,
    project,
    host,
    service.name,
    runtime,
  );
  actions.className = "service-actions";
  row.append(heading, actions);
  return row;
}

function createServices(project, host) {
  const section = document.createElement("div");
  section.className = "services";
  const toggle = document.createElement("button");
  toggle.className = "services-toggle";
  toggle.type = "button";
  toggle.dataset.expandProject = project.uid;
  const expanded = state.expandedProjects.has(project.uid);
  toggle.setAttribute("aria-expanded", String(expanded));
  const serviceCount = (project.defined_services || []).length;
  toggle.textContent = `${expanded ? "▾" : "▸"} ${serviceCount} service${serviceCount === 1 ? "" : "s"}`;
  section.append(toggle);

  if (!expanded) return section;
  const list = document.createElement("div");
  list.className = "service-list";
  const runtime = state.runtimes.get(project.uid);
  if (!runtime) {
    const loading = document.createElement("div");
    loading.className = "service-row service-meta";
    loading.textContent = "Loading current Container state…";
    list.append(loading);
  } else {
    for (const service of project.defined_services || []) {
      list.append(createServiceRow(project, host, service, runtime));
    }
  }
  section.append(list);
  return section;
}

function createProjectCard(project) {
  const host = hostForProject(state.dashboard, project);
  const [condition, warning] = projectCondition(project, host);
  const card = document.createElement("article");
  card.className = `project-card${warning ? " unavailable" : ""}`;

  const header = document.createElement("div");
  header.className = "project-header";
  const heading = document.createElement("div");
  heading.className = "project-heading";
  const title = document.createElement("div");
  title.className = "project-title";
  const name = document.createElement("h3");
  name.textContent = project.name || project.uid;
  const path = document.createElement("p");
  path.className = "project-path";
  path.textContent = `${host?.display_name || project.agent_id} · ${project.working_dir}`;
  path.title = path.textContent;
  title.append(name, path);
  const badge = document.createElement("span");
  badge.className = `condition${warning ? " warning" : ""}`;
  badge.textContent = condition;
  heading.append(title, badge);

  const actions = document.createElement("div");
  actions.className = "project-actions";
  actions.append(
    createActionGroup(PROJECT_ACTIONS.slice(0, 3), project, host),
    createActionGroup(PROJECT_ACTIONS.slice(3), project, host),
  );
  header.append(heading, actions);
  card.append(header, createServices(project, host));
  return card;
}

function renderProjects() {
  elements.projectList.replaceChildren();
  const projects = sortProjects(state.dashboard?.projects);
  for (const project of projects) {
    elements.projectList.append(createProjectCard(project));
  }
  elements.emptyState.hidden = projects.length > 0;
  if (!projects.length) {
    elements.emptyTitle.textContent = "No Compose projects";
    elements.emptyMessage.textContent =
      "The connected Server has not discovered any projects.";
  }
  elements.observedLabel.textContent = projects.length
    ? `${projects.length} project${projects.length === 1 ? "" : "s"} · ${state.dashboard.hosts?.length || 0} host${state.dashboard.hosts?.length === 1 ? "" : "s"} · refreshed now`
    : "No Compose projects were discovered.";
}

async function refresh({ quiet = false } = {}) {
  if (!state.connection || state.refreshing) return;
  setRefreshing(true);
  if (!state.dashboard) setConnectionState("Connecting", "connecting");
  if (!quiet) showNotice("");
  try {
    const dashboard = await invoke("dashboard", {
      connection: state.connection,
    });
    state.dashboard = dashboard;
    setConnectionState(new URL(state.connection.serverUrl).host, "connected");
    showNotice("");
    renderProjects();

    const expanded = [...state.expandedProjects];
    await Promise.allSettled(expanded.map(loadRuntime));
    renderProjects();
  } catch (error) {
    setConnectionState("Connection unavailable", "error");
    showNotice(String(error));
    if (!state.dashboard) {
      elements.emptyTitle.textContent = "Project data unavailable";
      elements.emptyMessage.textContent =
        "Review the Server URL and certificate, then try again.";
      elements.emptyState.hidden = false;
    }
  } finally {
    setRefreshing(false);
  }
}

async function loadRuntime(projectUid) {
  const runtime = await invoke("project_runtime", {
    connection: state.connection,
    projectUid,
  });
  state.runtimes.set(projectUid, runtime);
}

function operationContext(project, target) {
  return target || project?.name || project?.uid || "Compose project";
}

function showToast(tone, title, detail = "") {
  const toast = document.createElement("div");
  toast.className = `toast ${tone}`;
  const content = document.createElement("div");
  const heading = document.createElement("strong");
  heading.textContent = title;
  const message = document.createElement("span");
  message.textContent = detail;
  content.append(heading, message);
  toast.append(content);
  elements.toastRegion.append(toast);
  window.setTimeout(() => toast.remove(), 7_000);
  return { heading, message, toast };
}

function confirmOperation(kind, context) {
  const label = actionLabel(kind);
  elements.confirmTitle.textContent = `Confirm ${label}`;
  elements.confirmMessage.textContent = `${label} ${context}? This action changes the Compose project.`;
  elements.confirmAction.textContent = label;
  elements.confirmDialog.showModal();
  return new Promise((resolve) => {
    elements.confirmDialog.addEventListener(
      "close",
      () => resolve(elements.confirmDialog.returnValue === "confirm"),
      { once: true },
    );
  });
}

function delay(milliseconds) {
  return new Promise((resolve) => window.setTimeout(resolve, milliseconds));
}

async function trackOperation(initial, context) {
  const label = actionLabel(initial.kind);
  const toast = showToast("info", `${label} started`, context);
  let operation = initial;
  let trackingFailures = 0;
  const deadline = Date.now() + MAX_POLL_TIME_MS;

  while (!isOperationTerminal(operation) && Date.now() < deadline) {
    await delay(POLL_INTERVAL_MS);
    try {
      operation = await invoke("operation", {
        connection: state.connection,
        agentId: initial.agent_id,
        operationId: initial.operation_id,
      });
      trackingFailures = 0;
    } catch (error) {
      trackingFailures += 1;
      if (trackingFailures < 3) continue;
      toast.toast.className = "toast warning";
      toast.heading.textContent = `${label} status unavailable`;
      toast.message.textContent = `${context} · ${String(error)}`;
      return;
    }
  }

  const status = String(operation.status || "running").toLowerCase();
  toast.toast.className = `toast ${operationTone(operation)}`;
  toast.heading.textContent = isOperationTerminal(operation)
    ? `${label} ${status}`
    : `${label} still running`;
  toast.message.textContent =
    operation.error ||
    (operation.partial_effects_possible
      ? `${context} · Partial effects may be possible.`
      : context);
  await refresh({ quiet: true });
}

async function runOperation(button) {
  const kind = button.dataset.kind;
  const project = (state.dashboard?.projects || []).find(
    (item) => item.uid === button.dataset.project,
  );
  if (!project) return;
  const target = button.dataset.target || "";
  const context = operationContext(project, target);
  if (DESTRUCTIVE_ACTIONS.has(kind)) {
    const confirmed = await confirmOperation(kind, context);
    if (!confirmed) return;
  }

  button.disabled = true;
  try {
    const operation = await invoke("start_operation", {
      connection: state.connection,
      agentId: project.agent_id,
      projectUid: project.uid,
      kind,
      target: target || null,
    });
    await trackOperation(operation, context);
  } catch (error) {
    showToast("error", `${actionLabel(kind)} failed`, String(error));
  } finally {
    button.disabled = false;
  }
}

async function toggleProject(projectUid) {
  if (state.expandedProjects.has(projectUid)) {
    state.expandedProjects.delete(projectUid);
    renderProjects();
    return;
  }
  state.expandedProjects.add(projectUid);
  renderProjects();
  try {
    await loadRuntime(projectUid);
  } catch (error) {
    showToast("warning", "Container state unavailable", String(error));
  }
  renderProjects();
}

function scheduleRefresh() {
  window.clearInterval(state.refreshTimer);
  const seconds = Number(elements.refreshInterval.value);
  localStorage.setItem(REFRESH_KEY, String(seconds));
  if (seconds > 0) {
    state.refreshTimer = window.setInterval(
      () => refresh({ quiet: true }),
      seconds * 1_000,
    );
  }
}

elements.settingsButton.addEventListener("click", showSettings);
elements.minimizeWindow.addEventListener("click", () => {
  runWindowAction("minimize");
});
elements.closeWindow.addEventListener("click", () => {
  runWindowAction("close");
});
elements.windowDragRegion.addEventListener("pointerdown", (event) => {
  if (event.button !== 0 || event.target.closest("button, select")) return;
  runWindowAction("startDragging");
});
elements.cancelSettings.addEventListener("click", hideSettings);
elements.refreshButton.addEventListener("click", () => refresh());
elements.refreshInterval.addEventListener("change", scheduleRefresh);
elements.settingsForm.addEventListener("submit", async (event) => {
  event.preventDefault();
  state.connection = {
    serverUrl: elements.serverUrl.value.trim(),
    caPem: elements.caPem.value.trim(),
  };
  saveConnection(state.connection);
  hideSettings();
  await refresh();
});
elements.projectList.addEventListener("click", (event) => {
  const action = event.target.closest("[data-kind]");
  if (action) {
    runOperation(action);
    return;
  }
  const toggle = event.target.closest("[data-expand-project]");
  if (toggle) toggleProject(toggle.dataset.expandProject);
});

const storedInterval = localStorage.getItem(REFRESH_KEY);
if (["0", "15", "30", "60"].includes(storedInterval)) {
  elements.refreshInterval.value = storedInterval;
}
scheduleRefresh();
state.connection = readConnection();
if (state.connection) {
  hideSettings();
  refresh();
} else {
  setConnectionState("Not connected", "disconnected");
  showSettings();
}
