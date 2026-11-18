// Small DOM helpers that keep the views declarative, without a framework.
// Most are the Keera Gateway panel's own, so the two panels behave alike.

/** h builds an element. Children may be nodes, strings, arrays, or null. */
export function h(tag, props, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (v === null || v === undefined || v === false) continue;
    if (k === "class") el.className = v;
    else if (k === "style" && typeof v === "object") Object.assign(el.style, v);
    else if (k.startsWith("on")) el.addEventListener(k.slice(2).toLowerCase(), v);
    else if (k === "dataset") Object.assign(el.dataset, v);
    else if (v === true) el.setAttribute(k, "");
    else el.setAttribute(k, v);
  }
  append(el, children);
  return el;
}

function append(el, children) {
  for (const c of children.flat(4)) {
    if (c === null || c === undefined || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
}

/** svg builds an SVG element, which needs its own namespace. */
export function svg(tag, props, ...children) {
  const el = document.createElementNS("http://www.w3.org/2000/svg", tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (v === null || v === undefined || v === false) continue;
    if (k.startsWith("on")) el.addEventListener(k.slice(2).toLowerCase(), v);
    else el.setAttribute(k, v);
  }
  append(el, children);
  return el;
}

export function replace(el, ...children) {
  el.replaceChildren();
  append(el, children);
  return el;
}

const numberFmt = new Intl.NumberFormat();

export function num(n) {
  return numberFmt.format(Math.round(n || 0));
}

/** compact renders large counts short: 1.2M, not 1,203,344. */
export function compact(n) {
  n = n || 0;
  const abs = Math.abs(n);
  if (abs >= 1e9) return (n / 1e9).toFixed(abs >= 1e10 ? 0 : 1) + "B";
  if (abs >= 1e6) return (n / 1e6).toFixed(abs >= 1e7 ? 0 : 1) + "M";
  if (abs >= 1e4) return (n / 1e3).toFixed(0) + "k";
  if (abs >= 1e3) return (n / 1e3).toFixed(1) + "k";
  return numberFmt.format(n);
}

/** money renders integer micro-units of CHF, with the currency first: CHF 3.00.
 *  Under a franc it keeps two significant digits, so a fraction of a cent is
 *  not 0.00. */
export function money(micros) {
  const v = (micros || 0) / 1e6;
  let s;
  if (v === 0) s = "0.00";
  else if (Math.abs(v) >= 1) s = v.toFixed(2);
  else s = String(Number(v.toPrecision(2)));
  if (!s.includes(".")) s += ".00";
  else if (s.split(".")[1].length < 2) s += "0";
  return `CHF ${s}`;
}

/** plural picks word for one and many for the rest. */
export function plural(n, word, many = word + "s") {
  return n === 1 ? word : many;
}

/** personName is someone's name, or their address when they have none. */
export function personName(u) {
  return u.name || u.email;
}

/** initials of a name or an address, for the avatar. */
export function initials(name, email) {
  const words = (name || "").trim().split(/\s+/).filter(Boolean);
  if (words.length >= 2) return (words[0][0] + words[words.length - 1][0]).toUpperCase();
  if (words.length === 1) return words[0].slice(0, 2).toUpperCase();
  return (email || "?").slice(0, 2).toUpperCase();
}

export function capitalise(word) {
  return word ? word[0].toUpperCase() + word.slice(1) : "";
}

/** duration is how long a task ran, in minutes and hours rather than
 *  seconds. */
export function duration(msValue) {
  const secs = Math.round((msValue || 0) / 1000);
  if (secs < 60) return secs + " s";
  const mins = Math.floor(secs / 60);
  if (mins < 60) return mins + " min" + (secs % 60 ? " " + (secs % 60) + " s" : "");
  const hours = Math.floor(mins / 60);
  return hours + " h" + (mins % 60 ? " " + (mins % 60) + " min" : "");
}

export function dateTime(iso) {
  if (!iso) return "-";
  const d = new Date(iso);
  return d.toLocaleString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
}

export function date(iso) {
  if (!iso) return "-";
  return new Date(iso).toLocaleDateString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
  });
}

/** ago is a relative time for tables, such as "5m ago". */
export function ago(iso) {
  if (!iso) return "-";
  const secs = (Date.now() - new Date(iso).getTime()) / 1000;
  const steps = [
    [60, "s"],
    [3600, "m", 60],
    [86400, "h", 3600],
    [2592000, "d", 86400],
    [Infinity, "mo", 2592000],
  ];
  if (secs < 45) return "just now";
  for (const [limit, unit, div] of steps) {
    if (secs < limit) return Math.round(secs / (div || 1)) + unit + " ago";
  }
  return date(iso);
}

/** brandMark uses the favicon file, so the two cannot drift apart. */
export function brandMark(size) {
  return h("img", {
    class: "brand-mark",
    src: "/assets/favicon.svg",
    alt: "",
    width: size,
    height: size,
  });
}

export function icon(path) {
  return svg(
    "svg",
    {
      viewBox: "0 0 24 24",
      fill: "none",
      stroke: "currentColor",
      "stroke-width": "1.8",
      "stroke-linecap": "round",
      "stroke-linejoin": "round",
      "aria-hidden": "true",
    },
    svg("path", { d: path }),
  );
}

export const icons = {
  dashboard: "M3 13h5v8H3zM10 3h5v18h-5zM17 9h4v12h-4z",
  start:
    "M4.5 16.5c-1.5 1.26-2 5-2 5s3.74-.5 5-2c.71-.84.7-2.13-.09-2.91a2.18 2.18 0 0 0-2.91-.09z" +
    "M12 15l-3-3a22 22 0 0 1 2-3.95A12.88 12.88 0 0 1 22 2c0 2.72-.78 7.5-6 11a22.35 22.35 0 0 1-4 2z" +
    "M9 12H4s.55-3.03 2-4c1.62-1.08 5 0 5 0M12 15v5s3.03-.55 4-2c1.08-1.62 0-5 0-5",
  // A robot's head: an agent is a worker, not a person.
  agents:
    "M12 2v3M8 5h8a3 3 0 0 1 3 3v7a3 3 0 0 1-3 3H8a3 3 0 0 1-3-3V8a3 3 0 0 1 3-3z" +
    "M9.5 10.5h.01M14.5 10.5h.01M9.5 14.5h5M2 10v4M22 10v4M9 18v3M15 18v3",
  // Columns of cards, as on a kanban board.
  board:
    "M4 3h4a1 1 0 0 1 1 1v16a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1z" +
    "M11 3h4a1 1 0 0 1 1 1v10a1 1 0 0 1-1 1h-4a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1z" +
    "M18 3h2a1 1 0 0 1 1 1v6a1 1 0 0 1-1 1h-2a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1z",
  tasks: "M9 11l3 3 8-8M20 12v7a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h9",
  schedules: "M21 12a9 9 0 1 1-18 0 9 9 0 0 1 18 0zM12 7v5l3 2",
  connections:
    "M10 13a5 5 0 0 0 7.54.54l3-3a5 5 0 0 0-7.07-7.07l-1.72 1.71" +
    "M14 11a5 5 0 0 0-7.54-.54l-3 3a5 5 0 0 0 7.07 7.07l1.71-1.71",
  audit: "M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8zM14 2v6h6M9 13h6M9 17h4",
  billing: "M6 2h12v20l-3-2-3 2-3-2-3 2zM9 7h6M9 11h6M9 15h4",
  orgs: "M3 21h18M5 21V7l7-4 7 4v14M9 9h.01M9 13h.01M9 17h.01M15 9h.01M15 13h.01M15 17h.01",
  // The Keera Gateway, as its own panel draws a route.
  gateway: "M3 12h4c3.5 0 4.5-7 9-7h5M3 12h4c3.5 0 4.5 7 9 7h5M18 2l3 3-3 3M18 16l3 3-3 3",
  signout: "M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4M16 17l5-5-5-5M21 12H9",
  plus: "M12 5v14M5 12h14",
  cloud: "M17.5 19H9a7 7 0 1 1 6.71-9h1.79a4.5 4.5 0 1 1 0 9z",
  rows: "M8 6h13M8 12h13M8 18h13M3 6h.01M3 12h.01M3 18h.01",
  tiles: "M3 3h7v7H3zM14 3h7v7h-7zM3 14h7v7H3zM14 14h7v7h-7z",
  passkey:
    "M2.6 17.4A2 2 0 0 0 2 18.8V21a1 1 0 0 0 1 1h3a1 1 0 0 0 1-1v-1a1 1 0 0 1 1-1h1a1 1 0 0 0 1-1v-1a1 1 0 0 1 1-1h.2a2 2 0 0 0 1.4-.6l.8-.8a6.5 6.5 0 1 0-4-4zM16.5 7.5h.01",
  eye: "M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8zM15 12a3 3 0 1 1-6 0 3 3 0 0 1 6 0z",
  eyeOff:
    "M17.94 17.94A10.07 10.07 0 0 1 12 20c-7 0-11-8-11-8a18.45 18.45 0 0 1 5.06-5.94M9.9 4.24A9.12 9.12 0 0 1 12 4c7 0 11 8 11 8a18.5 18.5 0 0 1-2.16 3.19M14.12 14.12a3 3 0 1 1-4.24-4.24M1 1l22 22",
  copy: "M20 9h-9a2 2 0 0 0-2 2v9a2 2 0 0 0 2 2h9a2 2 0 0 0 2-2v-9a2 2 0 0 0-2-2zM5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1",
  close: "M18 6L6 18M6 6l12 12",
  menu: "M3 6h18M3 12h18M3 18h18",
  more: "M5 12h.01M12 12h.01M19 12h.01",
  theme: "M12 3a9 9 0 1 0 9 9 7 7 0 0 1-9-9z",
  check: "M20 6L9 17l-5-5",
  chat: "M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z",
  send: "M22 2L11 13M22 2l-7 20-4-9-9-4z",
  play: "M7 4l13 8-13 8z",
  stop: "M7 5h10a2 2 0 0 1 2 2v10a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V7a2 2 0 0 1 2-2z",
  restart: "M3 12a9 9 0 1 0 2.64-6.36L3 8M3 3v5h5",
  trash: "M3 6h18M8 6V4a1 1 0 0 1 1-1h6a1 1 0 0 1 1 1v2m3 0v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6",
  pencil: "M12 20h9M16.5 3.5a2.1 2.1 0 0 1 3 3L7 19l-4 1 1-4z",
  terminal: "M4 17l6-6-6-6M12 19h8",
  // A terminal's prompt in a window, for a workspace.
  workspace: "M4 4h16a2 2 0 0 1 2 2v12a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V6a2 2 0 0 1 2-2zM7 9l3 3-3 3M13 15h4",
  code: "M16 18l6-6-6-6M8 6l-6 6 6 6",
  folder: "M22 19a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h5l2 3h9a2 2 0 0 1 2 2z",
  file: "M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8zM14 2v6h6",
  // A cylinder, as databases are drawn.
  database:
    "M4 6c0-1.66 3.58-3 8-3s8 1.34 8 3-3.58 3-8 3-8-1.34-8-3zM4 6v12c0 1.66 3.58 3 8 3s8-1.34 8-3V6" +
    "M4 12c0 1.66 3.58 3 8 3s8-1.34 8-3",
  calendar: "M8 2v4M16 2v4M5 4h14a2 2 0 0 1 2 2v14a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V6a2 2 0 0 1 2-2zM3 10h18",
  person: "M20 21v-2a4 4 0 0 0-4-4H8a4 4 0 0 0-4 4v2M12 3a4 4 0 1 1 0 8 4 4 0 0 1 0-8z",
  // A wrench, for a tool the agent called.
  tool:
    "M14.7 6.3a1 1 0 0 0 0 1.4l1.6 1.6a1 1 0 0 0 1.4 0l3.77-3.77a6 6 0 0 1-7.94 7.94" +
    "l-6.91 6.91a2.12 2.12 0 0 1-3-3l6.91-6.91a6 6 0 0 1 7.94-7.94z",
  move: "M5 9l-3 3 3 3M9 5l3-3 3 3M15 19l-3 3-3-3M19 9l3 3-3 3M2 12h20M12 2v20",
  collapse: "M11 17l-5-5 5-5M18 17l-5-5 5-5",
  back: "M19 12H5M12 19l-7-7 7-7",
  forward: "M5 12h14M12 5l7 7-7 7",
  up: "M12 19V5M5 12l7-7 7 7",
  down: "M12 5v14M19 12l-7 7-7-7",
  right: "M9 18l6-6-6-6",
  docs: "M2 3h6a4 4 0 0 1 4 4v14a3 3 0 0 0-3-3H2zM22 3h-6a4 4 0 0 0-4 4v14a3 3 0 0 1 3-3h7z",
};

