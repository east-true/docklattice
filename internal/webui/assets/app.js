"use strict";

const escapeText = value => String(value ?? "");
const MAX_STATS_SAMPLES = 120;
const MAX_LOG_CHARACTERS = 262144;
const MAX_SSE_BUFFER = 2097152;
const MAX_RENDERED_INVENTORY_ITEMS = 500;
const MAX_RENDERED_AUDIT_EVENTS = 500;
const utf8Decoder = new TextDecoder();
let logsController;
let projectLogsController;
let statsController;
let statsHistory = [];
let loadedFile;
let inventoryController;
const projectStates = new Map();
const hostStates = new Map();
const historyViews = {
  audit: {cursor: undefined, scope: "", controller: undefined},
  activity: {cursor: undefined, scope: "", controller: undefined},
};

function newOperationID(prefix) {
  return `${prefix}-${crypto.randomUUID()}`;
}

async function jsonRequest(url, options = {}) {
  const response = await fetch(url, {
    cache: "no-store",
    headers: {Accept: "application/json", ...(options.body ? {"Content-Type": "application/json"} : {})},
    ...options,
  });
  const value = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(value.message || `Request failed (${response.status})`);
  return value;
}

function capabilityList(capabilities) {
  const list = document.createElement("ul");
  list.className = "caps";
  for (const [name, value] of Object.entries(capabilities)) {
    const item = document.createElement("li");
    const reason = value.reason || "";
    const warning = value.enabled && reason;
    item.className = `cap${value.enabled ? "" : " off"}${warning ? " warning" : ""}`;
    item.textContent = `${name}: ${value.enabled ? "ready" : "unavailable"}${reason ? ` — ${reason}` : ""}`;
    if (reason) item.title = reason;
    list.append(item);
  }
  return list;
}

function card(title, details) {
  const node = document.createElement("article");
  node.className = "card";
  const heading = document.createElement("h3");
  heading.textContent = escapeText(title);
  const paragraph = document.createElement("p");
  paragraph.textContent = escapeText(details);
  node.append(heading, paragraph);
  return node;
}

function capabilityFor(projectUID, name, mutation = false) {
  const state = projectStates.get(projectUID);
  if (!state) return {enabled: false, reason: "Choose a discovered project"};
  if (mutation && state.project.read_only) return {enabled: false, reason: "Project is read-only"};
  const connection = state.capabilities.connection || {};
  if (!connection.enabled) return {enabled: false, reason: connection.reason || "Agent is offline"};
  return state.capabilities[name] || {enabled: false, reason: `${name} capability unavailable`};
}

function applyCapability(button, capability, additionallyEnabled = true) {
  button.disabled = !capability.enabled || !additionallyEnabled;
  button.title = capability.reason || (capability.enabled ? "" : "Capability unavailable");
}

function updateProjectControls() {
  const fileProject = document.querySelector("#file-project").value;
  const fileRead = capabilityFor(fileProject, "fs_read");
  applyCapability(document.querySelector("#file-load"), fileRead);
  applyCapability(document.querySelector("#file-reveal"), fileRead);
  const fileWrite = capabilityFor(fileProject, "fs_write", true);
  const editableLoaded = Boolean(loadedFile?.revealed && loadedFile.projectUID === fileProject && loadedFile.relativePath === document.querySelector("#file-path").value);
  applyCapability(document.querySelector("#file-save"), fileWrite, editableLoaded);

  applyCapability(document.querySelector("#backup-list-button"), capabilityFor(document.querySelector("#backup-project").value, "connection"));
  applyCapability(document.querySelector("#backup-create-button"), capabilityFor(document.querySelector("#backup-create-project").value, "fs_read"));
  applyCapability(document.querySelector("#backup-restore-button"), capabilityFor(document.querySelector("#backup-restore-project").value, "fs_write", true));

  applyCapability(document.querySelector("#project-logs-start"), capabilityForProjectComposeLogs(document.querySelector("#project-logs-project").value));
}

