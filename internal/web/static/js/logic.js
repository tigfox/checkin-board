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

// A checkpoint's state is its own (4.7): it opens and closes while the
// race goes on elsewhere, so its pages say so.
const CHECKPOINT_STATE_LABELS = {
  setup: "Checkpoint not open",
  active: "Checkpoint open",
  complete: "Checkpoint closed",
};

export function stateLabel(state, role) {
  if (role === "checkpoint" && CHECKPOINT_STATE_LABELS[state]) return CHECKPOINT_STATE_LABELS[state];
  return STATE_LABELS[state] || state || "";
}

const ACTION_LABELS = {
  start: ["Start race", "Open the keypad and start taking entries?"],
  complete: ["Complete race", "Close the keypad? Unsent entries keep going out."],
  secure: ["Secure for travel", "Pack up: nothing is transmitted until the final check-in at HQ. Continue?"],
  "check-in": ["Final check-in", "Send everything HQ hasn't confirmed, now?"],
  "cleanup-graywolf": ["Clean up graywolf messages", "Delete this race's messages from graywolf? Messages still needed are kept."],
};

const CHECKPOINT_ACTION_LABELS = {
  start: ["Open checkpoint", "Open this checkpoint's keypad and start taking entries?"],
  complete: ["Close checkpoint", "Close this checkpoint? The keypad stops taking entries (the race goes on elsewhere). Unsent entries keep going out, and HQ sees it as closed."],
};