// Google names both a sign-in and the services Google Chat and Drive.
const googleMark = {
  viewBox: "0 0 24 24",
  path: "M6 12a6 6 0 0 0 11.659 2H12v-4h9.805v4H21.8c-.927 4.564-4.962 8-9.8 8c-5.523 0-10-4.477-10-10S6.477 2 12 2a9.99 9.99 0 0 1 8.282 4.393l-3.278 2.295A6 6 0 0 0 6 12",
};

function drawMark(mark) {
  if (!mark) return null;
  return svg(
    "svg",
    { class: "provider-mark", viewBox: mark.viewBox, fill: "currentColor", "aria-hidden": "true" },
    svg("path", { d: mark.path }),
  );
}

/** idpMarks are the identity providers' logos, drawn in currentColor to
 *  follow the theme, keyed by the provider names the gateway signs in with.
 *  idpAliases maps other obvious spellings of each vendor to them.
 *  internal/webui/marks/NOTICE says why the panel may show them. */
const idpMarks = {
  google: googleMark,
  // Padded, as the logo fills its square and Google's mark does not.
  microsoft: {
    viewBox: "-2 -2 27 27",
    path: "M0 0h11v11H0zM12 0h11v11H12zM0 12h11v11H0zM12 12h11v11H12z",
  },
  github: {
    viewBox: "0 0 24 24",
    path: "M12 .297c-6.63 0-12 5.373-12 12 0 5.303 3.438 9.8 8.205 11.385.6.113.82-.258.82-.577 0-.285-.01-1.04-.015-2.04-3.338.724-4.042-1.61-4.042-1.61C4.422 18.07 3.633 17.7 3.633 17.7c-1.087-.744.084-.729.084-.729 1.205.084 1.838 1.236 1.838 1.236 1.07 1.835 2.809 1.305 3.495.998.108-.776.417-1.305.76-1.605-2.665-.3-5.466-1.332-5.466-5.93 0-1.31.465-2.38 1.235-3.22-.135-.303-.54-1.523.105-3.176 0 0 1.005-.322 3.3 1.23.96-.267 1.98-.399 3-.405 1.02.006 2.04.138 3 .405 2.28-1.552 3.285-1.23 3.285-1.23.645 1.653.24 2.873.12 3.176.765.84 1.23 1.91 1.23 3.22 0 4.61-2.805 5.625-5.475 5.92.42.36.81 1.096.81 2.22 0 1.606-.015 2.896-.015 3.286 0 .315.21.69.825.57C20.565 22.092 24 17.592 24 12.297c0-6.627-5.373-12-12-12",
  },
};

const idpAliases = {
  workspace: "google",
  gsuite: "google",
  entra: "microsoft",
  entraid: "microsoft",
  azure: "microsoft",
  azuread: "microsoft",
  m365: "microsoft",
};

/** idpMark is an identity provider's logo at text size, or null. */
export function idpMark(name) {
  const key = String(name || "").toLowerCase();
  return drawMark(idpMarks[idpAliases[key] || key]);
}

// Padded, as the logo fills its square and Google's mark does not.
const slackMark = {
  viewBox: "-42 -42 596 596",
  path:
    "M107.9 323.6c0 29.7-24 53.8-53.8 53.8S.3 353.4.3 323.6c0-29.7 24-53.8 53.8-53.8h53.8zm26.9 0c0-29.7 24-53.8 53.8-53.8s53.8 24 53.8 53.8V458c0 29.7-24 53.8-53.8 53.8s-53.8-24-53.8-53.8z" +
    "M188.6 107.7c-29.7 0-53.8-24-53.8-53.8S158.8.1 188.6.1s53.8 24 53.8 53.8v53.8zm0 27.3c29.7 0 53.8 24 53.8 53.8s-24 53.8-53.8 53.8H53.8C24 242.6 0 218.5 0 188.8S24 135 53.8 135z" +
    "M404.1 188.8c0-29.7 24-53.8 53.8-53.8s53.8 24 53.8 53.8-24 53.8-53.8 53.8h-53.8zm-26.9 0c0 29.7-24 53.8-53.8 53.8-29.7 0-53.8-24-53.8-53.8V54c0-29.7 24-53.8 53.8-53.8s53.8 24 53.8 53.8z" +
    "M323.4 404.3c29.7 0 53.8 24 53.8 53.8 0 29.7-24 53.8-53.8 53.8-29.7 0-53.8-24-53.8-53.8v-53.8zm0-26.9c-29.7 0-53.8-24-53.8-53.8s24-53.8 53.8-53.8h134.8c29.7 0 53.8 24 53.8 53.8 0 29.7-24 53.8-53.8 53.8z",
};

const gitlabMark = {
  viewBox: "0 0 256 256",
  path:
    "M231.928 169.78l-94.82 65.645a16.076 16.076 0 0 1-18.215 0L24.072 169.78a16.04 16.04 0 0 1-6.352-17.278L45.049 50.018" +
    "a12 12 0 0 1 22.831-1.121L88.544 104h78.912l20.664-55.104a12 12 0 0 1 22.83 1.121l27.33 102.486a16.04 16.04 0 0 1-6.352 17.278z",
};

// Atlassian's, for Jira and Confluence alike. Padded, as it fills its square.
const atlassianMark = {
  viewBox: "-16 -16 288 288",
  path:
    "M75.79 117.95C71.97 113.87 66.02 114.1 63.43 119.29L.79 244.57C-.37 246.89-.25 249.64 1.12 251.85" +
    "C2.48 254.06 4.89 255.4 7.49 255.4L94.72 255.4C97.57 255.47 100.2 253.85 101.41 251.26" +
    "C120.22 212.37 108.83 153.25 75.79 117.95ZM121.76 4.01C86.72 59.52 89.03 120.99 112.11 167.14L154.17 251.26" +
    "C155.44 253.8 158.03 255.4 160.87 255.4L248.09 255.4C250.69 255.4 253.1 254.06 254.46 251.85" +
    "C255.83 249.64 255.95 246.88 254.79 244.56C254.79 244.56 137.44 9.84 134.49 3.96C131.85-1.29 125.15-1.37 121.76 4.01Z",
};

// The sprocket alone, cut out of HubSpot's wordmark.
const hubspotMark = {
  viewBox: "67.2 -0.2 30 30",
  path:
    "M89.806 9.75V6.223a2.716 2.716 0 0 0 1.566-2.448v-.081c0-1.5-1.216-2.716-2.716-2.716h-.081a2.716 2.716 0 0 0-2.716 2.716" +
    "v.081a2.716 2.716 0 0 0 1.566 2.448V9.75a7.691 7.691 0 0 0-3.657 1.61l-9.673-7.534c.069-.249.105-.505.109-.762" +
    "a3.06 3.06 0 1 0-3.064 3.054 3.027 3.027 0 0 0 1.507-.41l9.525 7.412a7.715 7.715 0 0 0 .118 8.69l-2.897 2.898" +
    "a2.488 2.488 0 0 0-.724-.118 2.513 2.513 0 1 0 2.515 2.515 2.48 2.48 0 0 0-.118-.724l2.866-2.867A7.728 7.728 0 1 0 89.806 9.75" +
    "m-1.188 11.6a3.965 3.965 0 1 1 0-7.927 3.965 3.965 0 0 1 .004 7.925",
};

const salesforceMark = {
  viewBox: "0 0 52 52",
  path:
    "M21.7 11.9c1.7-1.7 4-2.8 6.6-2.8 3.4 0 6.4 1.9 8 4.7 1.4-.6 2.9-1 4.5-1C47 12.8 52 17.8 52 24s-5 11.2-11.2 11.2" +
    "c-.8 0-1.5-.1-2.2-.2-1.4 2.5-4.1 4.2-7.2 4.2-1.3 0-2.5-.3-3.6-.8-1.4 3.3-4.7 5.6-8.6 5.6-4 0-7.5-2.5-8.8-6.1" +
    "-.6.1-1.2.2-1.8.2-4.8 0-8.7-3.9-8.7-8.7 0-3.2 1.7-6 4.3-7.5-.5-1.2-.8-2.6-.8-4 0-5.5 4.5-10 10.1-10 3.5 0 6.4 1.5 8.2 3.9",
};

const awsMark = {
  viewBox: "0 .05 32.25 32.25",
  path:
    "M15.63 31.388l-7.135-2.56V18.373l7.135 2.43zm1.3 0l7.135-2.56V18.373l-7.135 2.432zm-7.7-13.8l7.2-2.033 6.696 2.16-6.696 2.273z" +
    "m-2.092-.8L0 14.22V3.75l7.135 2.43zm1.307 0l7.135-2.56V3.75L8.443 6.192zm-7.7-13.8l7.2-2.043 6.696 2.16-6.696 2.273z" +
    "m23.052 13.8l-7.135-2.56V3.75l7.135 2.43zm1.3 0l7.135-2.56V3.75l-7.135 2.43zm-7.7-13.8l7.2-2.033 6.696 2.16-6.696 2.273z",
};