function capabilityForProjectComposeLogs(projectUID) {
  const state = projectStates.get(projectUID);
  if (!state) return {enabled: false, reason: "Choose a discovered project"};
  if (state.project.present === false || state.project.stale || !state.project.compose_executable) {
    return {enabled: false, reason: state.project.capability_reason || "Compose logs are unavailable for this project"};
  }
  return capabilityFor(projectUID, "compose");
}

function capabilityForHost(agentID) {
  const host = hostStates.get(agentID);
  if (!host) return {enabled: false, reason: "Choose a known host"};
  const connection = host.capabilities?.connection || {};
  if (!connection.enabled) return {enabled: false, reason: connection.reason || "Agent is offline"};
  return host.capabilities?.docker || {enabled: false, reason: "Docker capability unavailable"};
}

function updateInventoryControl() {
  applyCapability(document.querySelector("#inventory-load"), capabilityForHost(document.querySelector("#inventory-agent").value));
}

function inventoryStatus(resource, message, tooltip = "") {
  const status = document.querySelector(`#${resource}-status`);
  status.textContent = message;
  status.title = tooltip;
}

function resetInventory(message = "Not loaded.") {
  for (const resource of ["containers", "images", "networks", "volumes"]) {
    document.querySelector(`#${resource}-list`).replaceChildren();
    inventoryStatus(resource, message);
  }
}

function renderedInventory(items, render) {
  return items.slice(0, MAX_RENDERED_INVENTORY_ITEMS).map(render);
}

const inventoryRenderers = {
  containers: item => card((item.names || []).join(", ") || item.id, `${item.image} · ${item.state} · ${item.status || "status unavailable"}`),
  images: item => card((item.repo_tags || []).join(", ") || item.id, `${item.size_bytes} bytes · ${item.containers} containers · created ${item.created_unix}`),
  networks: item => card(item.name, `${item.driver} · ${item.scope} · ${[item.internal && "internal", item.attachable && "attachable", item.ingress && "ingress"].filter(Boolean).join(", ") || "standard"}`),
  volumes: item => card(item.name, `${item.driver} · ${item.scope}${item.created_at ? ` · ${item.created_at}` : ""}`),
};

async function loadInventoryResource(agentID, resource, signal) {
  const output = document.querySelector(`#${resource}-list`);
  output.replaceChildren();
  inventoryStatus(resource, "Loading live data…");
  try {
    const items = await jsonRequest(`/api/v1/hosts/${encodeURIComponent(agentID)}/${resource}`, {signal});
    output.replaceChildren(...renderedInventory(items, inventoryRenderers[resource]));
    const suffix = items.length > MAX_RENDERED_INVENTORY_ITEMS ? ` Showing the first ${MAX_RENDERED_INVENTORY_ITEMS}.` : "";
    inventoryStatus(resource, `${items.length} live ${resource} loaded.${suffix}`);
  } catch (error) {
    output.replaceChildren();
    if (error.name === "AbortError") {
      inventoryStatus(resource, "Request stopped.");
      return;
    }
    inventoryStatus(resource, "Unavailable — no inventory shown.", error.message);
  }
}

function updateHistoryControls() {
  const hostChosen = Boolean(document.querySelector("#audit-agent").value);
  const projectChosen = Boolean(document.querySelector("#activity-project").value);
  document.querySelector("#audit-load").disabled = !hostChosen;
  document.querySelector("#audit-load").title = hostChosen ? "Stored history is available even when this Agent is offline" : "Choose a host";
  document.querySelector("#activity-load").disabled = !projectChosen;
  document.querySelector("#activity-load").title = projectChosen ? "Stored history is available even when the owning Agent is offline" : "Choose a project";
  document.querySelector("#audit-next").disabled = !hostChosen || !historyViews.audit.cursor;
  document.querySelector("#activity-next").disabled = !projectChosen || !historyViews.activity.cursor;
}

