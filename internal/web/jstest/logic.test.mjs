// Run with: node --test internal/web/jstest/
import { test } from "node:test";
import assert from "node:assert/strict";
import * as L from "../static/js/logic.js";

test("keypad caps at four digits and edits", () => {
  let d = "";
  for (const k of ["1", "2", "3", "4", "5"]) d = L.keypad(d, k);
  assert.equal(d, "1234");
  assert.equal(L.keypad(d, "back"), "123");
  assert.equal(L.keypad(d, "clear"), "");
  assert.equal(L.keypad("12", "x"), "12");
  assert.equal(L.keypad("12", "12"), "12");
});

test("bibValue accepts 1-9999 with leading zeros", () => {
  assert.equal(L.bibValue("0042"), 42);
  assert.equal(L.bibValue("9999"), 9999);
  assert.equal(L.bibValue("0"), 0);
  assert.equal(L.bibValue("0000"), 0);
  assert.equal(L.bibValue(""), 0);
  assert.equal(L.bibValue("12a"), 0);
});

test("formatAgo", () => {
  assert.equal(L.formatAgo(3000), "just now");
  assert.equal(L.formatAgo(45000), "45 s ago");
  assert.equal(L.formatAgo(5 * 60000), "5 min ago");
  assert.equal(L.formatAgo(125 * 60000), "2 h 5 min ago");
  assert.equal(L.formatAgo(-1), "");
  assert.equal(L.formatAgo(NaN), "");
});

test("formatTime is 24 h with seconds", () => {
  assert.equal(L.formatTime("2026-10-10T13:05:09Z", "UTC"), "13:05:09");
  assert.equal(L.formatTime("garbage", "UTC"), "");
});

test("clockState flags unset clocks and drift over 5 s", () => {
  const now = Date.parse("2026-10-10T13:00:00Z");
  assert.deepEqual(L.clockState({ source: "unsynced", now: "2026-10-10T13:00:00Z" }, now), { unset: true, drift: 0, warn: false });
  assert.equal(L.clockState({ source: "browser", now: "2026-10-10T12:59:50Z" }, now).warn, true);
  assert.equal(L.clockState({ source: "browser", now: "2026-10-10T12:59:57Z" }, now).warn, false);
  assert.equal(L.clockState({ source: "system", now: "2026-10-10T13:00:10Z" }, now).drift, -10);
});

test("keypad opens only while active", () => {
  assert.equal(L.keypadOpen("active"), true);
  for (const s of ["setup", "complete", "secured", "checking_in", "checked_in"]) assert.equal(L.keypadOpen(s), false);
  assert.equal(L.stateLabel("secured"), "Secured for travel");
});

test("actionsFor follows the lifecycle", () => {
  assert.deepEqual(L.actionsFor("checkpoint", "setup"), ["start", "reset"]);
  assert.deepEqual(L.actionsFor("checkpoint", "active"), ["complete", "secure", "reset"]);
  assert.deepEqual(L.actionsFor("checkpoint", "complete"), ["secure", "check-in", "cleanup-graywolf", "reset"]);
  assert.deepEqual(L.actionsFor("checkpoint", "secured"), ["check-in", "cleanup-graywolf", "reset"]);
  assert.deepEqual(L.actionsFor("checkpoint", "checking_in"), ["secure", "reset"]);
  assert.deepEqual(L.actionsFor("checkpoint", "checked_in"), ["cleanup-graywolf", "reset"]);
  assert.deepEqual(L.actionsFor("hq", "active"), ["complete", "reset"]);
  assert.deepEqual(L.actionsFor("hq", "complete"), ["cleanup-graywolf", "reset"]);
  assert.deepEqual(L.actionsFor("", "setup"), []);
});

test("entryBadge", () => {
  assert.equal(L.entryBadge({ State: "queued" }).label, "queued");
  assert.equal(L.entryBadge({ State: "confirmed" }).cls, "ok");
  assert.equal(L.entryBadge({ State: "sent", Voided: true, VoidState: "queued" }).label, "voiding");
  assert.equal(L.entryBadge({ State: "recorded", Voided: true }).label, "voided");
});

test("deliverySummary", () => {
  const now = Date.parse("2026-10-10T13:10:00Z");
  assert.equal(L.deliverySummary({ role: "hq" }, now), "");
  assert.equal(L.deliverySummary({ role: "checkpoint", unconfirmed: 0 }, now), "All entries confirmed by HQ, no HQ contact yet");
  assert.equal(
    L.deliverySummary({ role: "checkpoint", unconfirmed: 3, last_hq_contact: "2026-10-10T13:05:00Z" }, now),
    "3 unconfirmed, last HQ contact 5 min ago",
  );
});