// The wheel alone, cut out of the Kubernetes wordmark.
const kubernetesMark = {
  viewBox: "0 -1.5 87.5 87.5",
  path:
    "M43.6 0C42.8 0 41.9 0 41.1.2L10.6 14.9C9.1 15.6 8 16.9 7.6 18.6L.1 51.5C-.2 53.2.2 54.9 1.2 56.3L22.3 82.4" +
    "C23.5 83.6 25.2 84.4 26.9 84.5L60.5 84.5C62.3 84.7 64 83.9 65.1 82.4L86.2 56.3C87.2 54.9 87.6 53.2 87.4 51.5L79.8 18.8" +
    "C79.3 17.2 78.1 15.9 76.6 15.1L46.1.5C45.3.1 44.4 0 43.6 0zM43.57 10.68C43.65 10.68 43.72 10.69 43.8 10.7" +
    "C44.8 10.8 45.6 11.8 45.5 12.8L45.5 13.4C45.4 13.8 45.4 14.2 45.3 14.6C45.3 14.8 45.2 15 45.2 15.2L45.2 15.3" +
    "C45 16.5 45 17.6 45 18.7C45.1 19 45.2 19.3 45.5 19.5L45.5 19.4L45.6 20.2C50.3 20.7 54.9 22.5 58.6 25.4" +
    "C59.4 26.2 60.3 27 61.1 27.8L61.9 27.3L62 27.3C62.1 27.4 62.3 27.4 62.4 27.4C62.6 27.4 62.8 27.3 63 27.2" +
    "C63.9 26.6 64.8 25.8 65.5 25C65.6 24.9 65.8 24.8 65.9 24.6C66.1 24.3 66.4 24 66.7 23.7C66.8 23.7 66.9 23.6 67 23.5" +
    "L67.1 23.4C67.5 23.1 68 22.9 68.5 22.9C69 22.9 69.6 23.2 69.9 23.6C70.6 24.5 70.4 25.7 69.5 26.4" +
    "C69.5 26.5 69.6 26.5 69.5 26.6C69.4 26.7 69.3 26.7 69.2 26.8C68.8 27 68.5 27.2 68.1 27.4L67.5 27.7" +
    "C66.5 28.3 65.6 28.9 64.7 29.7C64.5 29.9 64.4 30.3 64.4 30.6L64.4 30.7L63.8 31.3C65.4 33.8 66.6 36.6 67.3 39.5" +
    "C67.9 42.4 68.1 45.4 67.7 48.3L68.5 48.5C68.6 48.8 68.9 49.1 69.2 49.2C70.3 49.5 71.5 49.7 72.6 49.8L72.7 49.8" +
    "C72.9 49.9 73.1 49.9 73.3 49.9C73.7 49.9 74.1 49.9 74.5 50C74.7 50 74.9 50 74.9 50.1C74.9 50.2 75 50.2 75.1 50.2" +
    "C76 50.4 76.8 51.2 76.9 52.3C76.6 53.1 75.8 53.7 74.9 53.7C74.7 53.7 74.6 53.7 74.4 53.6C74.3 53.5 74.3 53.5 74.2 53.5" +
    "C74 53.5 73.9 53.4 73.8 53.4C73.4 53.3 73.1 53.1 72.7 52.9C72.5 52.9 72.3 52.8 72.1 52.7L72 52.7" +
    "C71 52.3 69.9 52 68.8 51.8L68.7 51.8C68.4 51.8 68.1 51.9 67.9 52.1C67.9 52.1 67.9 52.2 67.8 52.2L67 52.1" +
    "C65.2 57.8 61.3 62.6 56.2 65.7L56.5 66.5C56.5 66.5 56.4 66.5 56.4 66.6C56.2 66.9 56.2 67.3 56.3 67.6" +
    "C56.7 68.6 57.2 69.6 57.9 70.5L57.9 70.7C58.1 70.9 58.2 71 58.3 71.2C58.6 71.5 58.8 71.8 59 72.2" +
    "C59.1 72.3 59.2 72.4 59.2 72.5C59.2 72.5 59.3 72.5 59.3 72.6C59.5 73.1 59.5 73.6 59.4 74.1C59.3 74.6 58.9 75 58.5 75.2" +
    "C58.2 75.3 58 75.4 57.7 75.4C56.9 75.4 56.2 74.9 55.8 74.2C55.7 74.2 55.7 74.1 55.7 74.1C55.6 74 55.6 73.9 55.5 73.8" +
    "C55.3 73.5 55.2 73.1 55.1 72.7L54.9 72.1L54.9 72C54.6 70.9 54.1 69.9 53.6 68.9C53.4 68.6 53.1 68.4 52.8 68.3" +
    "C52.8 68.26 52.8 68.24 52.79 68.22C52.79 68.22 52.79 68.22 52.79 68.21L52.79 68.21C52.78 68.21 52.78 68.21 52.78 68.21" +
    "C52.76 68.2 52.74 68.2 52.7 68.2L52.3 67.5C51.3 67.8 50.2 68.2 49.1 68.4C47.3 68.9 45.5 69.1 43.7 69.1" +
    "C40.7 69.1 37.8 68.6 35 67.5L34.6 68.3C34.6 68.34 34.6 68.37 34.59 68.38C34.59 68.38 34.58 68.39 34.58 68.39" +
    "C34.57 68.4 34.54 68.4 34.5 68.4C34.2 68.5 33.9 68.7 33.7 69C33.2 70 32.7 71 32.4 72.1L32.2 72.7" +
    "C32.1 73.1 31.9 73.4 31.8 73.8C31.7 73.9 31.6 74 31.6 74.1C31.5 74.1 31.5 74.2 31.5 74.2C31.1 74.9 30.4 75.4 29.6 75.4" +
    "C29.4 75.4 29.1 75.3 28.9 75.2C28 74.7 27.6 73.5 28.1 72.5C28.2 72.5 28.2 72.4 28.2 72.4C28.3 72.3 28.3 72.2 28.4 72.1" +
    "C28.7 71.7 28.9 71.3 29.1 71.1C29.3 70.9 29.4 70.8 29.5 70.6L29.5 70.5C30.1 69.6 30.7 68.6 31.1 67.6" +
    "C31.2 67.3 31.2 66.9 31 66.6C31 66.6 30.9 66.6 30.9 66.5L31.4 65.8C30.4 65.3 29.6 64.7 28.7 64" +
    "C24.8 61 22 56.9 20.5 52.3L19.6 52.4C19.6 52.4 19.6 52.3 19.5 52.3C19.3 52.1 19 52 18.7 52L18.6 52" +
    "C17.4 52.2 16.4 52.5 15.3 52.9L15.2 52.9C15 52.9 14.8 53 14.6 53.1C14.3 53.2 13.9 53.4 13.5 53.5" +
    "C13.41 53.5 13.18 53.43 13.11 53.47C13.11 53.47 13.11 53.47 13.11 53.47C13.11 53.48 13.1 53.48 13.1 53.48" +
    "C13.1 53.49 13.1 53.5 13.1 53.5C13.1 53.54 13.1 53.56 13.09 53.58C13.09 53.58 13.09 53.59 13.08 53.59L13.08 53.59" +
    "C13.07 53.6 13.04 53.6 13 53.6C12.8 53.7 12.7 53.7 12.5 53.7C11.6 53.8 10.7 53.2 10.5 52.3C10.3 51.2 11 50.2 12.1 50" +
    "C12.2 49.9 12.2 49.9 12.3 49.9C12.5 49.9 12.6 49.8 12.7 49.8C13.1 49.8 13.5 49.7 13.9 49.7" +
    "C14.1 49.6 14.3 49.6 14.5 49.6C15.7 49.5 16.8 49.3 17.9 49C18.2 48.8 18.5 48.6 18.6 48.3C18.6 48.3 18.7 48.3 18.7 48.2" +
    "L19.5 48C18.6 42.3 19.9 36.5 23 31.6C23.1 31.4 23.2 31.3 23.3 31.1L22.72 30.52C22.71 30.55 22.71 30.57 22.7 30.6" +
    "L22.7 30.5L22.72 30.52C22.76 30.23 22.58 29.88 22.4 29.7C21.6 28.9 20.6 28.3 19.6 27.7L19 27.4" +
    "C18.6 27.2 18.2 27 17.9 26.8C17.8 26.8 17.6 26.6 17.6 26.6C17.6 26.6 17.6 26.5 17.5 26.5C16.7 25.8 16.5 24.6 17.1 23.7" +
    "C17.4 23.2 17.9 23 18.5 23C19 23 19.5 23.2 19.9 23.5L20 23.6C20.1 23.7 20.2 23.7 20.3 23.8" +
    "C20.6 24.1 20.8 24.4 21.1 24.7C21.2 24.8 21.4 24.9 21.5 25.1C22.2 25.9 23.1 26.7 24 27.4C24.2 27.5 24.4 27.6 24.6 27.6" +
    "C24.8 27.6 24.9 27.5 25 27.5L25.1 27.5L25.7 27.9C29 24.4 33.3 21.9 38 20.9C39.2 20.7 40.3 20.5 41.4 20.4L41.5 19.6" +
    "L41.5 19.4C41.8 19.2 41.9 18.9 42 18.6C42 17.4 42 16.3 41.8 15.2L41.8 15.1C41.8 14.9 41.8 14.7 41.7 14.5" +
    "C41.6 14.1 41.5 13.7 41.5 13.3L41.5 12.8C41.5 12.3 41.7 11.8 42.1 11.4C42.54 10.96 43.05 10.68 43.57 10.68zM41 25.5" +
    "L39 25.9C35.5 26.7 32.3 28.5 29.7 31L38 36.9C38.3 37 38.5 37.1 38.8 37.1C39.7 37.1 40.5 36.4 40.5 35.5L41 25.5z" +
    "M45.8 25.6L46.4 35.7C46.4 36 46.5 36.2 46.7 36.4C47.2 37.1 48.2 37.2 48.9 36.8L57.1 31C54.1 28 50.2 26.1 45.8 25.6z" +
    "M26.8 34.7C24.6 38.4 23.6 42.7 23.9 46.9L33.6 44.1C34 44 34.2 43.9 34.4 43.6C35 42.9 34.9 41.9 34.2 41.3L26.8 34.7z" +
    "M60.1 34.7L52.6 41.5C52.4 41.7 52.3 41.9 52.2 42.2C52 43.1 52.5 44 53.4 44.2L62.9 46.9C63.1 44.8 63 42.7 62.5 40.6" +
    "C62 38.5 61.2 36.5 60.1 34.7zM42 41.8L40.1 44.2L40.8 47.1L43.5 48.4L46.2 47.1L46.9 44.2L45 41.8L42 41.8zM51.9 49.8" +
    "C51.6 49.8 51.3 49.9 51 50C50.3 50.3 49.9 51.1 50.1 51.9L54 61.3C56.4 59.8 58.5 57.7 60 55.3" +
    "C60.8 54.2 61.4 52.9 61.9 51.5L51.9 49.8zM35 49.9L25.1 51.6C26.5 55.6 29.3 59.1 32.8 61.4L36.6 52.2" +
    "C36.8 51.9 36.8 51.5 36.7 51.2C36.6 50.4 35.8 49.9 35 49.9zM43.29 53.99C42.84 54.01 42.41 54.22 42.1 54.6L37.2 63.5" +
    "C39.2 64.1 41.4 64.5 43.5 64.5C45 64.5 46.4 64.3 47.8 64C48.5 63.8 49.1 63.7 49.7 63.6L44.9 54.9" +
    "C44.6 54.6 44.4 54.4 44.1 54.2C43.84 54.05 43.56 53.98 43.29 53.99z",
};