// actionLabel is the button label and confirm question for a lifecycle
// action in this role.
export function actionLabel(action, role) {
  return (role === "checkpoint" && CHECKPOINT_ACTION_LABELS[action]) || ACTION_LABELS[action] || [action, `${action}?`];
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
  // When HQ heard the close, which may be later than the close itself.
  if (h.ClosedAt) flags.push(`closed (heard ${formatTime(h.ClosedAt).slice(0, 5)})`);
  if (!h.LastHeardAt) flags.push("never heard");
  else if (!h.ClosedAt && nowMs - new Date(h.LastHeardAt).getTime() > 15 * 60 * 1000) flags.push("quiet for 15+ min");
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

// levelNote flags a receive level outside the useful range (spec 4.8.4).
export function levelNote(dbfs) {
  if (typeof dbfs !== "number") return "";
  if (dbfs > -6) return " (too hot)";
  if (dbfs < -40) return " (very low)";
  return "";
}

// linkSummary is a checkpoint's latest link check for the health panel.
export function linkSummary(s, nowMs) {
  if (!s) return "never checked";
  const parts = [`${s.verdict} ${s.heard}/${s.total}`];
  if (typeof s.level === "number") parts.push(`${s.level} dBFS${levelNote(s.level)}`);
  parts.push(formatAgo(nowMs - new Date(s.at).getTime()));
  return parts.join(", ");
}

// linkProgress describes a link check still in progress ("" once done).
export function linkProgress(c) {
  if (c.state === "requested") return "Starting…";
  if (c.state !== "running") return "";
  if (c.probes_sent < c.count) return `Probe ${c.probes_sent} of ${c.count} sent…`;
  return c.reply_received ? "Reply received; collecting ACKs…" : "All probes sent; waiting for the reply…";
}

// sameStation mirrors graywolf's own-call rule (store.SameStation):
// case-insensitive, SSID 0 the same as no SSID, empty never matches.
export function sameStation(a, b) {
  const norm = (s) => String(s ?? "").trim().toUpperCase().replace(/-0$/, "");
  return norm(a) !== "" && norm(a) === norm(b);
}

// linkTargets turns HQ's checkpoint health (/api/admin/status) into the
// link check's "Checkpoint to probe" options, in course order: listed
// checkpoints by their expected callsign (greyed out without one), then
// codes HQ heard that aren't on its list, by the call they came from.
export function linkTargets(health, nowMs) {
  const heard = (c) => {
    const ago = c.LastHeardAt ? formatAgo(nowMs - Date.parse(c.LastHeardAt)) : "";
    return ago ? ` · heard ${ago}` : "";
  };
  const out = [];
  for (const c of health || []) {
    const name = [c.CPCode, c.Name].filter(Boolean).join(" ");
    if (c.Defined && c.ExpectedCall) {
      const from = c.SenderMismatch && c.LastSourceCall ? ` · last heard from ${c.LastSourceCall}` : heard(c);
      out.push({ value: c.ExpectedCall, label: `${name} (${c.ExpectedCall})${from}`, disabled: false });
    } else if (c.Defined) {
      out.push({ value: "", label: `${name}: no callsign (add one on the HQ tab)`, disabled: true });
    } else if (c.LastSourceCall) {
      out.push({ value: c.LastSourceCall, label: `${c.CPCode} (heard from ${c.LastSourceCall}, not on the list)${heard(c)}`, disabled: false });
    }
  }
  return out;
}

// radioIcon marks a radio check item's status.
export function radioIcon(status) {
  return { ok: "✓", warn: "⚠", fail: "✗" }[status] || "?";
}

// radioSummary is the radio check (/api/admin/radio report) in one line
// for the Race page: {kind: banner kind, text}, or null with no report.
export function radioSummary(report) {
  if (!report || !Array.isArray(report.items)) return null;
  const labels = (st) => report.items.filter((i) => i.status === st).map((i) => i.label).join(", ");
  const failed = labels("fail");
  const warned = labels("warn");
  const unchecked = labels("unknown");
  if (!failed && !warned) {
    return { kind: "info", text: unchecked ? `Radio: no problems found; ${unchecked} not checked yet.` : "Radio: all checks pass." };
  }
  const parts = [];
  if (failed) parts.push(`${failed} failed`);
  if (warned) parts.push(`${warned} needs a look`);
  return { kind: failed ? "bad" : "warn", text: `Radio: ${parts.join("; ")}. See Station → Radio.` };
}

// markdownBlocks parses the event page's Markdown subset (phase 12a):
// "#"-"###" headings, paragraphs, "-"/"*" lists, "|" tables (first row is
// the header; "|---|" rows are skipped) and **bold**. Everything else is
// plain text; the page is built from text nodes, so HTML stays text.
export function markdownBlocks(text) {
  const spans = (s) => {
    const out = [];
    let rest = s;
    for (;;) {
      const open = rest.indexOf("**");
      const close = open < 0 ? -1 : rest.indexOf("**", open + 2);
      if (open < 0 || close < 0) break;
      if (open > 0) out.push({ text: rest.slice(0, open), bold: false });
      out.push({ text: rest.slice(open + 2, close), bold: true });
      rest = rest.slice(close + 2);
    }
    if (rest) out.push({ text: rest, bold: false });
    return out;
  };
  const cells = (line) => line.trim().replace(/^\|/, "").replace(/\|$/, "").split("|").map((c) => spans(c.trim()));
  const blocks = [];
  let para = null;
  const flush = () => {
    if (para) blocks.push({ type: "para", spans: spans(para.join(" ")) });
    para = null;
  };
  for (const raw of String(text ?? "").split(/\r?\n/)) {
    const line = raw.trim();
    const last = blocks[blocks.length - 1];
    let m;
    if (!line) {
      flush();
    } else if ((m = /^(#{1,3})\s+(.*)$/.exec(line))) {
      flush();
      blocks.push({ type: "heading", level: m[1].length, spans: spans(m[2]) });
    } else if ((m = /^[-*]\s+(.*)$/.exec(line))) {
      flush();
      if (last?.type === "list") last.items.push(spans(m[1]));
      else blocks.push({ type: "list", items: [spans(m[1])] });
    } else if (line.startsWith("|")) {
      flush();
      if (/^\|[\s:|-]*$/.test(line)) continue; // header separator
      if (last?.type === "table") last.rows.push(cells(line));
      else blocks.push({ type: "table", head: cells(line), rows: [] });
    } else {
      (para ||= []).push(line);
    }
  }
  flush();
  return blocks;
}