function resetHistory(view, message) {
  historyViews[view].controller?.abort();
  historyViews[view].cursor = undefined;
  historyViews[view].scope = "";
  document.querySelector(`#${view}-list`).replaceChildren();
  document.querySelector(`#${view}-status`).textContent = message;
  const coverage = document.querySelector(`#${view}-coverage`);
  coverage.textContent = "Coverage not loaded.";
  coverage.classList.remove("warning");
  coverage.title = "";
  updateHistoryControls();
}

function renderCoverage(view, coverage = {}) {
  const node = document.querySelector(`#${view}-coverage`);
  const warnings = [];
  if (!coverage.established) warnings.push("Agent-wide audit coverage has not been established; the Server has no continuity evidence for this Agent stream.");
  if ((coverage.unknown_incarnations || []).length) warnings.push(`Coverage is unknown for incarnation(s) ${(coverage.unknown_incarnations || []).join(", ")}.`);
  if ((coverage.gaps || []).length || coverage.effective_gap_records) warnings.push(`${(coverage.gaps || []).length} effective unavailable interval(s) cover ${coverage.effective_gap_records || 0} same-incarnation record position(s).`);
  if (coverage.coverage_entries_truncated) warnings.push("The bounded coverage summary is truncated.");
  if (coverage.ack_blocked_while_ingesting) warnings.push(`ACK is blocked while ingesting (${coverage.ack_blocked_while_ingesting_seconds || 0}s beyond the warning threshold).`);
  if (warnings.length) {
    node.textContent = warnings.join(" ");
    node.classList.add("warning");
    node.title = warnings.join(" ");
    return;
  }
  const start = coverage.start ? `${coverage.start.cursor.incarnation}:${coverage.start.cursor.seq} (${coverage.start.reason})` : "not established";
  const ack = coverage.ack ? `${coverage.ack.incarnation}:${coverage.ack.seq}` : "not yet recorded";
  node.textContent = `Agent-wide coverage start ${start}; ACK ${ack}; no effective gaps reported.`;
  node.classList.remove("warning");
  node.title = "";
}

function auditEventCard(event) {
  const resource = [event.resource_type, event.resource_id].filter(Boolean).join(" ");
  const facts = [event.kind, resource, event.occurred_at, event.count > 1 && `count ${event.count}`, event.actor && `actor ${event.actor}`, event.operation_id && `operation ${event.operation_id}`].filter(Boolean);
  if (event.continuity_reason) facts.push(`continuity ${event.continuity_reason}`);
  return card(event.action, facts.join(" · "));
}

function appendBoundedAuditEvents(view, events, append) {
  const output = document.querySelector(`#${view}-list`);
  const nodes = append ? [...output.children, ...events.map(auditEventCard)] : events.map(auditEventCard);
  output.replaceChildren(...nodes.slice(-MAX_RENDERED_AUDIT_EVENTS));
}

async function loadHistoryPage(view, append) {
  const isHost = view === "audit";
  const scope = document.querySelector(isHost ? "#audit-agent" : "#activity-project").value;
  const state = historyViews[view];
  if (!append || state.scope !== scope) state.cursor = undefined;
  state.controller?.abort();
  state.controller = new AbortController();
  state.scope = scope;
  const status = document.querySelector(`#${view}-status`);
  status.textContent = "Loading canonical history…";
  status.title = "";
  const base = isHost ? `/api/v1/hosts/${encodeURIComponent(scope)}/audit` : `/api/v1/projects/${encodeURIComponent(scope)}/activity`;
  const query = new URLSearchParams({limit: "100"});
  if (append && state.cursor) query.set("cursor", `${state.cursor.incarnation}:${state.cursor.seq}`);
  try {
    const page = await jsonRequest(`${base}?${query}`, {signal: state.controller.signal});
    appendBoundedAuditEvents(view, page.events || [], append);
    renderCoverage(view, page.coverage);
    state.cursor = page.next_cursor;
    const retained = document.querySelector(`#${view}-list`).children.length;
    status.textContent = `${(page.events || []).length} event(s) loaded; ${retained} retained in this bounded browser view.`;
    status.title = "";
  } catch (error) {
    if (error.name === "AbortError") return;
    document.querySelector(`#${view}-list`).replaceChildren();
    state.cursor = undefined;
    status.textContent = "History unavailable — no events shown.";
    status.title = error.message;
    const coverage = document.querySelector(`#${view}-coverage`);
    coverage.textContent = "Coverage unavailable.";
    coverage.classList.add("warning");
    coverage.title = error.message;
  } finally {
    updateHistoryControls();
  }
}