// The elephant's body, with its outlines and eyes cut out. Padded, as it
// fills its square.
const postgresMark = {
  viewBox: "-5.4 -.4 265 265",
  path:
    "M186.05 14.76C177.63 14.57 170.11 15.63 164.61 16.77C177.4 22.42 186.76 30.78 193.18 38.26" +
    "C204.92 51.92 210.68 67.02 211.16 72.07C211.23 72.84 211.18 73.49 211.04 74.04C211.22 79.31 210.55 83.77 209.88 88.08" +
    "C209.34 91.58 208.79 95.2 208.65 99.42C208.52 103.4 209.03 107.64 209.57 112.13" +
    "C210.89 123.07 212.36 135.4 204.81 147.87L205.18 148.33A171.33 171.33 0 0 1 206.07 149.45" +
    "C235.28 103.48 245.37 50.19 236.21 38.48C224.26 23.21 207.39 15.23 186.05 14.76zM133.63 17.55" +
    "C119.85 17.32 108.55 21.55 100.06 30.11C86.58 43.71 82.97 65.2 82.02 76.7C82.65 76.35 83.32 75.99 84.05 75.62" +
    "C87.47 73.88 94.33 70.86 101.82 70.09C114.31 68.79 122.55 74.37 125.02 85.78C131.61 116.25 125.54 129.74 121.11 139.59" +
    "C120.29 141.41 119.51 143.14 118.89 144.8L118.34 146.28C117.03 149.78 115.83 153.02 115.13 155.89" +
    "C118.39 155.05 121.11 156.18 122.59 156.79C126.31 158.34 129.42 162.06 130.69 166.51A15.44 15.44 0 0 1 131.15 168.67" +
    "C131.26 169.04 131.32 169.44 131.31 169.84C130.45 200.44 131.66 228.15 134.12 234.29" +
    "C137.92 243.81 143.7 252.13 160.02 248.64L160.02 248.64C175.75 245.26 181.35 239.6 183.97 224.39" +
    "C185.96 212.88 189.87 180.29 190.42 173.33C189.74 159.46 197.22 156.68 201.39 155.48" +
    "C200.85 154.72 200.15 153.84 199.43 152.94C197.07 149.99 193.83 145.94 191.34 139.89" +
    "C190.95 138.96 189.74 136.78 188.33 134.26C180.66 120.51 164.7 91.88 175.02 77.13" +
    "C179.76 70.35 189.11 67.64 202.87 69.04C201.04 63.6 196.46 53.53 187.99 43.53C178.17 31.92 160.77 17.99 133.63 17.55z" +
    "M56.67 18.18C47.55 18.53 38.51 20.6 31.23 25.76C19.31 34.21 13.84 49.95 14.97 72.53" +
    "C15.32 79.66 19.84 105.12 27.57 130.44C36.5 159.7 46.63 178.15 55.36 181.07C56.7 181.51 60.77 182.87 66.67 175.78" +
    "C75.01 165.75 82.85 157.21 86.45 153.35A36.27 36.27 0 0 1 83.83 150.88C75.74 142.44 72.05 130.73 73.72 118.78" +
    "C75.52 105.91 74.85 94.55 74.48 88.44C74.39 86.77 74.31 85.56 74.32 84.71C74.26 82.49 73.77 46.19 94.8 24.95" +
    "C94.98 24.76 95.18 24.59 95.36 24.41C87.35 21.8 71.88 17.6 56.67 18.18zM194.34 76.08" +
    "C186.46 76.41 182.77 78.91 181.06 81.36C173.39 92.32 188.37 119.2 194.77 130.67" +
    "C196.41 133.62 197.6 135.75 198.15 137.08A36.51 36.51 0 0 0 200.1 141.09C204.5 131.72 203.36 122.23 202.26 113.01" +
    "C201.71 108.44 201.14 103.72 201.29 99.18C201.44 94.52 202.03 90.68 202.6 86.96C203.13 83.5 203.63 80.19 203.7 76.54" +
    "C200.06 76.09 196.97 75.96 194.34 76.08zM105.21 77.26C95.64 77.2 84.4 83.34 81.71 85.72" +
    "C81.74 86.34 81.79 87.12 81.84 88.01C82.22 94.37 82.92 106.2 81.02 119.79C79.66 129.5 82.63 138.97 89.15 145.78" +
    "C94.28 151.14 101.03 154.1 107.97 154.12C108.77 150.85 110.06 147.41 111.44 143.7L111.98 142.23" +
    "C112.69 140.34 113.51 138.51 114.38 136.57C118.62 127.15 123.89 115.44 117.82 87.34" +
    "C116.64 81.91 113.82 78.75 109.19 77.69C107.92 77.4 106.58 77.27 105.21 77.26zM192.55 79.54" +
    "C194.79 79.59 196.67 80.23 196.86 81.6C196.97 82.42 196.35 83.69 195.27 84.82C194.56 85.57 193.07 86.86 191.03 87.14" +
    "C190.77 87.18 190.51 87.2 190.25 87.2C187.2 87.2 184.62 84.76 184.42 83.34C184.17 81.5 187.17 80.11 190.25 79.68" +
    "C191.03 79.57 191.81 79.52 192.55 79.54zM106.13 81.38C106.75 81.38 107.41 81.43 108.08 81.53" +
    "C111.37 81.98 114.76 83.54 114.43 85.88C114.17 87.74 111.22 90.27 107.97 90.27C107.7 90.27 107.41 90.25 107.13 90.21" +
    "C105.19 89.94 103.22 88.71 101.88 86.93C101.4 86.3 100.63 85.08 100.78 83.97C100.84 83.56 101.05 82.95 101.74 82.43" +
    "C102.66 81.73 104.26 81.36 106.13 81.38zM205.54 161.94C205.04 162.11 204.51 162.26 203.98 162.41" +
    "C200.57 163.36 197.36 164.27 197.78 172.91C198.67 173.64 202.48 174.83 209.96 174.48" +
    "C216.73 174.16 223.86 172.68 228.12 170.7C233.08 168.4 236.57 165.84 238.61 163.93L238.45 163.96" +
    "C223.31 167.09 212.58 166.66 206.57 162.69C206.19 162.44 205.85 162.19 205.54 161.94zM117.47 162.96" +
    "C116.55 162.98 115.61 163.49 114.26 165.01L114.25 165.02C112.63 166.82 111.46 168.31 110.43 169.62" +
    "C105.94 175.33 103.88 177.27 90.54 180.01C87.78 180.58 86.19 181.15 85.3 181.57C86.37 182.3 88.19 183.18 90.22 183.69" +
    "C100.05 186.15 113.74 186.31 123.14 173.16C124.25 171.6 123.91 169.6 123.61 168.54" +
    "C122.97 166.3 121.39 164.27 119.77 163.6C118.89 163.24 118.19 162.95 117.47 162.96z",
};

/** serviceMarks name the vendor behind each connector kind. */
const serviceMarks = {
  teams: idpMarks.microsoft,
  onedrive: idpMarks.microsoft,
  googlechat: googleMark,
  gdrive: googleMark,
  slack: slackMark,
  github: idpMarks.github,
  gitlab: gitlabMark,
  jira: atlassianMark,
  confluence: atlassianMark,
  hubspot: hubspotMark,
  salesforce: salesforceMark,
  aws: awsMark,
  kubernetes: kubernetesMark,
  postgres: postgresMark,
  gdocs: googleMark,
  gsheets: googleMark,
  gslides: googleMark,
  gcalendar: googleMark,
  gmail: googleMark,
  gcp: googleMark,
  outlook: idpMarks.microsoft,
  azure: idpMarks.microsoft,
};

/** serviceMark is a connector kind's mark at text size, or null. */
function serviceMark(kind) {
  return drawMark(serviceMarks[kind]);
}

/** categoryIcons stand in for the marks of kinds that have none, by the
 *  category the control plane gives them. */
const categoryIcons = {
  Chat: icons.chat,
  Files: icons.folder,
  Code: icons.terminal,
  "Projects and wikis": icons.board,
  Documents: icons.file,
  "Email and calendar": icons.calendar,
  CRM: icons.person,
  Data: icons.database,
  Cloud: icons.cloud,
};

/** kindMark is a connector kind's mark, or its category's icon. kinds is
 *  /connection-kinds' answer. */
export function kindMark(kind, kinds) {
  const d = (kinds || []).find((k) => k.kind === kind);
  return serviceMark(kind) || icon(categoryIcons[d && d.category] || icons.connections);
}

/** remembered is a setting kept for this tab, or with lasting for every visit,
 *  or null. */
export function remembered(key, { lasting } = {}) {
  try {
    return (lasting ? localStorage : sessionStorage).getItem(key);
  } catch {
    return null;
  }
}

/** remember keeps a setting, or forgets it when value is null. */
export function remember(key, value, { lasting } = {}) {
  try {
    const store = lasting ? localStorage : sessionStorage;
    if (value === null) store.removeItem(key);
    else store.setItem(key, value);
  } catch {
    // Nothing to do: the setting holds until the page reloads.
  }
}

/** The reporting windows, as Keera Gateway's. */
const RANGES = [
  { label: "24h", since: "24h", long: "Last 24 hours" },
  { label: "7d", since: "168h", long: "Last 7 days" },
  { label: "30d", since: "720h", long: "Last 30 days" },
];

/** rangeOf returns fallback when no entry matches. */
export function rangeOf(since, fallback) {
  return RANGES.find((r) => r.since === since) || fallback;
}

/** currentRange is the chosen window, or the last 7 days. */
export function currentRange() {
  const since = remembered("keera.range");
  return rangeOf(since) ? since : "168h";
}

/** rangePicker sets the window and reloads the screen. */
export function rangePicker(ctx, since) {
  return h(
    "div",
    { class: "seg" },
    RANGES.map((r) =>
      h(
        "button",
        {
          type: "button",
          "aria-pressed": String(r.since === since),
          title: r.long,
          onClick: () => {
            remember("keera.range", r.since);
            ctx.reload();
          },
        },
        r.label,
      ),
    ),
  );
}

/** pill is a short label. dot puts a dot before it, as for a state. */
export function pill(text, tone, { dot = false } = {}) {
  return h("span", { class: "pill" + (tone ? " pill-" + tone : "") }, dot ? h("span", { class: "dot" }) : null, text);
}

/** rowLink is a row's name as a link to its screen. A table with `open` also
 *  opens the row on any click in it; the link stays for the keyboard and for
 *  new tabs. */
export function rowLink(ctx, path, text, title) {
  return link(ctx, path, text, { class: "row-link", title: title || null });
}

/** link is a link to a screen of the panel. props such as a class go on the
 *  <a>; text may be nodes. */
export function link(ctx, path, text, props) {
  return h("a", { ...props, href: path, onClick: go(ctx, path) }, text);
}

/** go routes inside the panel instead of reloading it. The href stays the
 *  real path, so the link still opens in a new tab. */
export function go(ctx, path) {
  return (e) => {
    e.preventDefault();
    ctx.navigate(path);
  };
}

/** isAdmin reports whether the signed-in person administers the
 *  organisation: its connectors, its audit log and every agent in it. */
export function isAdmin(ctx) {
  return !!ctx.state.me.can_admin;
}

/** checkButton is a Check button with its verdict beside it once run.
 *  verdict turns run's result into { label, tone, title, open }: a pill that
 *  opens the full report. The result is not kept, as it is only true now. */
