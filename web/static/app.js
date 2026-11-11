// PulseGo read-only dashboard.
//
// Everything shown here comes from the existing /v1 API, called from the
// browser with the API key the user enters. The key lives in sessionStorage
// only, travels only in the Authorization header, and is never logged or put
// in a URL. Strings from the API are inserted with textContent, never as HTML.
"use strict";

(function () {
  const REFRESH_MS = 10000;
  const KEY_STORAGE = "pulsego.apiKey";
  const PAGE_LIMIT = 100;
  const MAX_MONITOR_PAGES = 10;
  const INCIDENTS_PER_MONITOR = 10;
  const MAX_INCIDENT_ROWS = 10;
  const CONCURRENCY = 6;
  const STATES = ["up", "degraded", "down", "unknown"];

  const $ = (id) => document.getElementById(id);

  // ---- API key storage --------------------------------------------------

  let memoryKey = ""; // fallback when sessionStorage is unavailable

  function getKey() {
    try {
      return sessionStorage.getItem(KEY_STORAGE) || "";
    } catch (_) {
      return memoryKey;
    }
  }

  function setKey(key) {
    memoryKey = key;
    try {
      sessionStorage.setItem(KEY_STORAGE, key);
    } catch (_) {
      // Storage disabled: the key stays in memory for this page only.
    }
  }

  function clearKey() {
    memoryKey = "";
    try {
      sessionStorage.removeItem(KEY_STORAGE);
    } catch (_) {
      // Nothing stored.
    }
  }

  // ---- HTTP ---------------------------------------------------------------

  class AuthError extends Error {}

  class NotFoundError extends Error {}

  async function apiGet(path) {
    const res = await fetch(path, {
      method: "GET",
      headers: { Authorization: "Bearer " + getKey(), Accept: "application/json" },
      cache: "no-store",
      credentials: "omit",
      redirect: "error",
    });
    if (res.status === 401) {
      throw new AuthError("unauthorized");
    }
    if (res.status === 404) {
      throw new NotFoundError("not found");
    }
    if (!res.ok) {
      let title = res.statusText || "Request failed";
      try {
        const problem = await res.json();
        if (problem && typeof problem.title === "string") {
          title = problem.title;
        }
      } catch (_) {
        // Not a problem document; keep the status text.
      }
      throw new Error("The API answered " + res.status + " (" + title + ").");
    }
    return res.json();
  }

  async function listMonitors() {
    const monitors = [];
    let cursor = "";
    for (let page = 0; page < MAX_MONITOR_PAGES; page++) {
      let path = "/v1/monitors?limit=" + PAGE_LIMIT;
      if (cursor) {
        path += "&cursor=" + encodeURIComponent(cursor);
      }
      const body = await apiGet(path);
      monitors.push(...(body.data || []));
      if (!body.next_cursor) {
        break;
      }
      cursor = body.next_cursor;
    }
    return monitors;
  }

  // A monitor deleted between the list and this call is a 404: show it
  // without the extra data rather than failing the whole refresh.
  async function optional(promise) {
    try {
      return await promise;
    } catch (err) {
      if (err instanceof NotFoundError) {
        return null;
      }
      throw err;
    }
  }

  async function loadDetails(monitor) {
    const base = "/v1/monitors/" + encodeURIComponent(monitor.id);
    const [stats, incidents] = await Promise.all([
      optional(apiGet(base + "/stats?window=24h")),
      optional(apiGet(base + "/incidents?limit=" + INCIDENTS_PER_MONITOR)),
    ]);
    return { monitor, stats, incidents: incidents ? incidents.data || [] : [] };
  }

  // Runs fn over items with at most `limit` requests in flight.
  async function mapLimited(items, limit, fn) {
    const results = new Array(items.length);
    let next = 0;
    async function worker() {
      while (next < items.length) {
        const i = next++;
        results[i] = await fn(items[i]);
      }
    }
    const workers = [];
    for (let i = 0; i < Math.min(limit, items.length); i++) {
      workers.push(worker());
    }
    await Promise.all(workers);
    return results;
  }

  // ---- Formatting -------------------------------------------------------

  const timeFmt = new Intl.DateTimeFormat(undefined, {
    hour: "2-digit", minute: "2-digit", second: "2-digit",
  });
  const dateTimeFmt = new Intl.DateTimeFormat(undefined, {
    month: "short", day: "numeric", hour: "2-digit", minute: "2-digit",
  });
  const fullFmt = new Intl.DateTimeFormat(undefined, {
    dateStyle: "medium", timeStyle: "medium",
  });

  function parseDate(value) {
    if (typeof value !== "string") {
      return null;
    }
    const d = new Date(value);
    return Number.isNaN(d.getTime()) ? null : d;
  }

  // Only the host (and port) of a target: never userinfo, path or query.
  function targetHost(raw) {
    try {
      return new URL(raw).host || "—";
    } catch (_) {
      return "invalid URL";
    }
  }

  function formatDuration(ms) {
    const s = Math.max(0, Math.round(ms / 1000));
    if (s < 60) return s + " s";
    const m = Math.floor(s / 60);
    if (m < 60) return m + " min " + (s % 60) + " s";
    const h = Math.floor(m / 60);
    if (h < 24) return h + " h " + (m % 60) + " min";
    const d = Math.floor(h / 24);
    return d + " d " + (h % 24) + " h";
  }

  function formatAgo(date, now) {
    const s = Math.round((now - date) / 1000);
    if (s < 5) return "just now";
    return formatDuration(s * 1000) + " ago";
  }

  // ---- DOM helpers --------------------------------------------------------

  function el(tag, className, text) {
    const node = document.createElement(tag);
    if (className) node.className = className;
    if (text !== undefined && text !== null) node.textContent = String(text);
    return node;
  }

  function cell(text, className) {
    return el("td", className, text);
  }

  function naCell(className, title) {
    const td = el("td", ((className || "") + " na").trim(), "—");
    if (title) td.title = title;
    return td;
  }

  function stateBadge(state) {
    const known = STATES.includes(state) ? state : "unknown";
    return el("span", "badge badge-" + known, known);
  }

  function show(id, visible) {
    $(id).hidden = !visible;
  }

  // ---- Rendering ----------------------------------------------------------

  function renderSummary(monitors) {
    let up = 0;
    let down = 0;
    for (const m of monitors) {
      if (m.state === "up") up++;
      else if (m.state === "down") down++;
    }
    $("count-up").textContent = String(up);
    $("count-down").textContent = String(down);
    $("count-other").textContent = String(monitors.length - up - down);
    $("count-total").textContent = String(monitors.length);
  }

  function uptimeClass(pct) {
    if (pct >= 99) return "uptime-good";
    if (pct >= 95) return "uptime-warn";
    return "uptime-bad";
  }

  function monitorRow(entry, now) {
    const m = entry.monitor;
    const s = entry.stats;
    const tr = document.createElement("tr");

    const name = el("td");
    name.append(el("div", "monitor-name", m.name), el("div", "monitor-host", targetHost(m.url)));
    tr.append(name);

    const state = el("td");
    state.append(stateBadge(m.state));
    if (m.enabled === false) {
      state.append(el("span", "badge badge-paused", "paused"));
    }
    tr.append(state);

    if (s && typeof s.uptime_percent === "number") {
      tr.append(cell(s.uptime_percent.toFixed(2) + " %", "num " + uptimeClass(s.uptime_percent)));
    } else {
      tr.append(naCell("num", "No checks in the last 24 h"));
    }

    if (s && typeof s.p95_latency_ms === "number") {
      tr.append(cell(s.p95_latency_ms + " ms", "num"));
    } else {
      tr.append(naCell("num", "No successful checks in the last 24 h"));
    }

    const checked = parseDate(m.last_checked_at);
    if (checked) {
      const td = cell(formatAgo(checked, now));
      td.title = fullFmt.format(checked);
      tr.append(td);
    } else {
      tr.append(naCell("", "Not checked yet"));
    }

    tr.append(typeof m.last_status_code === "number"
      ? cell(m.last_status_code, "num")
      : naCell("num", checked ? "No response received" : ""));

    tr.append(typeof m.last_latency_ms === "number"
      ? cell(m.last_latency_ms + " ms", "num")
      : naCell("num"));

    return tr;
  }

  function renderMonitors(entries, now) {
    const body = $("monitors-body");
    body.replaceChildren(...entries.map((e) => monitorRow(e, now)));
    show("monitors-loading", false);
    show("monitors-empty", entries.length === 0);
    show("monitors-table", entries.length > 0);
  }

  function collectIncidents(entries) {
    const open = [];
    const resolved = [];
    for (const e of entries) {
      for (const inc of e.incidents) {
        const started = parseDate(inc.started_at);
        if (!started) continue;
        const item = { name: e.monitor.name, started, resolved: parseDate(inc.resolved_at), cause: inc.cause };
        (item.resolved ? resolved : open).push(item);
      }
    }
    open.sort((a, b) => b.started - a.started);
    resolved.sort((a, b) => b.resolved - a.resolved);
    // Every open incident is shown; resolved ones fill the remaining rows.
    return open.concat(resolved.slice(0, Math.max(0, MAX_INCIDENT_ROWS - open.length)));
  }

  function incidentRow(inc, now) {
    const tr = document.createElement("tr");
    tr.append(cell(inc.name, "monitor-name"));

    const status = el("td");
    status.append(inc.resolved ? el("span", "badge badge-up", "resolved") : el("span", "badge badge-down", "open"));
    tr.append(status);

    const started = cell(dateTimeFmt.format(inc.started));
    started.title = fullFmt.format(inc.started);
    tr.append(started);

    if (inc.resolved) {
      const resolved = cell(dateTimeFmt.format(inc.resolved));
      resolved.title = fullFmt.format(inc.resolved);
      tr.append(resolved, cell(formatDuration(inc.resolved - inc.started), "num"));
    } else {
      tr.append(cell("ongoing", "na"), cell(formatDuration(now - inc.started), "num"));
    }

    tr.append(inc.cause ? cell(inc.cause, "cause") : naCell("cause"));
    return tr;
  }

  function renderIncidents(entries, now) {
    const rows = collectIncidents(entries);
    $("incidents-body").replaceChildren(...rows.map((i) => incidentRow(i, now)));
    show("incidents-loading", false);
    show("incidents-empty", rows.length === 0);
    show("incidents-table", rows.length > 0);
  }

  function setStatus(kind, text) {
    const dot = $("refresh-dot");
    dot.classList.toggle("is-loading", kind === "loading");
    dot.classList.toggle("is-stale", kind === "error");
    if (text) $("updated-at").textContent = text;
  }

  // ---- Refresh loop -------------------------------------------------------

  let timer = 0;
  let generation = 0; // bumps on sign-out so a late response is discarded
  let lastUpdated = null;

  function schedule() {
    clearTimeout(timer);
    timer = setTimeout(refresh, REFRESH_MS);
  }

  async function refresh() {
    clearTimeout(timer);
    const gen = generation;
    setStatus("loading");
    try {
      const monitors = await listMonitors();
      const entries = await mapLimited(monitors, CONCURRENCY, loadDetails);
      if (gen !== generation) return;
      const now = new Date();
      renderSummary(monitors);
      renderMonitors(entries, now);
      renderIncidents(entries, now);
      lastUpdated = now;
      show("error-banner", false);
      setStatus("ok", "Updated " + timeFmt.format(now));
      schedule();
    } catch (err) {
      if (gen !== generation) return;
      if (err instanceof AuthError) {
        promptForKey("The API key was rejected. Enter it again.");
        return;
      }
      const banner = $("error-banner");
      banner.textContent = "Could not refresh the dashboard: " +
        (err && err.message ? err.message : "network error") + " Retrying in 10 s.";
      show("error-banner", true);
      if (!lastUpdated) {
        $("monitors-loading").textContent = "Monitors could not be loaded.";
        $("incidents-loading").textContent = "Incidents could not be loaded.";
      }
      setStatus("error", lastUpdated
        ? "Last updated " + timeFmt.format(lastUpdated) + " · refresh failed"
        : "Not loaded yet");
      schedule();
    }
  }

  function resetDashboard() {
    lastUpdated = null;
    $("monitors-body").replaceChildren();
    $("incidents-body").replaceChildren();
    $("monitors-loading").textContent = "Loading monitors…";
    $("incidents-loading").textContent = "Loading incidents…";
    for (const id of ["monitors-loading", "incidents-loading"]) show(id, true);
    for (const id of ["monitors-empty", "monitors-table", "incidents-empty", "incidents-table", "error-banner"]) show(id, false);
    for (const id of ["count-up", "count-down", "count-other", "count-total"]) $(id).textContent = "–";
    $("updated-at").textContent = "Loading…";
  }

  function promptForKey(message) {
    generation++;
    clearTimeout(timer);
    clearKey();
    resetDashboard();
    show("dashboard", false);
    show("meta", false);
    show("key-panel", true);
    const error = $("key-error");
    error.textContent = message || "";
    error.hidden = !message;
    $("key-input").focus();
  }

  function openDashboard() {
    show("key-panel", false);
    show("dashboard", true);
    show("meta", true);
    refresh();
  }

  document.addEventListener("DOMContentLoaded", () => {
    $("key-form").addEventListener("submit", (event) => {
      event.preventDefault();
      const input = $("key-input");
      const key = input.value.trim();
      input.value = "";
      if (!key) {
        promptForKey("Enter the API key.");
        return;
      }
      setKey(key);
      openDashboard();
    });

    $("sign-out").addEventListener("click", () => promptForKey(""));

    if (getKey()) {
      openDashboard();
    } else {
      promptForKey("");
    }
  });
})();