async function load() {
  try {
    const response = await fetch("/api/v1/dashboard", {headers:{Accept:"application/json"}, cache:"no-store"});
    if (!response.ok) throw new Error(`Dashboard request failed (${response.status})`);
    const data = await response.json();
    const hosts = document.querySelector("#hosts");
    const projects = document.querySelector("#projects");
	const hostsByID = new Map();
	const hostOptions = [];
    for (const host of data.hosts || []) {
	  hostsByID.set(host.id, host);
	  hostStates.set(host.id, host);
      const node = card(host.display_name || host.id, host.state);
      node.append(capabilityList(host.capabilities || {}));
	  const inspect = document.createElement("button");
	  inspect.type = "button";
	  inspect.textContent = "View live inventory";
	  applyCapability(inspect, capabilityForHost(host.id));
	  inspect.addEventListener("click", () => {
	    document.querySelector("#inventory-agent").value = host.id;
	    updateInventoryControl();
	    resetInventory();
	    document.querySelector("#inventory").scrollIntoView();
	  });
	  node.append(inspect);
	  const inspectAudit = document.createElement("button");
	  inspectAudit.type = "button";
	  inspectAudit.textContent = "View stored audit";
	  inspectAudit.title = "Stored canonical history remains available while the Agent is offline";
	  inspectAudit.addEventListener("click", () => {
	    document.querySelector("#audit-agent").value = host.id;
	    resetHistory("audit", "No audit page loaded.");
	    document.querySelector("#audit").scrollIntoView();
	  });
	  node.append(inspectAudit);
      hosts.append(node);
	  const hostOption = document.createElement("option");
	  hostOption.value = host.id;
	  hostOption.label = host.display_name || host.id;
	  hostOptions.push(hostOption);
    }
	document.querySelector("#agent-ids").replaceChildren(...hostOptions);
	if (!document.querySelector("#inventory-agent").value && hostOptions.length) document.querySelector("#inventory-agent").value = hostOptions[0].value;
	updateInventoryControl();
	const options = [];
    for (const project of data.projects || []) {
	  const flags = [
		project.present === false && "missing from latest complete scan",
		project.stale && "stale discovery",
		project.managed === false && "unmanaged Compose project",
		project.read_only && "read-only",
		project.collision && "name collision",
		project.drift === "changed" && "changed since last Dockpilot apply",
		project.drift === "no-baseline" && "no Dockpilot apply baseline",
		(project.included_by || []).length && `included by ${(project.included_by || []).length} project(s)`,
		project.source_graph_complete === false && "source provenance incomplete; Docker Compose evaluation is not cached",
		(project.source_references || []).map(reference => `${reference.kind}: ${reference.path} (${reference.read_only ? "read-only" : reference.accessible ? "metadata only" : "unavailable"})`).join("; "),
		project.unmanaged_reason,
		(project.services || []).length && `services: ${(project.services || []).join(", ")}`,
		(project.container_ids || []).length && `${(project.container_ids || []).length} attached containers`,
		project.capability_reason,
	  ].filter(Boolean).join(", ") || "ready";
	  const projectNode = card(project.name, flags);
	  const inspectActivity = document.createElement("button");
	  inspectActivity.type = "button";
	  inspectActivity.textContent = "View stored activity";
	  inspectActivity.title = "Stored canonical history remains available while the owning Agent is offline";
	  inspectActivity.addEventListener("click", () => {
	    document.querySelector("#activity-project").value = project.uid;
	    resetHistory("activity", "No activity page loaded.");
	    document.querySelector("#activity").scrollIntoView();
	  });
	  projectNode.append(inspectActivity);
	  projects.append(projectNode);
	  projectStates.set(project.uid, {project, capabilities: hostsByID.get(project.agent_id)?.capabilities || {}});
	  const option = document.createElement("option");
	  option.value = project.uid;
	  option.label = project.name;
	  options.push(option);
    }
	document.querySelector("#project-uids").replaceChildren(...options);
	if (!document.querySelector("#audit-agent").value && hostOptions.length) document.querySelector("#audit-agent").value = hostOptions[0].value;
	if (!document.querySelector("#activity-project").value && options.length) document.querySelector("#activity-project").value = options[0].value;
	if (!document.querySelector("#project-logs-project").value && options.length) document.querySelector("#project-logs-project").value = options[0].value;
	updateHistoryControls();
	updateProjectControls();
    document.querySelector("#summary").textContent = `${(data.hosts || []).length} hosts · ${(data.projects || []).length} projects`;
  } catch (error) {
    const node = document.querySelector("#error");
    node.hidden = false;
    node.textContent = error.message;
    document.querySelector("#summary").textContent = "State unavailable";
  }
}