export function checkButton(title, run, verdict) {
  const slot = h("span");
  const button = h(
    "button",
    {
      class: "btn btn-sm btn-quiet",
      title,
      onClick: async () => {
        button.disabled = true;
        slot.replaceChildren(h("span", { class: "faint nowrap" }, h("span", { class: "blip" }), " Checking…"));
        try {
          slot.replaceChildren(verdictPill(verdict(await run())));
        } catch (ex) {
          slot.replaceChildren(h("span", { class: "pill pill-bad" }, "Check failed"));
          toast(ex.message, "bad");
        } finally {
          button.disabled = false;
        }
      },
    },
    "Check",
  );
  return h("div", { class: "row-tight" }, button, slot);
}

function verdictPill({ label, tone, title, open }) {
  return h(
    "button",
    { class: "pill pill-" + tone + " pill-button", title, onClick: open },
    h("span", { class: "dot" }),
    label,
  );
}

export function crumb(ctx, path, label) {
  return link(ctx, path, [icon(icons.back), label], { class: "crumb" });
}

/** gone is the detail screen for something that is not there. */
export function gone(ctx, path, label, title, body) {
  return h(
    "div",
    {},
    h("div", { class: "detail-head" }, crumb(ctx, path, label)),
    h("div", { class: "card" }, empty(title, body)),
  );
}

/** detailHead is the top strip of a detail screen: back link, chips, then
 *  controls. */
export function detailHead(ctx, [path, label], chips, controls) {
  return h(
    "div",
    { class: "detail-head" },
    crumb(ctx, path, label),
    h(
      "div",
      { class: "row", style: { flexWrap: "wrap" } },
      h("div", { class: "wrap-chips" }, chips),
      h("div", { class: "spacer" }),
      controls,
    ),
  );
}

/** busyButton is disabled while run works. A failure is toasted. */
export function busyButton(props, label, run) {
  const button = h(
    "button",
    {
      class: "btn",
      ...props,
      onClick: async () => {
        button.disabled = true;
        try {
          await run();
        } catch (ex) {
          toast(ex.message, "bad");
        } finally {
          button.disabled = false;
        }
      },
    },
    label,
  );
  return button;
}

/** iconButton shows only an icon. title is its tooltip and, unless label is
 *  given, what a screen reader reads.
 *
 *  A row's edit, delete and copy buttons are icons alone: they scan well
 *  down a column, and everyone knows these icons. Every other action is a
 *  word. Above a list or on a detail screen, actions are words too; only a
 *  button that creates something has an icon, the plus. */
export function iconButton(glyph, title, onClick, { quiet, danger, cls, label = title, ...attrs } = {}) {
  return h(
    "button",
    {
      class: ["btn", quiet && "btn-quiet", "btn-sm", danger && "btn-danger", cls].filter(Boolean).join(" "),
      title,
      "aria-label": label,
      onClick,
      ...attrs,
    },
    icon(glyph),
  );
}

/** listHead is the strip above a list. add is { label, onClick }, or null
 *  when the reader cannot add. */
export function listHead(intro, controls, add) {
  return h(
    "div",
    { class: "detail-head" },
    intro ? h("div", { class: "muted" }, intro) : null,
    h(
      "div",
      { class: "row list-actions", style: { flexWrap: "wrap" } },
      h("div", { class: "spacer" }),
      controls,
      add ? h("button", { class: "btn btn-primary", onClick: add.onClick }, icon(icons.plus), add.label) : null,
    ),
  );
}

/** sectionHead is a heading between tables. A text note shows faint on the
 *  right; anything else, such as buttons, goes there as is. */
export function sectionHead(title, note, style) {
  return h(
    "div",
    { class: "section-head", style },
    h("h2", {}, title),
    h("div", { class: "spacer" }),
    typeof note === "string" ? h("span", { class: "faint" }, note) : note,
  );
}

/** fact is one labelled detail in a list of them; empty shows a dash. */
export function fact(label, value) {
  return h("div", { class: "fact" }, h("span", { class: "muted small" }, label), h("span", {}, value || dash()));
}

/** stat is a figure in a card, with its label and an optional note below. */
export function stat(label, value, note) {
  return h(
    "div",
    { class: "card stat" },
    h("div", { class: "stat-label" }, label),
    h("div", { class: "stat-value" }, value),
    note ? h("div", { class: "stat-note" }, note) : null,
  );
}

/** loadMore adds the next page to the same table, so the reader keeps their
 *  place. next returns how many rows came; fewer than size shows end. */
export function loadMore(size, next, end) {
  const label = `Load ${size} more`;
  const wrap = h("div", { style: { marginTop: "12px", textAlign: "center" } });
  const more = h(
    "button",
    {
      class: "btn",
      onClick: async () => {
        more.disabled = true;
        more.textContent = "Loading…";
        try {
          if ((await next()) < size) {
            wrap.replaceChildren(h("div", { class: "hint" }, end));
            return;
          }
        } catch (ex) {
          wrap.append(banner("bad", ex.message));
        } finally {
          more.disabled = false;
          more.textContent = label;
        }
      },
    },
    label,
  );
  wrap.append(more);
  return wrap;
}

export function dash() {
  return h("span", { class: "faint" }, "-");
}

export function empty(title, body) {
  return h("div", { class: "empty" }, h("strong", {}, title), body || null);
}

/** listCard is a card with a short list of miniRows, such as the latest
 *  tasks. more, such as a link to the whole list, goes beside the title. With
 *  no rows it shows empty(emptyTitle, emptyBody). */
export function listCard({ title, more, rows, emptyTitle, emptyBody }) {
  return card(title, rows.length ? h("div", { class: "mini-list" }, rows) : empty(emptyTitle, emptyBody), {
    more,
    style: { alignSelf: "start" },
    bodyClass: null,
  });
}

/** miniRow is one row of a listCard, linking to its screen: a title with a
 *  faint line below it, and end, such as a state, on the right. */
export function miniRow(ctx, path, title, below, end) {
  return link(
    ctx,
    path,
    [
      h(
        "div",
        { class: "stack", style: { minWidth: "0" } },
        h("span", { class: "mini-title" }, title),
        h("span", { class: "faint small row-tight" }, below),
      ),
      end,
    ],
    { class: "mini-row" },
  );
}

function toggleBox(t, checked, onChange) {
  return h(
    "label",
    { class: "row-tight" },
    h("input", {
      type: "checkbox",
      checked,
      onChange: (e) => onChange(e.target.checked),
    }),
    t.label,
  );
}

/** table renders rows in a card, with search, filters and sorting.
 *
 *  opts:
 *    search       (row) => string - the text the search box matches. Passing
 *                 it adds the box.
 *    searchLabel  what is being searched, for the placeholder
 *    toggles      [{ label, test, hidden, on, onChange }] - a checkbox that,
 *                 while checked, keeps only the rows test() accepts. hidden()
 *                 instead drops those rows while unchecked. `on` is whether it
 *                 starts checked. onChange(checked) is for a toggle whose
 *                 caller fetches a different list.
 *    choices      [{ label, options: [{ value, label }], test }] - a dropdown
 *                 that keeps the rows test(row, value) accepts. Its first
 *                 option is "All".
 *    sortBy       the label of the column to sort by at first
 *    sortDir      "asc" (the default) or "desc"
 *    rowClass     (row) => string - a class for that row's <tr>
 *    open         (row) => path - the row's screen, opened by a click anywhere
 *                 on the row. Needs `ctx`. Null leaves that row closed.
 *    pick         (row) => void - what a click or Enter on the row does when
 *                 it has no screen of its own.
 *    pickTitle    what pick does, for the tooltip
 *    emptyTitle / emptyBody
 *    tile         (row) => node - the row as a tile. Passing it adds a switch
 *                 between the table and a grid of tiles. Tiles show until
 *                 someone chooses the table. Pass sortBy with it: the tiles'
 *                 sort dropdown has no "unsorted" choice.
 *
 *  A column with a sortKey gets a header that sorts by it, first in its
 *  sortDir. Sorting happens here, so a paged list has no sortKeys: sorting
 *  one page would rank the wrong rows.
 *
 *  A column with a `width` keeps it, so the table does not shift on redraw.
 */
