// Pure UI logic, unit-tested with `node --test` (no DOM, no network).

export const MAX_BIB_DIGITS = 4;
export const DRIFT_WARN_SEC = 5;

// keypad applies one key to the typed digits: "0"-"9" append (up to 4
// digits), "back" deletes one, "clear" empties.
export function keypad(digits, key) {
  if (key === "clear") return "";
  if (key === "back") return digits.slice(0, -1);
  if (/^[0-9]$/.test(key) && digits.length < MAX_BIB_DIGITS) return digits + key;
  return digits;
}

// bibValue is the bib number typed, or 0 if it isn't a valid bib (1-9999).
export function bibValue(digits) {
  if (!/^[0-9]{1,4}$/.test(digits)) return 0;
  const n = Number(digits);
  return n >= 1 && n <= 9999 ? n : 0;
}

// formatAgo describes an age in milliseconds for status strips.
export function formatAgo(ms) {
  if (!Number.isFinite(ms) || ms < 0) return "";
  const s = Math.floor(ms / 1000);
  if (s < 10) return "just now";
  if (s < 60) return `${s} s ago`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m} min ago`;
  const h = Math.floor(m / 60);
  return `${h} h ${m % 60} min ago`;
}

// formatTime shows a time of day (local, 24 h, seconds).
export function formatTime(iso, timeZone) {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  return d.toLocaleTimeString("en-GB", { hour12: false, timeZone });
}

// clockState summarizes the race clock for the banner: whether it's
// set, and how far this device's clock is from it (seconds; positive =
// device ahead).
export function clockState(view, deviceNowMs) {
  const raceMs = new Date(view.now).getTime();
  const drift = Math.round((deviceNowMs - raceMs) / 1000);
  return {
    unset: view.source === "unsynced",
    drift,
    warn: view.source !== "unsynced" && Math.abs(drift) > DRIFT_WARN_SEC,
  };
}

const STATE_LABELS = {
  setup: "Race not started",
  active: "Race active",
  complete: "Race complete",
  secured: "Secured for travel",
  checking_in: "Final check-in",
  checked_in: "Checked in",
};

export function stateLabel(state) {
  return STATE_LABELS[state] || state || "";
}

// keypadOpen reports whether the keypad takes entries.
export function keypadOpen(state) {
  return state === "active";
}

// actionsFor lists the lifecycle actions an admin can take now (spec 4.7).
export function actionsFor(role, state) {
  const a = [];
  if (!role) return a;
  if (state === "setup") a.push("start");
  if (state === "active") a.push("complete");
  if (role === "checkpoint") {
    if (["active", "complete", "checking_in"].includes(state)) a.push("secure");
    if (["secured", "complete"].includes(state)) a.push("check-in");
    if (["complete", "secured", "checked_in"].includes(state)) a.push("cleanup-graywolf");
  } else if (role === "hq" && state === "complete") {
    a.push("cleanup-graywolf");
  }
  a.push("reset");
  return a;
}

// entryBadge is the delivery badge for a keypad entry.
export function entryBadge(e) {
  if (e.Voided) return { label: e.VoidState === "confirmed" || !e.VoidState ? "voided" : "voiding", cls: "void" };
  switch (e.State) {
    case "queued": return { label: "queued", cls: "queued" };
    case "sent": return { label: "sent", cls: "sent" };
    case "confirmed": return { label: "confirmed", cls: "ok" };
    case "recorded": return { label: "recorded", cls: "ok" };
    default: return { label: e.State || "", cls: "" };
  }
}

// deliverySummary is the keypad's "N unconfirmed, last HQ contact" strip.
export function deliverySummary(station, nowMs) {
  if (station.role !== "checkpoint") return "";
  const parts = [station.unconfirmed === 0 ? "All entries confirmed by HQ" : `${station.unconfirmed} unconfirmed`];
  if (station.last_hq_contact) {
    parts.push(`last HQ contact ${formatAgo(nowMs - new Date(station.last_hq_contact).getTime())}`);
  } else {
    parts.push("no HQ contact yet");
  }
  return parts.join(", ");
}

// cellText renders a board cell's passage times.
export function cellText(cell, timeZone) {
  if (!cell || !cell.Times || cell.Times.length === 0) return "";
  return cell.Times.map((t) => formatTime(t, timeZone).slice(0, 5)).join(" / ");
}

// healthFlags lists a checkpoint's problems for the HQ health panel.
export function healthFlags(h, nowMs) {
  const flags = [];
  if (!h.LastHeardAt) flags.push("never heard");
  else if (nowMs - new Date(h.LastHeardAt).getTime() > 15 * 60 * 1000) flags.push("quiet for 15+ min");
  if (h.Missing && h.Missing.length) flags.push(`${h.Missing.length} batch(es) missing`);
  if (h.GivenUp && h.GivenUp.length) flags.push(`${h.GivenUp.length} given up`);
  if (h.SenderMismatch) flags.push(`heard from ${h.LastSourceCall}, expected ${h.ExpectedCall}`);
  if (h.ClockSkewSec !== null && h.ClockSkewSec !== undefined && Math.abs(h.ClockSkewSec) > 30) {
    flags.push(`clock off by ${h.ClockSkewSec} s`);
  }
  if (h.BadReports) flags.push(`${h.BadReports} unreadable report(s)`);
  if (h.SeqReuseCount) flags.push("checkpoint restarted its numbering");
  return flags;
}

// requestId makes an idempotency key so a retried keypad entry can't
// be logged twice.
export function requestId() {
  if (globalThis.crypto && globalThis.crypto.randomUUID) return globalThis.crypto.randomUUID();
  return `${Date.now()}-${Math.random().toString(36).slice(2)}`;
}

// keepPending reports whether a failed save might still have been
// stored, so LOG again must reuse the same request id (the server then
// answers the retry instead of logging the bib twice). Only a definite
// 4xx refusal starts afresh.
export function keepPending(err) {
  if (!err || typeof err.status !== "number" || err.network) return true;
  if (err.status >= 500 || err.status === 408 || err.status === 429) return true;
  return err.status === 409 && err.code === "in_progress";
}
