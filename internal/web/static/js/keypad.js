// Volunteer keypad (spec 8.2).
import { del, get, post } from "./api.js";
import { $, banner, clear, h, poll, show } from "./dom.js";
import * as L from "./logic.js";

let digits = "";
let station = null;
let limit = 20;
let lastRTT = 0;
let pending = null; // a bib whose save hasn't been confirmed: {bib, cp, id}

const display = $("#display");
const result = $("#log-result");

function render() {
  display.textContent = digits || " ";
  const open = station && L.keypadOpen(station.race_state);
  for (const b of document.querySelectorAll("#keys button")) b.disabled = !open;
  $("#keys .log").disabled = !open || saving || L.bibValue(digits) === 0;
}

function buildKeys() {
  const keys = $("#keys");
  for (const k of ["1", "2", "3", "4", "5", "6", "7", "8", "9", "back", "0", "log"]) {
    const label = k === "back" ? "⌫" : k === "log" ? "LOG" : k;
    keys.append(h("button", {
      type: "button", class: k === "log" ? "log" : "", "aria-label": k === "back" ? "delete digit" : label,
      onclick: () => (k === "log" ? logBib() : press(k)),
    }, label));
  }
}

function press(k) {
  digits = L.keypad(digits, k);
  render();
}

// Only digits, Backspace and Enter on the page body drive the keypad:
// Enter on any other control (a select, a button) never logs.
document.addEventListener("keydown", (ev) => {
  if (ev.target !== document.body) return;
  if (/^[0-9]$/.test(ev.key)) press(ev.key);
  else if (ev.key === "Backspace") press("back");
  else if (ev.key === "Escape") press("clear");
  else if (ev.key === "Enter") logBib();
  else return;
  ev.preventDefault();
});

function currentCP() {
  const sel = $("#cp-select");
  return station && station.local_codes && station.local_codes.length > 1 ? sel.value : "";
}

let saving = false;
let entriesGen = 0;

async function logBib() {
  const bib = L.bibValue(digits);
  if (saving || !bib || !station || !L.keypadOpen(station.race_state)) return;
  // A retry of an unconfirmed save reuses its id, so it can't double up.
  let note = "";
  if (pending && pending.bib !== bib) note = ` (earlier ${pending.bib} may not have saved: check the list)`;
  if (!pending || pending.bib !== bib) pending = { bib, cp: currentCP(), id: L.requestId() };
  saving = true;
  render();
  result.className = "log-result";
  result.textContent = `Saving ${bib}…`;
  try {
    const r = await post("/api/entries", { bib: String(bib), cp: pending.cp, request_id: pending.id });
    pending = null;
    digits = "";
    if (r.journal_only) {
      result.className = "log-result error";
      result.textContent = `${bib}: ${r.warning}`;
    } else {
      result.className = "log-result ok";
      result.textContent = `${bib} logged ${L.formatTime(r.time_in)}` + (r.warning ? ` (${r.warning})` : "") + note;
    }
  } catch (e) {
    // Keep the digits: LOG again retries with the same id when the save
    // might have happened, so nothing is lost or doubled.
    result.className = "log-result error";
    if (L.keepPending(e)) {
      result.textContent = `${bib} NOT confirmed yet: ${e.message}. Tap LOG to retry.`;
    } else {
      result.textContent = `${bib}: ${e.message}`;
      pending = null;
    }
  } finally {
    saving = false;
    render();
  }
  await refreshEntries().catch(() => {});
}

async function voidEntry(e) {
  if (!confirm(`Void bib ${e.Bib} logged at ${L.formatTime(e.TimeIn)}?`)) return;
  try {
    const r = await del(`/api/entries/${e.ID}`);
    if (r && r.warning) alert(r.warning);
  } catch (err) {
    alert(err.message);
  }
  await refreshEntries().catch(() => {});
}

// refreshEntries re-renders the list; a slower, older response never
// overwrites a newer one.
async function refreshEntries() {
  const gen = ++entriesGen;
  const { entries } = await get(`/api/entries?limit=${limit}`);
  if (gen !== entriesGen) return;
  const ul = clear($("#entries"));
  for (const e of entries || []) {
    const badge = L.entryBadge(e);
    ul.append(h("li", {},
      h("span", { class: "bib mono" }, e.Bib),
      h("span", { class: "time mono" }, L.formatTime(e.TimeIn)),
      e.ClockSynced ? null : h("span", { class: "badge sent", title: "logged before the clock was set" }, "clock?"),
      h("span", { class: `badge ${badge.cls}` }, badge.label),
      h("span", { class: "grow" }),
      e.Voided ? null : h("button", { type: "button", class: "danger", onclick: () => voidEntry(e) }, "Void"),
    ));
  }
  show($("#more"), (entries || []).length >= limit);
}

$("#more").addEventListener("click", async () => {
  limit = Math.min(limit + 40, 500);
  await refreshEntries();
});

async function syncClock() {
  const t0 = Date.now();
  const view = await post("/api/clock/sync", { client_unix_ms: Date.now(), rtt_ms: lastRTT });
  lastRTT = Date.now() - t0;
  return view;
}

let clockMsg = null;

async function refreshClock() {
  const t0 = Date.now();
  const view = await get("/api/clock");
  lastRTT = Date.now() - t0;
  const cs = L.clockState(view, Date.now());
  $("#race-clock").textContent = L.formatTime(view.now);
  const msg = cs.unset ? "Clock not set: times are stored but flagged."
    : cs.warn ? `Race clock differs from this device by ${cs.drift} s.` : "";
  // Rebuild only when the message changes, so the button isn't replaced
  // under a finger.
  if (msg === clockMsg) return;
  clockMsg = msg;
  const fix = h("button", { type: "button", onclick: async () => {
    try {
      await syncClock();
      clockMsg = null;
      await refreshClock();
    } catch (e) {
      banner($("#clock-banner"), `Couldn't set the time: ${e.message}`, "bad");
      clockMsg = null;
    }
  } }, "Set time from this device");
  banner($("#clock-banner"), msg, cs.unset ? "bad" : "warn", fix);
}

async function refreshStation() {
  station = await get("/api/station");
  const name = [station.race_name, station.station_tactical, (station.local_codes || []).join("/")].filter(Boolean).join(" · ");
  $("#station-title").textContent = name || "checkin-board";
  $("#race-state").textContent = L.stateLabel(station.race_state, station.role);
  $("#delivery").textContent = L.deliverySummary(station, Date.now());
  const gw = station.graywolf;
  banner($("#gw-banner"), gw.problem ? `${gw.problem}: entries are kept here and sent when it's back.` : "", "warn");
  banner($("#state-banner"), L.keypadOpen(station.race_state) ? "" : `${L.stateLabel(station.race_state, station.role)}: the keypad is closed.`, "info");
  const codes = station.local_codes || [];
  const sel = $("#cp-select");
  if (codes.length > 1 && sel.options.length !== codes.length) {
    clear(sel).append(...codes.map((c) => h("option", { value: c }, c)));
  }
  show($("#cp-select-wrap"), codes.length > 1);
  render();
}

$("#logout").addEventListener("click", async () => {
  await post("/api/logout");
  location.href = "/login.html";
});

buildKeys();
render();
get("/api/session")
  .then((session) => show($("#admin-link"), session.role === "admin"))
  .catch(() => {}); // the pollers below report a down node
poll(async () => {
  try {
    await Promise.all([refreshStation(), refreshClock()]);
  } catch (e) {
    banner($("#gw-banner"), e.message, "bad");
  }
}, 5000);
poll(() => refreshEntries().catch(() => {}), 5000);