export function table(columns, rows, opts = {}) {
  const toggles = opts.toggles || [];
  const choices = opts.choices || [];

  if (!rows.length) {
    const card = h("div", { class: "card" }, empty(opts.emptyTitle || "Nothing here yet", opts.emptyBody));
    // Over an empty table, only a toggle that fetches its own list can bring
    // rows back.
    const fetches = toggles.filter((t) => t.onChange);
    if (!fetches.length) return card;
    return h(
      "div",
      {},
      h(
        "div",
        { class: "toolbar" },
        fetches.map((t) => toggleBox(t, !!t.on, (v) => t.onChange(v))),
        h("div", { class: "spacer" }),
      ),
      card,
    );
  }

  const view = {
    q: "",
    on: toggles.map((t) => !!t.on),
    picked: choices.map(() => ""),
    sort: columns.find((c) => c.sortKey && c.label === opts.sortBy) || null,
    dir: opts.sortDir === "desc" ? -1 : 1,
    tiles: !!opts.tile,
  };

  const body = h("tbody");
  const grid = opts.tile ? h("div", { class: "table-tiles" }) : null;
  const count = h("span", { class: "faint nowrap small" });
  const sortable = new Map();

  const heads = columns.map((c) => {
    const attrs = {
      class: cellClass(c),
      style: c.width ? { width: c.width } : null,
    };
    if (!c.sortKey) return h("th", attrs, c.label);
    const arrow = h("span", { class: "sort-arrow", "aria-hidden": "true" });
    const th = h(
      "th",
      attrs,
      h(
        "button",
        {
          class: "th-sort",
          title: `Sort by ${c.label.toLowerCase()}`,
          onClick: () => {
            if (view.sort === c) view.dir = -view.dir;
            else {
              view.sort = c;
              view.dir = c.sortDir === "desc" ? -1 : 1;
            }
            draw();
          },
        },
        c.label,
        arrow,
      ),
    );
    sortable.set(c, { th, arrow });
    return th;
  });

  function visible() {
    let out = rows;
    const q = view.q.trim().toLowerCase();
    if (q && opts.search) {
      out = out.filter((r) =>
        String(opts.search(r) ?? "")
          .toLowerCase()
          .includes(q),
      );
    }
    toggles.forEach((t, i) => {
      if (view.on[i]) {
        if (t.test) out = out.filter(t.test);
      } else if (t.hidden) {
        out = out.filter((r) => !t.hidden(r));
      }
    });
    choices.forEach((c, i) => {
      const v = view.picked[i];
      if (v) out = out.filter((r) => c.test(r, v));
    });
    if (view.sort) out = out.slice().sort(order(view.sort.sortKey, view.dir));
    return out;
  }

  const none = "Nothing matches. Clear the search, or turn a filter off.";

  function clickable(el, row) {
    const path = opts.open ? opts.open(row) : null;
    if (path) openRow(el, opts.ctx, path);
    else if (opts.pick) pickRow(el, () => opts.pick(row), opts.pickTitle);
    return el;
  }

  function setLayout(tiles) {
    view.tiles = tiles;
    card.hidden = tiles;
    grid.hidden = !tiles;
    tileControls.hidden = !tiles;
    tableButton.setAttribute("aria-pressed", String(!tiles));
    tilesButton.setAttribute("aria-pressed", String(tiles));
    draw();
  }

  function draw() {
    const shown = visible();
    count.textContent = shown.length === rows.length ? "" : `${shown.length} of ${rows.length}`;
    if (sortSelect) {
      sortSelect.value = String(sortCols.indexOf(view.sort));
      sortDir.textContent = view.dir === 1 ? "↑" : "↓";
      sortDir.disabled = !view.sort;
    }
    if (view.tiles) {
      replace(
        grid,
        shown.length
          ? shown.map((row) => clickable(opts.tile(row), row))
          : h("div", { class: "card table-none" }, none),
      );
      return;
    }
    replace(
      body,
      shown.length
        ? shown.map((row) =>
            clickable(
              h(
                "tr",
                { class: (opts.rowClass && opts.rowClass(row)) || null },
                columns.map((c) => h("td", { class: cellClass(c) }, c.cell(row))),
              ),
              row,
            ),
          )
        : h("tr", {}, h("td", { colspan: String(columns.length), class: "table-none" }, none)),
    );
    for (const [c, { th, arrow }] of sortable) {
      const active = view.sort === c;
      arrow.textContent = active ? (view.dir === 1 ? "↑" : "↓") : "";
      if (active) th.setAttribute("aria-sort", view.dir === 1 ? "ascending" : "descending");
      else th.removeAttribute("aria-sort");
    }
  }

  // Tiles have no headers, so they sort with a dropdown. Both change the same
  // sort, so switching keeps the order.
  const sortCols = columns.filter((c) => c.sortKey);
  const sortSelect =
    grid && sortCols.length
      ? h(
          "select",
          {
            class: "select toolbar-select",
            "aria-label": "Sort by",
            onChange: (e) => {
              const c = sortCols[Number(e.target.value)] || null;
              view.sort = c;
              if (c) view.dir = c.sortDir === "desc" ? -1 : 1;
              draw();
            },
          },
          sortCols.map((c, i) => h("option", { value: String(i) }, `Sort by ${c.label.toLowerCase()}`)),
        )
      : null;
  const sortDir = sortSelect
    ? h("button", {
        class: "btn btn-sort",
        type: "button",
        title: "Reverse the order",
        "aria-label": "Reverse the order",
        onClick: () => {
          view.dir = -view.dir;
          draw();
        },
      })
    : null;
  const tileControls = grid ? h("div", { class: "row-tight" }, sortSelect, sortDir) : null;
  const layoutButton = (tiles, path, label) =>
    h("button", { type: "button", title: label, "aria-label": label, onClick: () => setLayout(tiles) }, icon(path));
  const tableButton = grid ? layoutButton(false, icons.rows, "Table") : null;
  const tilesButton = grid ? layoutButton(true, icons.tiles, "Tiles") : null;

  let toolbar = null;
  if (opts.search || toggles.length || choices.length || grid) {
    const label = opts.searchLabel ? `Search ${opts.searchLabel}` : "Search";
    toolbar = h(
      "div",
      { class: "toolbar" },
      opts.search
        ? h("input", {
            class: "input toolbar-search",
            type: "search",
            placeholder: label,
            "aria-label": label,
            autocomplete: "off",
            spellcheck: "false",
            onInput: (e) => {
              view.q = e.target.value;
              draw();
            },
          })
        : null,
      choices.map((c, i) =>
        h(
          "select",
          {
            class: "select toolbar-select",
            "aria-label": c.label,
            onChange: (e) => {
              view.picked[i] = e.target.value;
              draw();
            },
          },
          h("option", { value: "" }, `All ${c.label.toLowerCase()}`),
          c.options.map((o) => h("option", { value: o.value }, o.label)),
        ),
      ),
      toggles.map((t, i) =>
        toggleBox(t, view.on[i], (v) => {
          view.on[i] = v;
          draw();
          if (t.onChange) t.onChange(v);
        }),
      ),
      // One group, so a narrow screen wraps it whole, still on the right.
      h(
        "div",
        { class: "row toolbar-end" },
        count,
        tileControls,
        grid
          ? h("div", { class: "seg seg-icons", role: "group", "aria-label": "Layout" }, tableButton, tilesButton)
          : null,
      ),
    );
  }

  const card = h(
    "div",
    { class: "card" },
    h("div", { class: "table-wrap" }, h("table", {}, h("thead", {}, h("tr", {}, heads)), body)),
  );
  if (grid) setLayout(view.tiles);
  else draw();
  const el = toolbar ? h("div", {}, toolbar, card, grid) : card;
  // Load more pushes rows and calls this. Calling table() again would lose
  // the reader's search and sort.
  el.redraw = draw;
  return el;
}

/** ownClick reports whether a click in a row is not the row's to handle: a
 *  link, button or field in the row does its own thing, and selecting text
 *  to copy it is not a click. */
function ownClick(e) {
  return !!e.target.closest("a, button, input, select, textarea, label") || !!getSelection().toString();
}

/** openRow opens a row's screen on a click anywhere in it, and in a new tab
 *  on a middle or modified click, like a link. */
function openRow(el, ctx, path) {
  const open = (e) => {
    if (ownClick(e)) return;
    if (e.type === "auxclick" && e.button !== 1) return;
    if (e.button === 1 || e.ctrlKey || e.metaKey || e.shiftKey) window.open(path, "_blank");
    else ctx.navigate(path);
  };
  el.classList.add("row-opens");
  el.addEventListener("click", open);
  el.addEventListener("auxclick", open);
}

/** pickRow runs pick on a click in the row, or on Enter or Space. The row
 *  takes focus, as it has no link to tab to. */
function pickRow(el, pick, title) {
  el.classList.add("row-opens");
  el.tabIndex = 0;
  if (title) el.title = title;
  el.addEventListener("click", (e) => {
    if (!ownClick(e)) pick();
  });
  el.addEventListener("keydown", (e) => {
    if (e.target !== el || (e.key !== "Enter" && e.key !== " ")) return;
    e.preventDefault();
    pick();
  });
}

function cellClass(c) {
  return (
    [
      c.num && "num",
      c.shrink && "shrink",
      // A column without a heading holds the row's buttons.
      c.label === "" && "actions",
    ]
      .filter(Boolean)
      .join(" ") || null
  );
}

/** order keeps rows with no value at the bottom in either direction: a key
 *  never used is not the oldest. */
function order(key, dir) {
  const missing = (v) => v === null || v === undefined || v === "";
  return (a, b) => {
    const x = key(a);
    const y = key(b);
    if (missing(x) || missing(y)) return missing(x) && missing(y) ? 0 : missing(x) ? 1 : -1;
    if (typeof x === "number" && typeof y === "number") return (x - y) * dir;
    return (
      String(x).localeCompare(String(y), undefined, {
        numeric: true,
        sensitivity: "base",
      }) * dir
    );
  };
}

let openMenu = null;

/** menu opens a list of actions next to anchor. Each item is
 *  { label, icon, onClick, hint }, { label, icon, href } for a link in a new
 *  tab, a separator, or a node shown as it is. */
export function menu(anchor, items, { up = false } = {}) {
  // A second click on the anchor closes its menu.
  if (anchor.getAttribute("aria-expanded") === "true") return closeMenu();
  closeMenu();
  const list = h("div", { class: "menu", role: "menu" });
  for (const item of items) {
    if (!item) continue;
    if (item instanceof Node) {
      list.append(item);
      continue;
    }
    if (item.separator) {
      list.append(h("div", { class: "menu-sep", role: "separator" }));
      continue;
    }
    list.append(
      h(
        item.href ? "a" : "button",
        {
          class: "menu-item",
          role: "menuitem",
          type: item.href ? null : "button",
          href: item.href || null,
          target: item.href ? "_blank" : null,
          rel: item.href ? "noopener" : null,
          onClick: (e) => {
            closeMenu();
            if (item.onClick) item.onClick(e);
          },
        },
        icon(item.icon),
        h(
          "span",
          { class: "menu-label" },
          h("span", {}, item.label),
          item.hint ? h("span", { class: "menu-hint" }, item.hint) : null,
        ),
      ),
    );
  }
  document.body.append(list);
  place(list, anchor, up);
  anchor.setAttribute("aria-expanded", "true");

  const onDown = (e) => {
    if (!list.contains(e.target) && !anchor.contains(e.target)) closeMenu();
  };
  const onKey = (e) => {
    if (e.key === "Escape") {
      closeMenu();
      anchor.focus();
    }
    if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      e.preventDefault();
      const items = [...list.querySelectorAll(".menu-item")];
      const i = items.indexOf(document.activeElement);
      const next = e.key === "ArrowDown" ? i + 1 : i - 1;
      items[(next + items.length) % items.length]?.focus();
    }
  };
  setTimeout(() => document.addEventListener("mousedown", onDown), 0);
  document.addEventListener("keydown", onKey);
  window.addEventListener("resize", closeMenu);
  openMenu = () => {
    list.remove();
    anchor.setAttribute("aria-expanded", "false");
    document.removeEventListener("mousedown", onDown);
    document.removeEventListener("keydown", onKey);
    window.removeEventListener("resize", closeMenu);
  };
  list.querySelector(".menu-item")?.focus({ preventScroll: true });
  return list;
}

function closeMenu() {
  if (openMenu) {
    const close = openMenu;
    openMenu = null;
    close();
  }
}

/** place puts a floating element beside its anchor, on screen. The page is
 *  zoomed by --ui-scale: measurements come back zoomed, but top and left are
 *  zoomed again when set, so they are divided by the zoom first. */
function place(el, anchor, up) {
  const a = anchor.getBoundingClientRect();
  const zoom = a.width / anchor.offsetWidth || 1;
  const m = el.getBoundingClientRect();
  const gap = 6;
  let top = up ? a.top - m.height - gap : a.bottom + gap;
  if (!up && top + m.height > window.innerHeight - 8) top = a.top - m.height - gap;
  if (top < 8) top = 8;
  const left = Math.max(8, Math.min(a.left, window.innerWidth - m.width - 8));
  el.style.top = top / zoom + "px";
  el.style.left = left / zoom + "px";
}