document.querySelector("#inventory-agent").addEventListener("input", () => {
  inventoryController?.abort();
  resetInventory();
  updateInventoryControl();
});

document.querySelector("#inventory-form").addEventListener("submit", async event => {
  event.preventDefault();
  const agentID = String(new FormData(event.currentTarget).get("agent_id") || "");
  inventoryController?.abort();
  inventoryController = new AbortController();
  await Promise.all(["containers", "images", "networks", "volumes"].map(resource => loadInventoryResource(agentID, resource, inventoryController.signal)));
});

document.querySelector("#audit-agent").addEventListener("input", () => resetHistory("audit", "No audit page loaded."));
document.querySelector("#activity-project").addEventListener("input", () => resetHistory("activity", "No activity page loaded."));
document.querySelector("#audit-form").addEventListener("submit", event => { event.preventDefault(); loadHistoryPage("audit", false); });
document.querySelector("#activity-form").addEventListener("submit", event => { event.preventDefault(); loadHistoryPage("activity", false); });
document.querySelector("#audit-next").addEventListener("click", () => loadHistoryPage("audit", true));
document.querySelector("#activity-next").addEventListener("click", () => loadHistoryPage("activity", true));

function requestURL(path, values) {
  const query = new URLSearchParams(values);
  return `${path}?${query}`;
}

async function loadProjectFile(reveal) {
  const form = new FormData(document.querySelector("#file-form"));
  const projectUID = String(form.get("project_uid") || "");
  const relativePath = String(form.get("relative_path") || "");
  const status = document.querySelector("#file-status");
  const editor = document.querySelector("#file-content");
  const save = document.querySelector("#file-save");
  status.textContent = reveal ? "Loading and revealing live file…" : "Loading live file…";
  editor.disabled = true;
  save.disabled = true;
  try {
    const file = await jsonRequest(requestURL(`/api/v1/projects/${encodeURIComponent(projectUID)}/files`, {path: relativePath, reveal: String(reveal)}));
    editor.value = file.content || "";
    loadedFile = {projectUID, relativePath, sha256: file.sha256, revealed: !file.secret || reveal};
    editor.disabled = !loadedFile.revealed;
	updateProjectControls();
    status.textContent = file.secret && !reveal ? "Secret file loaded masked. Choose Load and reveal to edit." : `Loaded ${relativePath} at sha256 ${file.sha256}.`;
  } catch (error) {
    loadedFile = undefined;
    editor.value = "";
	updateProjectControls();
    status.textContent = error.message;
  }
}