test("cellText lists every passage", () => {
  assert.equal(L.cellText({ Times: ["2026-10-10T13:05:09Z", "2026-10-10T15:40:00Z"] }, "UTC"), "13:05 / 15:40");
  assert.equal(L.cellText({ Times: [] }, "UTC"), "");
  assert.equal(L.cellText(undefined, "UTC"), "");
});

test("healthFlags", () => {
  const now = Date.parse("2026-10-10T14:00:00Z");
  assert.deepEqual(L.healthFlags({}, now), ["never heard"]);
  const flags = L.healthFlags({
    LastHeardAt: "2026-10-10T13:00:00Z", Missing: [3, 4], GivenUp: [3], SenderMismatch: true,
    LastSourceCall: "SPOOF", ExpectedCall: "N0CALL-1", ClockSkewSec: -45, BadReports: 2, SeqReuseCount: 1,
  }, now);
  assert.deepEqual(flags, [
    "quiet for 15+ min", "2 batch(es) missing", "1 given up", "heard from SPOOF, expected N0CALL-1",
    "clock off by -45 s", "2 unreadable report(s)", "checkpoint restarted its numbering",
  ]);
  assert.deepEqual(L.healthFlags({ LastHeardAt: "2026-10-10T13:59:00Z", ClockSkewSec: 0 }, now), []);
});

test("requestId is unique", () => {
  assert.notEqual(L.requestId(), L.requestId());
});

test("keepPending keeps the request id whenever the save may have happened", () => {
  const err = (status, code, extra = {}) => ({ status, code, ...extra });
  assert.equal(L.keepPending(err(0, "network", { network: true })), true);
  assert.equal(L.keepPending(err(409, "in_progress")), true);
  assert.equal(L.keepPending(err(502, "graywolf_error")), true);
  assert.equal(L.keepPending(err(500, "internal")), true);
  assert.equal(L.keepPending(err(408)), true);
  assert.equal(L.keepPending(err(429, "rate_limited")), true);
  assert.equal(L.keepPending(new SyntaxError("bad JSON")), true);
  // Definite refusals: nothing was saved, so a new attempt is a new id.
  assert.equal(L.keepPending(err(400, "invalid_bib")), false);
  assert.equal(L.keepPending(err(409, "wrong_state")), false);
  assert.equal(L.keepPending(err(422, "request_id_reused")), false);
});

test("linkSummary describes a checkpoint's latest link check", () => {
  const now = Date.parse("2026-10-10T07:00:00Z");
  assert.equal(L.linkSummary(null, now), "never checked");
  assert.equal(
    L.linkSummary({ verdict: "PASS", heard: 5, total: 5, level: -22, at: "2026-10-10T06:50:00Z", side: "hq" }, now),
    "PASS 5/5, -22 dBFS, 10 min ago");
  assert.equal(
    L.linkSummary({ verdict: "FAIL", heard: 1, total: 5, at: "2026-10-10T06:59:55Z", side: "checkpoint" }, now),
    "FAIL 1/5, just now");
});

test("levelNote flags audio that is too hot or very low", () => {
  assert.equal(L.levelNote(-3), " (too hot)");
  assert.equal(L.levelNote(-45), " (very low)");
  assert.equal(L.levelNote(-20), "");
  assert.equal(L.levelNote(undefined), "");
});

test("linkProgress describes a running check", () => {
  assert.equal(L.linkProgress({ state: "requested" }), "Starting…");
  assert.equal(L.linkProgress({ state: "running", probes_sent: 2, count: 5 }), "Probe 2 of 5 sent…");
  assert.equal(L.linkProgress({ state: "running", probes_sent: 5, count: 5, reply_received: false }), "All probes sent; waiting for the reply…");
  assert.equal(L.linkProgress({ state: "done", verdict: "PASS" }), "");
});

test("a checkpoint's states and actions use Open/Close checkpoint wording", () => {
  assert.equal(L.stateLabel("active", "checkpoint"), "Checkpoint open");
  assert.equal(L.stateLabel("complete", "checkpoint"), "Checkpoint closed");
  assert.equal(L.stateLabel("setup", "checkpoint"), "Checkpoint not open");
  assert.equal(L.stateLabel("secured", "checkpoint"), "Secured for travel");
  assert.equal(L.stateLabel("complete", "hq"), "Race complete");
  assert.equal(L.actionLabel("start", "checkpoint")[0], "Open checkpoint");
  assert.equal(L.actionLabel("complete", "checkpoint")[0], "Close checkpoint");
  assert.equal(L.actionLabel("complete", "hq")[0], "Complete race");
  assert.equal(L.actionLabel("check-in", "checkpoint")[0], "Final check-in");
});