/** modal opens a dialog and returns a close function. */
export function modal({ title, subtitle, body, actions, wide }) {
  const overlay = h("div", { class: "overlay" });
  // Focus returns here on close, so a keyboard user keeps their place.
  const opener = document.activeElement;
  let open = true;
  const close = () => {
    if (!open) return;
    open = false;
    overlay.remove();
    document.removeEventListener("keydown", onKey, true);
    if (opener && opener.isConnected) opener.focus();
  };
  const onKey = (e) => {
    if (e.key === "Escape") return close();
    if (e.key === "Tab") trapTab(card, e);
  };
  document.addEventListener("keydown", onKey, true);
  overlay.addEventListener("mousedown", (e) => {
    if (e.target === overlay) close();
  });

  const titleID = "modal-title-" + Math.random().toString(36).slice(2, 8);
  const card = h(
    "div",
    {
      class: "modal" + (wide ? " modal-wide" : ""),
      role: "dialog",
      "aria-modal": "true",
      "aria-labelledby": titleID,
    },
    h(
      "div",
      { class: "modal-head" },
      h(
        "div",
        { class: "stack" },
        h("h2", { id: titleID }, title),
        subtitle ? h("div", { class: "page-sub" }, subtitle) : null,
      ),
      h("div", { class: "spacer" }),
      h(
        "button",
        {
          class: "btn btn-quiet btn-sm",
          "aria-label": "Close",
          onClick: close,
        },
        icon(icons.close),
      ),
    ),
    h("div", { class: "modal-body" }, body),
    h(
      "div",
      { class: "modal-foot" },
      actions ? actions(close) : h("button", { class: "btn", onClick: close }, "Close"),
    ),
  );
  overlay.append(card);
  document.body.append(overlay);

  // The actions are outside the body's form, so Enter is wired to the primary
  // action here.
  const form = card.querySelector("form");
  const primary = card.querySelector(".modal-foot .btn-primary, .modal-foot .btn-danger");
  if (form && primary) {
    // A browser submits on Enter only if the form has a submit button or a
    // single text field. This hidden one comes first, as the default.
    if (!form.querySelector("[type=submit]")) {
      form.prepend(
        h("button", {
          type: "submit",
          class: "submit-proxy",
          tabindex: "-1",
          "aria-hidden": "true",
        }),
      );
    }
    form.addEventListener("submit", (e) => {
      e.preventDefault();
      if (!primary.disabled) primary.click();
    });
  }

  const first = [...card.querySelectorAll("input:not([type=hidden]), select, textarea, button.btn-primary")].find(
    (el) => !el.disabled && el.offsetParent !== null,
  );
  if (first) first.focus();
  return close;
}

/** trapTab keeps Tab inside the dialog. */
function trapTab(card, e) {
  const items = [
    ...card.querySelectorAll(
      "a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), " +
        "textarea:not([disabled]), [tabindex]:not([tabindex='-1'])",
    ),
  ].filter((el) => el.tabIndex >= 0 && (el.offsetParent !== null || el === document.activeElement));
  if (!items.length) return;
  const firstEl = items[0];
  const lastEl = items[items.length - 1];
  if (e.shiftKey && document.activeElement === firstEl) {
    e.preventDefault();
    lastEl.focus();
  } else if (!e.shiftKey && document.activeElement === lastEl) {
    e.preventDefault();
    firstEl.focus();
  }
}

/** formModal is a dialog that does one thing with its primary button.
 *  submit(close) returns false to keep the dialog open, as after focusing an
 *  empty field. A failure, or a thrown { message } of text or nodes, shows in
 *  the dialog. On success it closes, toasts done (text, or a function of
 *  submit's result) and reloads ctx, each if given. lead(close) returns
 *  actions for the left of the footer, such as Delete. */
export function formModal({ title, subtitle, wide, body, submitLabel = "Save", danger, submit, done, ctx, lead }) {
  const err = h("div");
  body.prepend(err);
  let button;
  const close = modal({
    title,
    subtitle,
    wide,
    body,
    actions: (close) => [
      lead ? [lead(close), h("div", { class: "spacer" })] : null,
      h("button", { class: "btn", onClick: close }, "Cancel"),
      (button = h(
        "button",
        {
          class: "btn " + (danger ? "btn-danger" : "btn-primary"),
          onClick: async () => {
            button.disabled = true;
            showError(err, "");
            try {
              const result = await submit(close);
              if (result === false) {
                button.disabled = false;
                return;
              }
              close();
              const said = typeof done === "function" ? done(result) : done;
              if (said) toast(said, "good");
              if (ctx) ctx.reload();
            } catch (ex) {
              showError(err, (ex && ex.message) || "That did not work.");
              button.disabled = false;
            }
          },
        },
        submitLabel,
      )),
    ],
  });
  return { close, button };
}

/** confirm asks before something irreversible. A failure shows in the
 *  dialog. done and ctx work as in formModal. */
export function confirm({ title, body, confirmLabel = "Confirm", danger, onConfirm, done, ctx }) {
  formModal({
    title,
    body: h("div", {}, h("div", { class: "muted" }, body)),
    submitLabel: confirmLabel,
    danger,
    submit: onConfirm,
    done,
    ctx,
  });
}

let toastHost;

export function toast(message, tone = "") {
  if (!toastHost) {
    // A live region, so screen readers announce toasts.
    toastHost = h("div", {
      class: "toasts",
      role: "status",
      "aria-live": "polite",
    });
    document.body.append(toastHost);
  }
  const el = h("div", { class: "toast " + tone }, message);
  toastHost.append(el);
  // Bad news and warnings stay longer: they say what to do next.
  setTimeout(() => el.remove(), tone === "bad" || tone === "warn" ? 7000 : 3500);
}

export async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
    toast("Copied to the clipboard", "good");
    return true;
  } catch {
    // Clipboard access needs a secure context, which a plain-http demo is not.
    toast("Could not copy. Select the text and copy it by hand.", "bad");
    return false;
  }
}

/** secretField shows a value with a Copy button. hidden masks it until
 *  someone presses Show, so it is not on screen for anyone looking on. Copy
 *  works either way. */
export function secretField(value, ariaLabel, { hidden = false } = {}) {
  const input = h("input", {
    class: "input key-value mono",
    type: hidden ? "password" : "text",
    autocomplete: "off",
    readonly: true,
    value,
    "aria-label": ariaLabel,
    // Selecting on focus helps copy by hand: plain http on anything but
    // localhost has no clipboard.
    onFocus: (e) => e.target.select(),
    onClick: (e) => e.target.select(),
  });
  let reveal = null;
  if (hidden) {
    const draw = () => {
      const shown = input.type === "text";
      reveal.setAttribute("aria-pressed", String(shown));
      replace(reveal, icon(shown ? icons.eyeOff : icons.eye), shown ? "Hide" : "Show");
    };
    reveal = h("button", {
      type: "button",
      class: "btn",
      "aria-label": `Show ${ariaLabel}`,
      onClick: () => {
        input.type = input.type === "text" ? "password" : "text";
        draw();
      },
    });
    draw();
  }
  return h(
    "div",
    { class: "row-tight" },
    input,
    reveal,
    // A button, not a submit, since it often sits in a form.
    h("button", { type: "button", class: "btn", onClick: () => copyText(value) }, icon(icons.copy), "Copy"),
  );
}

/** banner is a coloured note. tone is bad, warn, info or good. Attributes,
 *  such as a style, may come before the children as a plain object. A bad
 *  one is an alert, so a screen reader says it when it appears. */
export function banner(tone, ...children) {
  const first = children[0];
  const props = first && Object.getPrototypeOf(first) === Object.prototype ? children.shift() : {};
  const role = tone === "bad" ? { role: "alert" } : {};
  return h("div", { ...role, ...props, class: `banner banner-${tone}` }, ...children);
}

/** showError shows a failure in a dialog's error host and scrolls to it, as
 *  it is out of sight in a tall dialog. No message clears the host. */
function showError(host, message) {
  if (!message) {
    replace(host);
    return;
  }
  replace(host, banner("bad", message));
  // "nearest" so a dialog already showing it does not jump. jsdom and very
  // old browsers lack scrollIntoView.
  if (host.scrollIntoView) {
    host.scrollIntoView({ block: "nearest" });
  }
}

/** modeTiles asks for a mode as tiles with a sentence each, since a long
 *  option list goes unread. Once chosen, the tiles fold into one row with a
 *  Change button.
 *
 *  modes is a list of { value, icon, name, what }, with an optional mark()
 *  that draws a logo in place of the icon. chosen is the starting value, or
 *  null. onChoose runs after each choice; the caller moves focus.
 *  choose(value) picks a mode from code. */
export function modeTiles(modes, chosen, onChoose) {
  const tiles = h(
    "div",
    { class: "tiles" },
    modes.map((m) =>
      h(
        "button",
        {
          class: "tile",
          type: "button",
          "aria-pressed": String(m.value === chosen),
          onClick: () => choose(m.value),
        },
        h("span", { class: "tile-mark" }, markOf(m)),
        h("span", { class: "tile-name" }, m.name),
        h("span", { class: "tile-what" }, m.what),
      ),
    ),
  );
  const chosenRow = h("div", { class: "tile-chosen", hidden: true });
  const change = h(
    "button",
    {
      class: "btn btn-sm",
      type: "button",
      onClick: () => {
        tiles.hidden = false;
        chosenRow.hidden = true;
        tiles.firstChild.focus();
      },
    },
    "Change",
  );
  const choose = (value) => {
    const m = modes.find((x) => x.value === value) || modes[0];
    tiles.childNodes.forEach((tile, i) => {
      tile.setAttribute("aria-pressed", String(modes[i].value === value));
    });
    chosenRow.replaceChildren(
      markOf(m),
      h(
        "div",
        { class: "stack", style: { gap: "1px" } },
        h("span", { class: "tile-name" }, m.name),
        h("span", { class: "tile-what" }, m.what),
      ),
      h("div", { class: "spacer" }),
      change,
    );
    tiles.hidden = true;
    chosenRow.hidden = false;
    onChoose(value);
  };
  return { tiles, chosenRow, change, choose };
}

function markOf(m) {
  return (m.mark && m.mark()) || icon(m.icon);
}

let controls = 0;

/** controlID returns the control's id, giving it one if needed, so its label
 *  focuses it and screen readers name it. el may wrap a single control. */
function controlID(el) {
  if (!(el instanceof Element)) return null;
  const control = el.matches("input, select, textarea")
    ? el
    : el.querySelectorAll("input, select, textarea").length === 1
      ? el.querySelector("input, select, textarea")
      : null;
  if (!control) return null;
  if (!control.id) control.id = "field-" + ++controls;
  return control.id;
}

/** field renders a labelled control. hint may be a .hint element the caller
 *  changes later; extra goes below it. */
export function field(label, input, hint, ...extra) {
  return h(
    "div",
    { class: "field" },
    h("label", { for: controlID(input) }, label),
    input,
    hint instanceof Element && hint.classList.contains("hint") ? hint : hint ? h("div", { class: "hint" }, hint) : null,
    extra,
  );
}

/* Helpers the gateway panel does not need. */

/** card is a card with a title. more, such as a link or a pill, goes beside
 *  the title. The body goes in a div of bodyClass, or as it is when that is
 *  null. */
export function card(title, body, { more, style, bodyClass = "card-body" } = {}) {
  return h(
    "div",
    { class: "card", style },
    h("div", { class: "card-head" }, h("h2", {}, title), more ? [h("div", { class: "spacer" }), more] : null),
    bodyClass ? h("div", { class: bodyClass }, body) : body,
  );
}

/** fail focuses a form's field at fault and throws { message }, for
 *  formModal to show. A field in a closed fold cannot take focus, so the fold
 *  opens first. */
export function fail(input, message) {
  const details = input.closest && input.closest("details");
  if (details) details.open = true;
  input.focus();
  throw { message };
}

/** stamp is a relative time, with the date and time on hover. ahead counts
 *  towards it, as for a next run. */
export function stamp(iso, { ahead = false, cls } = {}) {
  return h("span", { class: cls, title: dateTime(iso) }, ahead ? until(iso) : ago(iso));
}

/** loading holds a screen's place while it loads. */
export function loading(minHeight) {
  return h("div", { class: "boot", style: { minHeight } }, h("div", { class: "boot-mark" }));
}

/** pad writes a number with at least two digits, as times and dates want. */
export function pad(n) {
  return String(n).padStart(2, "0");
}