document.querySelector("#file-load").addEventListener("click", () => loadProjectFile(false));
document.querySelector("#file-reveal").addEventListener("click", () => loadProjectFile(true));
document.querySelector("#file-form").addEventListener("submit", async event => {
  event.preventDefault();
  const form = new FormData(event.currentTarget);
  const projectUID = String(form.get("project_uid") || "");
  const relativePath = String(form.get("relative_path") || "");
  const status = document.querySelector("#file-status");
  if (!loadedFile || !loadedFile.revealed || loadedFile.projectUID !== projectUID || loadedFile.relativePath !== relativePath) {
    status.textContent = "Reload this exact project file before saving.";
    return;
  }
  try {
    const operation = await jsonRequest(`/api/v1/projects/${encodeURIComponent(projectUID)}/files`, {
      method: "PUT",
      body: JSON.stringify({
        operation_id: form.get("operation_id"), relative_path: relativePath,
        expected_sha256: loadedFile.sha256, content: form.get("content"),
      }),
    });
    status.textContent = `Write operation ${operation.operation_id} accepted. Reload after it completes.`;
    loadedFile = undefined;
    document.querySelector("#file-content").disabled = true;
	updateProjectControls();
    document.querySelector("#file-operation").value = newOperationID("file-write");
  } catch (error) {
    status.textContent = error.message;
  }
});

document.querySelector("#backup-list-form").addEventListener("submit", async event => {
  event.preventDefault();
  const projectUID = String(new FormData(event.currentTarget).get("project_uid") || "");
  const output = document.querySelector("#backup-list");
  const status = document.querySelector("#backup-status");
  try {
    const backups = await jsonRequest(`/api/v1/projects/${encodeURIComponent(projectUID)}/backups`);
    output.replaceChildren(...backups.map(item => card(item.backup_id, `${item.trigger} · ${item.file_count} files · ${item.size_bytes} bytes · ${item.created_at}`)));
    status.textContent = `${backups.length} backup metadata records loaded.`;
  } catch (error) {
    output.replaceChildren();
    status.textContent = error.message;
  }
});

document.querySelector("#backup-create-form").addEventListener("submit", async event => {
  event.preventDefault();
  const form = new FormData(event.currentTarget);
  const projectUID = String(form.get("project_uid") || "");
  const relativePaths = String(form.get("relative_paths") || "").split("\n").map(value => value.trim()).filter(Boolean);
  const status = document.querySelector("#backup-status");
  try {
    const operation = await jsonRequest(`/api/v1/projects/${encodeURIComponent(projectUID)}/backups`, {
      method: "POST", body: JSON.stringify({operation_id: form.get("operation_id"), relative_paths: relativePaths}),
    });
    status.textContent = `Backup operation ${operation.operation_id} accepted.`;
    document.querySelector("#backup-create-operation").value = newOperationID("backup-create");
  } catch (error) {
    status.textContent = error.message;
  }
});

document.querySelector("#backup-restore-form").addEventListener("submit", async event => {
  event.preventDefault();
  const form = new FormData(event.currentTarget);
  const projectUID = String(form.get("project_uid") || "");
  const backupID = String(form.get("backup_id") || "");
  const status = document.querySelector("#backup-status");
  if (!window.confirm(`Restore backup ${backupID}? Current files are snapshotted first. Compose will not be started.`)) return;
  try {
    const operation = await jsonRequest(`/api/v1/projects/${encodeURIComponent(projectUID)}/backups/${encodeURIComponent(backupID)}/restore`, {
      method: "POST", body: JSON.stringify({operation_id: form.get("operation_id")}),
    });
    status.textContent = `Restore operation ${operation.operation_id} accepted. Compose was not started.`;
    document.querySelector("#backup-restore-operation").value = newOperationID("backup-restore");
  } catch (error) {
    status.textContent = error.message;
  }
});

