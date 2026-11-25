// The panel API client.
//
// Every mutating call carries the session's CSRF token. It comes from
// /api/v1/me, not a cookie, so a page that has not loaded its identity cannot
// change anything.
//
// An operator's calls name the organisation in view with ?org_id, as in
// Keera Gateway.

export class ApiError extends Error {
  constructor(status, message, code) {
    super(message);
    this.status = status;
    this.code = code;
  }
}

let csrf = "";

export function setCsrf(token) {
  csrf = token || "";
}

let orgID = "";

/** setOrg names the organisation an operator's calls are for. Empty is
 *  their own. */
export function setOrg(id) {
  orgID = id || "";
}

// withOrg adds the organisation in view to an API path.
function withOrg(path) {
  if (!orgID || !path.startsWith(V1 + "/")) return path;
  return path + (path.includes("?") ? "&" : "?") + "org_id=" + encodeURIComponent(orgID);
}

// request sends one call and decodes the answer. raw returns the response,
// for a body that streams. A refusal is an ApiError either way.
async function request(method, path, body, { raw, signal } = {}) {
  const headers = {};
  if (body !== undefined) headers["Content-Type"] = "application/json";
  if (csrf && method !== "GET") headers["X-CSRF-Token"] = csrf;

  let res;
  try {
    res = await fetch(withOrg(path), {
      method,
      headers,
      credentials: "same-origin",
      body: body === undefined ? undefined : JSON.stringify(body),
      signal,
    });
  } catch (e) {
    if (e && e.name === "AbortError") throw e;
    throw new ApiError(0, "Keera Agents is unreachable. Check your connection and try again.");
  }

  if (raw && res.ok) return res;
  if (res.status === 204) return null;

  const text = await res.text();
  let payload = null;
  if (text) {
    try {
      payload = JSON.parse(text);
    } catch {
      payload = null;
    }
  }
  if (!res.ok) {
    const err = payload && payload.error ? payload.error : {};
    throw new ApiError(res.status, err.message || `Request failed (${res.status}).`, err.code);
  }
  return payload;
}

const get = (path, opts) => request("GET", path, undefined, opts);
const post = (path, body = {}) => request("POST", path, body);
const patch = (path, body) => request("PATCH", path, body);
const del = (path) => request("DELETE", path);

function query(params) {
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(params || {})) {
    if (v !== undefined && v !== null && v !== "") q.set(k, v);
  }
  const s = q.toString();
  return s ? "?" + s : "";
}

// path encodes each value put into it, so an id is one segment.
const path = (strings, ...values) => strings.reduce((s, part, i) => s + encodeURIComponent(values[i - 1]) + part);

const V1 = "/api/v1";