/** until is a relative time either way: "in 5m" ahead, "5m ago" behind. */
export function until(iso, now = Date.now()) {
  if (!iso) return "-";
  const secs = (new Date(iso).getTime() - now) / 1000;
  if (secs < 0) return ago(iso);
  if (secs < 45) return "in a moment";
  if (secs < 3600) return `in ${Math.round(secs / 60)}m`;
  if (secs < 86400) return `in ${Math.round(secs / 3600)}h`;
  return `in ${Math.round(secs / 86400)}d`;
}

/** francs writes micro-CHF for a form field: 12.5, not 12.50, and "" for none. */
export function francs(micros) {
  if (!micros) return "";
  return String(Math.round(micros / 1e4) / 100);
}

/** microsOf reads a CHF amount from a form field. Empty is 0; anything that
 *  is not a non-negative amount is NaN. */
export function microsOf(text) {
  const s = String(text ?? "")
    .trim()
    .replace(/^CHF\s*/i, "")
    .replace(/['’\s]/g, "")
    .replace(",", ".");
  if (!s) return 0;
  if (!/^\d+(\.\d+)?$/.test(s)) return NaN;
  return Math.round(Number(s) * 1e6);
}

/** splitList reads a list typed by hand, one per line or comma separated,
 *  without blanks or repeats. */
export function splitList(text, { lower = false } = {}) {
  const seen = new Set();
  const out = [];
  for (let item of String(text || "").split(/[\n,;]+/)) {
    item = item.trim();
    if (lower) item = item.toLowerCase();
    if (!item || seen.has(item.toLowerCase())) continue;
    seen.add(item.toLowerCase());
    out.push(item);
  }
  return out;
}

/** clock is a time of day, or a date and time when not today. */
export function clock(iso, now = new Date()) {
  if (!iso) return "-";
  const d = new Date(iso);
  if (d.toDateString() === now.toDateString()) {
    return d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit", second: "2-digit" });
  }
  return dateTime(iso);
}

/** tabs is a row of tabs over one panel, drawn as the gateway's segmented
 *  control. pick(value) runs on a change; select(value) changes it from code. */
export function tabs(items, current, pick) {
  const buttons = items.map((t) =>
    h(
      "button",
      {
        type: "button",
        role: "tab",
        "aria-selected": String(t.value === current),
        "aria-pressed": String(t.value === current),
        onClick: () => select(t.value, true),
      },
      t.label,
      t.count ? h("span", { class: "tab-count" }, String(t.count)) : null,
    ),
  );
  const el = h("div", { class: "seg tabs", role: "tablist" }, buttons);
  const select = (value, fromClick) => {
    items.forEach((t, i) => {
      buttons[i].setAttribute("aria-selected", String(t.value === value));
      buttons[i].setAttribute("aria-pressed", String(t.value === value));
    });
    if (fromClick) pick(value);
  };
  // Arrow keys move between tabs, as screen reader users expect of a tablist.
  el.addEventListener("keydown", (e) => {
    if (e.key !== "ArrowRight" && e.key !== "ArrowLeft") return;
    const i = buttons.indexOf(document.activeElement);
    if (i < 0) return;
    const next = buttons[(i + (e.key === "ArrowRight" ? 1 : -1) + buttons.length) % buttons.length];
    next.focus();
    next.click();
  });
  el.select = (value) => select(value, false);
  return el;
}

/** fold is a section of a form that starts closed, such as Advanced. */
export function fold(summary, ...children) {
  return h("details", { class: "fold" }, h("summary", {}, summary), ...children);
}

/** checkRow is a checkbox with its name and a line on what it does. */
export function checkRow(label, hint, checked, props = {}) {
  const input = h("input", { type: "checkbox", checked, ...props });
  const el = h(
    "label",
    { class: "check-row" },
    input,
    h(
      "span",
      { class: "stack" },
      h("span", { class: "check-name" }, label),
      hint ? h("span", { class: "hint" }, hint) : null,
    ),
  );
  return { el, input };
}

/** AGENT_ICONS are what people choose from to tell their agents apart. The
 *  first is the default. The server keeps only the name, so a name dropped
 *  from here shows the default. */
const AGENT_ICONS = [
  { name: "", label: "Robot", path: icons.agents },
  {
    name: "sparkles",
    label: "Sparkles",
    path: "M12 3l1.8 4.7 4.7 1.8-4.7 1.8L12 16l-1.8-4.7-4.7-1.8 4.7-1.8zM19 15l.8 2.2 2.2.8-2.2.8L19 21l-.8-2.2-2.2-.8 2.2-.8z",
  },
  { name: "chat", label: "Chat", path: icons.chat },
  {
    name: "mail",
    label: "Mail",
    path: "M4 4h16a2 2 0 0 1 2 2v12a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V6a2 2 0 0 1 2-2zM22 6l-10 7L2 6",
  },
  {
    name: "megaphone",
    label: "Announcements",
    path: "M3 11v3a1 1 0 0 0 1 1h3l6 4V6l-6 4H4a1 1 0 0 0-1 1zM17 9a4 4 0 0 1 0 6M20 6a8 8 0 0 1 0 12",
  },
  { name: "person", label: "Person", path: icons.person },
  { name: "code", label: "Code", path: icons.code },
  { name: "terminal", label: "Terminal", path: icons.terminal },
  { name: "tool", label: "Wrench", path: icons.tool },
  { name: "shield", label: "Shield", path: "M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z" },
  { name: "chart", label: "Chart", path: "M18 20V10M12 20V4M6 20v-6" },
  { name: "money", label: "Money", path: "M12 1v22M17 5H9.5a3.5 3.5 0 0 0 0 7h5a3.5 3.5 0 0 1 0 7H6" },
  {
    name: "briefcase",
    label: "Briefcase",
    path: "M4 7h16a2 2 0 0 1 2 2v10a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V9a2 2 0 0 1 2-2zM16 21V5a2 2 0 0 0-2-2h-4a2 2 0 0 0-2 2v16",
  },
  {
    name: "cart",
    label: "Cart",
    path: "M9 21a1 1 0 1 0 0-2 1 1 0 0 0 0 2zM20 21a1 1 0 1 0 0-2 1 1 0 0 0 0 2zM1 1h4l2.68 13.39a2 2 0 0 0 2 1.61h9.72a2 2 0 0 0 2-1.61L23 6H6",
  },
  { name: "calendar", label: "Calendar", path: icons.calendar },
  { name: "file", label: "Document", path: icons.file },
  { name: "folder", label: "Folder", path: icons.folder },
  { name: "book", label: "Book", path: icons.docs },
  { name: "search", label: "Magnifier", path: "M11 3a8 8 0 1 1 0 16 8 8 0 0 1 0-16zM21 21l-4.35-4.35" },
  {
    name: "globe",
    label: "Globe",
    path: "M12 2a10 10 0 1 1 0 20 10 10 0 0 1 0-20zM2 12h20M12 2a15 15 0 0 1 4 10 15 15 0 0 1-4 10 15 15 0 0 1-4-10 15 15 0 0 1 4-10z",
  },
  { name: "bolt", label: "Lightning", path: "M13 2L3 14h9l-1 8 10-12h-9z" },
  {
    name: "heart",
    label: "Heart",
    path: "M20.8 4.6a5.5 5.5 0 0 0-7.8 0L12 5.7l-1-1.1a5.5 5.5 0 0 0-7.8 7.8l1 1.1 7.8 7.8 7.8-7.8 1-1.1a5.5 5.5 0 0 0 0-7.8z",
  },
  { name: "leaf", label: "Leaf", path: "M11 20A7 7 0 0 1 4 13c0-6 7-10 16-10 0 9-4 16-9 17zM4 21c4-6 8-9 12-11" },
  { name: "star", label: "Star", path: "M12 2l3.1 6.3 6.9 1-5 4.9 1.2 6.8-6.2-3.2-6.2 3.2L7 14.2 2 9.3l6.9-1z" },
];

/** agentIconPath is the path of an agent's icon, the default when unknown. */
export function agentIconPath(name) {
  return (AGENT_ICONS.find((i) => i.name === (name || "")) || AGENT_ICONS[0]).path;
}

/** avatarOf is a small round mark with someone's initials, or with an
 *  agent's icon. glyph is another icon's path, such as a workspace's. */
export function avatarOf(name, { agent = false, icon: agentIcon = "", glyph = "", large = false } = {}) {
  return h(
    "span",
    {
      class: "avatar" + (large ? "" : " avatar-sm") + (agent ? " avatar-agent" : ""),
      "aria-hidden": "true",
      title: name || null,
    },
    glyph ? icon(glyph) : agent ? icon(agentIconPath(agentIcon)) : initials(name, name),
  );
}

/** agentIconPicker is a button showing an agent's icon that opens a grid to
 *  choose another. value() is the chosen icon's name. */
export function agentIconPicker(chosen = "") {
  let value = AGENT_ICONS.some((i) => i.name === chosen) ? chosen : "";
  const label = () => (AGENT_ICONS.find((i) => i.name === value) || AGENT_ICONS[0]).label;
  const button = h("button", {
    type: "button",
    class: "btn icon-pick",
    "aria-expanded": "false",
    "aria-haspopup": "true",
    onClick: () => toggle(grid.hidden),
  });
  const choices = AGENT_ICONS.map((i) =>
    h(
      "button",
      {
        type: "button",
        title: i.label,
        "aria-label": i.label,
        "aria-pressed": String(i.name === value),
        onClick: () => {
          value = i.name;
          choices.forEach((c, n) => c.setAttribute("aria-pressed", String(AGENT_ICONS[n].name === value)));
          draw();
          toggle(false);
          button.focus();
        },
      },
      icon(i.path),
    ),
  );
  const grid = h("div", { class: "icon-grid", role: "group", "aria-label": "Icons", hidden: true }, choices);
  grid.addEventListener("keydown", (e) => {
    if (e.key === "Escape") {
      toggle(false);
      button.focus();
    }
  });
  function toggle(open) {
    grid.hidden = !open;
    button.setAttribute("aria-expanded", String(open));
    if (open) (choices.find((c) => c.getAttribute("aria-pressed") === "true") || choices[0]).focus();
  }
  function draw() {
    replace(button, avatarOf("", { agent: true, icon: value, large: true }));
    button.title = `Icon: ${label()}. Choose another`;
    button.setAttribute("aria-label", `Icon: ${label()}. Choose another`);
  }
  draw();
  return { button, grid, value: () => value };
}

/** every runs fn each ms while the screen is shown, skipping ticks while the
 *  tab is hidden or the last run has not finished. It stops when the screen
 *  is torn down, or when it calls the stop function it returns. */
export function every(ctx, ms, fn) {
  let busy = false;
  const timer = setInterval(async () => {
    if (busy || document.hidden) return;
    busy = true;
    try {
      await fn();
    } catch {
      // The next tick tries again.
    } finally {
      busy = false;
    }
  }, ms);
  const stop = () => clearInterval(timer);
  ctx.onTeardown(stop);
  return stop;
}

/** debounce runs fn once calls have stopped for ms. */
export function debounce(fn, ms) {
  let timer = 0;
  const run = (...args) => {
    clearTimeout(timer);
    timer = setTimeout(() => fn(...args), ms);
  };
  run.cancel = () => clearTimeout(timer);
  return run;
}