async function streamSSE(url, signal, onEvent) {
  const response = await fetch(url, {
    signal,
    cache: "no-store",
    headers: {Accept: "text/event-stream"},
  });
  if (!response.ok) throw new Error(`Live stream failed (${response.status})`);
  if (!response.body) throw new Error("Streaming response is unavailable");
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  while (true) {
    const {value, done} = await reader.read();
    if (done) break;
    buffer += decoder.decode(value, {stream: true}).replaceAll("\r\n", "\n");
    if (buffer.length > MAX_SSE_BUFFER) throw new Error("Live event exceeded the browser buffer limit");
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
  const binary = atob(encoded);
  const bytes = new Uint8Array(binary.length);
  for (let index = 0; index < binary.length; index += 1) bytes[index] = binary.charCodeAt(index);
  return utf8Decoder.decode(bytes);
}

function appendLog(output, event) {
  let prefix = event.stream === "STDERR" ? "[stderr] " : "";
  if (event.dropped_bytes) prefix += `[dropped ${event.dropped_bytes} bytes / ${event.dropped_lines || 0} lines]\n`;
  output.textContent += prefix + decodeLogData(event.data);
  if (output.textContent.length > MAX_LOG_CHARACTERS) {
    output.textContent = `[older browser output removed]\n${output.textContent.slice(-MAX_LOG_CHARACTERS)}`;
  }
  output.scrollTop = output.scrollHeight;
}

function renderStats(sample) {
  statsHistory.push(sample);
  if (statsHistory.length > MAX_STATS_SAMPLES) statsHistory.splice(0, statsHistory.length - MAX_STATS_SAMPLES);
  document.querySelector("#metric-cpu").textContent = `${Number(sample.cpu_percent || 0).toFixed(1)}%`;
  document.querySelector("#metric-memory").textContent = `${sample.memory_usage || 0} / ${sample.memory_limit || 0} bytes`;
  document.querySelector("#metric-network").textContent = `${sample.network_rx || 0} / ${sample.network_tx || 0} bytes`;
  document.querySelector("#metric-health").textContent = sample.health || "not reported";
  const points = statsHistory.map((item, index) => {
    const x = statsHistory.length === 1 ? 0 : (index * 119) / (statsHistory.length - 1);
    const y = 100 - Math.max(0, Math.min(100, Number(item.cpu_percent || 0)));
    return `${x.toFixed(2)},${y.toFixed(2)}`;
  }).join(" ");
  document.querySelector("#stats-line").setAttribute("points", points);
  document.querySelector("#stats-chart-title").textContent = `CPU history: ${statsHistory.length} of ${MAX_STATS_SAMPLES} browser-only samples`;
}

document.querySelector("#logs-form").addEventListener("submit", async event => {
  event.preventDefault();
  logsController?.abort();
  logsController = new AbortController();
  const form = new FormData(event.currentTarget);
  const url = requestURL("/api/v1/live/logs", {
    agent_id: form.get("agent_id"), container_id: form.get("container_id"), tail: form.get("tail"), follow: "true",
  });
  document.querySelector("#logs-output").textContent = "";
  document.querySelector("#logs-status").textContent = "Streaming live logs…";
  document.querySelector("#logs-stop").disabled = false;
  try {
    await streamSSE(url, logsController.signal, (kind, value) => {
      if (kind === "log") appendLog(document.querySelector("#logs-output"), value);
    });
    document.querySelector("#logs-status").textContent = "Log stream ended. Start again to reconnect.";
  } catch (error) {
    document.querySelector("#logs-status").textContent = error.name === "AbortError" ? "Log stream stopped." : error.message;
  } finally {
    document.querySelector("#logs-stop").disabled = true;
  }
});

document.querySelector("#logs-stop").addEventListener("click", () => logsController?.abort());

function projectComposeLogURL(projectUID, services, tail, timestamps) {
  const url = new URL(`/api/v1/projects/${encodeURIComponent(projectUID)}/compose/logs`, window.location.origin);
  for (const service of services) url.searchParams.append("service", service);
  url.searchParams.set("tail", tail);
  url.searchParams.set("follow", "true");
  if (timestamps) url.searchParams.set("timestamps", "true");
  return `${url.pathname}?${url.searchParams}`;
}

document.querySelector("#project-logs-form").addEventListener("submit", async event => {
  event.preventDefault();
  const form = new FormData(event.currentTarget);
  const projectUID = String(form.get("project_uid") || "");
  const services = String(form.get("services") || "").split(",").map(value => value.trim()).filter(Boolean);
  const serviceName = /^[A-Za-z0-9][A-Za-z0-9_.-]*$/;
  const status = document.querySelector("#project-logs-status");
  if (services.length > 256 || services.some(service => service.length > 128 || !serviceName.test(service)) || new Set(services).size !== services.length) {
    status.textContent = "Services must be unique Compose service names (up to 256).";
    return;
  }
  projectLogsController?.abort();
  projectLogsController = new AbortController();
  const output = document.querySelector("#project-logs-output");
  output.textContent = "";
  status.textContent = "Streaming live Compose logs…";
  document.querySelector("#project-logs-stop").disabled = false;
  try {
    await streamSSE(projectComposeLogURL(projectUID, services, String(form.get("tail") || "0"), form.get("timestamps") === "on"), projectLogsController.signal, (kind, value) => {
      if (kind === "log") appendLog(output, value);
    });
    status.textContent = "Compose log stream ended. Start again to reconnect.";
  } catch (error) {
    status.textContent = error.name === "AbortError" ? "Compose log stream stopped." : error.message;
  } finally {
    document.querySelector("#project-logs-stop").disabled = true;
  }
});

document.querySelector("#project-logs-stop").addEventListener("click", () => projectLogsController?.abort());

document.querySelector("#metrics-form").addEventListener("submit", async event => {
  event.preventDefault();
  statsController?.abort();
  statsController = new AbortController();
  statsHistory = [];
  const form = new FormData(event.currentTarget);
  const url = requestURL("/api/v1/live/stats", {
    agent_id: form.get("agent_id"), container_id: form.get("container_id"),
  });
  document.querySelector("#metrics-status").textContent = "Streaming live metrics…";
  document.querySelector("#metrics-stop").disabled = false;
  try {
    await streamSSE(url, statsController.signal, (kind, value) => {
      if (kind === "stats") renderStats(value);
    });
    document.querySelector("#metrics-status").textContent = "Metrics stream ended. Start again to reconnect.";
  } catch (error) {
    document.querySelector("#metrics-status").textContent = error.name === "AbortError" ? "Metrics stream stopped." : error.message;
  } finally {
    document.querySelector("#metrics-stop").disabled = true;
  }
});

document.querySelector("#metrics-stop").addEventListener("click", () => statsController?.abort());

for (const id of ["file-project", "file-path", "backup-project", "backup-create-project", "backup-restore-project", "project-logs-project"]) {
  document.querySelector(`#${id}`).addEventListener("input", updateProjectControls);
}
window.addEventListener("beforeunload", () => {
	inventoryController?.abort();
	historyViews.audit.controller?.abort();
	historyViews.activity.controller?.abort();
  logsController?.abort();
  projectLogsController?.abort();
  statsController?.abort();
});

document.querySelector("#file-operation").value = newOperationID("file-write");
document.querySelector("#backup-create-operation").value = newOperationID("backup-create");
document.querySelector("#backup-restore-operation").value = newOperationID("backup-restore");

load();