export const api = {
  authConfig: () => get("/auth/config"),
  // Signing in is a page the browser goes to, not a call.
  loginURL: (provider) => "/auth/login" + query({ provider }),
  silentLoginURL: () => "/auth/login?silent=1",
  signOut: () => post("/auth/logout"),
  // The gateway's operator key, which it checks. It is not kept.
  operatorSignIn: (key) => post("/auth/local", { key }),

  me: () => get(V1 + "/me"),
  // Operators only: every organisation, with its agents and tasks.
  orgs: () => get(V1 + "/orgs"),
  switchOrg: (orgID) => post(V1 + "/me/org", { org_id: orgID }),
  catalog: (projectID) => get(V1 + "/catalog" + query({ project_id: projectID })),
  // Administrators only: whom they can create an agent for.
  members: () => get(V1 + "/members"),
  overview: (since) => get(V1 + "/overview" + query({ since })),
  setup: () => get(V1 + "/setup"),

  agents: () => get(V1 + "/agents"),
  agent: (id) => get(V1 + path`/agents/${id}`),
  agentActivity: (id, since) => get(V1 + path`/agents/${id}/activity` + query({ since })),
  agentCost: (id) => get(V1 + path`/agents/${id}/cost`),
  workspaceCost: (id) => get(V1 + path`/workspaces/${id}/cost`),
  createAgent: (body) => post(V1 + "/agents", body),
  updateAgent: (id, body) => patch(V1 + path`/agents/${id}`, body),
  deleteAgent: (id) => del(V1 + path`/agents/${id}`),
  // action is start, stop or restart.
  agentAction: (id, action) => post(V1 + path`/agents/${id}/${action}`),
  // An administrator becomes the owner, as when the owner left.
  takeOverAgent: (id) => post(V1 + path`/agents/${id}/take-over`),
  agentLogs: (id, signal) => get(V1 + path`/agents/${id}/logs` + "?follow=1", { raw: true, signal }),
  giveTask: (id, prompt) => post(V1 + path`/agents/${id}/tasks`, { prompt }),

  addBinding: (agentID, body) => post(V1 + path`/agents/${agentID}/bindings`, body),
  addWorkspaceBinding: (id, body) => post(V1 + path`/workspaces/${id}/bindings`, body),
  deleteBinding: (id) => del(V1 + path`/bindings/${id}`),

  schedules: (agentID) => get(V1 + "/schedules" + query({ agent_id: agentID })),
  createSchedule: (body) => post(V1 + "/schedules", body),
  updateSchedule: (id, body) => patch(V1 + path`/schedules/${id}`, body),
  deleteSchedule: (id) => del(V1 + path`/schedules/${id}`),
  runSchedule: (id) => post(V1 + path`/schedules/${id}/run`),
  previewSchedule: (cron, timezone, signal) =>
    request("POST", V1 + "/schedules/preview", { cron, timezone }, { signal }),

  squads: () => get(V1 + "/squads"),
  squad: (id) => get(V1 + path`/squads/${id}`),
  createSquad: (body) => post(V1 + "/squads", body),
  updateSquad: (id, body) => patch(V1 + path`/squads/${id}`, body),
  deleteSquad: (id) => del(V1 + path`/squads/${id}`),
  createCard: (squadID, body) => post(V1 + path`/squads/${squadID}/cards`, body),
  card: (id) => get(V1 + path`/cards/${id}`),
  updateCard: (id, body) => patch(V1 + path`/cards/${id}`, body),
  deleteCard: (id) => del(V1 + path`/cards/${id}`),
  comment: (id, body) => post(V1 + path`/cards/${id}/comments`, { body }),

  // params: agent_id, state, source, limit, before.
  tasks: (params) => get(V1 + "/tasks" + query(params)),
  taskFacets: (params) => get(V1 + "/tasks/facets" + query(params)),
  task: (id) => get(V1 + path`/tasks/${id}`),
  taskEvents: (id, after) => get(V1 + path`/tasks/${id}/events` + query({ after })),
  cancelTask: (id) => post(V1 + path`/tasks/${id}/cancel`),

  connectionKinds: () => get(V1 + "/connection-kinds"),
  connections: () => get(V1 + "/connections"),
  connection: (id) => get(V1 + path`/connections/${id}`),
  createConnection: (body) => post(V1 + "/connections", body),
  updateConnection: (id, body) => patch(V1 + path`/connections/${id}`, body),
  deleteConnection: (id) => del(V1 + path`/connections/${id}`),
  checkConnection: (id) => post(V1 + path`/connections/${id}/check`),
  folders: (id, parent) => get(V1 + path`/connections/${id}/folders` + query({ parent })),
  channels: (id) => get(V1 + path`/connections/${id}/channels`),
  // Answers { url }, where the browser goes to sign in to the service.
  connectAccount: (id) => post(V1 + path`/connections/${id}/account`),
  disconnectAccount: (id) => del(V1 + path`/connections/${id}/account`),

  audit: (before) => get(V1 + "/audit" + query({ limit: 100, before })),
  // month is YYYY-MM; empty is this one.
  billing: (month) => get(V1 + "/billing" + query({ month })),

  // all lists every workspace in the organisation, for an administrator.
  workspaces: (all) => get(V1 + "/workspaces" + query({ all: all ? "true" : "" })),
  workspace: (id) => get(V1 + path`/workspaces/${id}`),
  createWorkspace: (body) => post(V1 + "/workspaces", body),
  updateWorkspace: (id, body) => patch(V1 + path`/workspaces/${id}`, body),
  deleteWorkspace: (id) => del(V1 + path`/workspaces/${id}`),
  // action is start or stop.
  workspaceAction: (id, action) => post(V1 + path`/workspaces/${id}/${action}`),
  workspaceLogs: (id, signal) => get(V1 + path`/workspaces/${id}/logs` + "?follow=1", { raw: true, signal }),

  sshKeys: () => get(V1 + "/me/ssh-keys"),
  addSSHKey: (body) => post(V1 + "/me/ssh-keys", body),
  deleteSSHKey: (id) => del(V1 + path`/me/ssh-keys/${id}`),
};