test("healthFlags shows a closed checkpoint as closed, not quiet", () => {
  const now = Date.parse("2026-10-10T12:00:00Z");
  const closed = { LastHeardAt: "2026-10-10T10:00:00Z", ClosedAt: "2026-10-10T09:58:00Z" };
  const flags = L.healthFlags(closed, now);
  assert.ok(flags[0].startsWith("closed "), flags.join("; "));
  assert.ok(!flags.some((f) => f.includes("quiet")), flags.join("; "));
  assert.ok(L.healthFlags({ LastHeardAt: "2026-10-10T10:00:00Z" }, now).some((f) => f.includes("quiet")));
});

test("sameStation matches graywolf's own-call rule", () => {
  assert.equal(L.sameStation("N0CALL", "n0call"), true);
  assert.equal(L.sameStation("N0CALL-0", "N0CALL"), true);
  assert.equal(L.sameStation(" n0call-4 ", "N0CALL-4"), true);
  assert.equal(L.sameStation("N0CALL-3", "N0CALL-4"), false);
  assert.equal(L.sameStation("", ""), false);
  assert.equal(L.sameStation(undefined, "N0CALL"), false);
});

test("linkTargets lists HQ's checkpoints for the link check", () => {
  const now = Date.parse("2026-10-10T13:00:00Z");
  const health = [
    { CPCode: "AS1", Name: "Ridge", Defined: true, ExpectedCall: "KD2DCM-4", LastHeardAt: "2026-10-10T12:55:00Z" },
    { CPCode: "AS2", Name: "Creek", Defined: true, ExpectedCall: "KD2DCM-5" },
    { CPCode: "AS3", Name: "Summit", Defined: true, ExpectedCall: "" },
    { CPCode: "ZZ9", Defined: false, LastSourceCall: "W1AW-7", LastHeardAt: "2026-10-10T12:59:30Z" },
    { CPCode: "QQ1", Defined: false, LastSourceCall: "" },
  ];
  assert.deepEqual(L.linkTargets(health, now), [
    { value: "KD2DCM-4", label: "AS1 Ridge (KD2DCM-4) · heard 5 min ago", disabled: false },
    { value: "KD2DCM-5", label: "AS2 Creek (KD2DCM-5)", disabled: false },
    { value: "", label: "AS3 Summit: no callsign (add one on the HQ tab)", disabled: true },
    { value: "W1AW-7", label: "ZZ9 (heard from W1AW-7, not on the list) · heard 30 s ago", disabled: false },
  ]);
  // Last heard from another call: say so, so HQ probes the right one.
  assert.deepEqual(L.linkTargets([{ CPCode: "AS4", Name: "Gap", Defined: true, ExpectedCall: "KD2DCM-6",
    SenderMismatch: true, LastSourceCall: "KD2DCM-9" }], now), [
    { value: "KD2DCM-6", label: "AS4 Gap (KD2DCM-6) · last heard from KD2DCM-9", disabled: false },
  ]);
  assert.deepEqual(L.linkTargets([], now), []);
  assert.deepEqual(L.linkTargets(undefined, now), []);
});

test("radioSummary says what's wrong in one line", () => {
  assert.deepEqual(L.radioSummary({ status: "ok", items: [{ key: "build", label: "graywolf", status: "ok" }] }),
    { kind: "info", text: "Radio: all checks pass." });
  const r = { status: "fail", items: [
    { key: "build", label: "graywolf", status: "fail" },
    { key: "rate", label: "Audio sample rate", status: "fail" },
    { key: "level", label: "Receive audio level", status: "warn" },
    { key: "keepup", label: "Modem keeping up", status: "unknown" },
  ] };
  assert.deepEqual(L.radioSummary(r), { kind: "bad",
    text: "Radio: graywolf, Audio sample rate failed; Receive audio level needs a look. See Station → Radio." });
  assert.deepEqual(L.radioSummary({ status: "warn", items: [{ label: "Packets decoded", status: "warn" }] }),
    { kind: "warn", text: "Radio: Packets decoded needs a look. See Station → Radio." });
  // Unchecked items are said, not hidden behind "all checks pass".
  assert.deepEqual(L.radioSummary({ status: "ok", items: [
    { label: "graywolf", status: "ok" }, { label: "Modem keeping up", status: "unknown" },
  ] }), { kind: "info", text: "Radio: no problems found; Modem keeping up not checked yet." });
  assert.equal(L.radioSummary(undefined), null);
  assert.equal(L.radioIcon("ok"), "✓");
  assert.equal(L.radioIcon("fail"), "✗");
  assert.equal(L.radioIcon("warn"), "⚠");
  assert.equal(L.radioIcon("unknown"), "?");
});
