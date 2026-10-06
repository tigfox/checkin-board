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
    LastSourceCall: "SPOOF", ExpectedCall: "K1CP", ClockSkewSec: -45, BadReports: 2, SeqReuseCount: 1,
  }, now);
  assert.deepEqual(flags, [
    "quiet for 15+ min", "2 batch(es) missing", "1 given up", "heard from SPOOF, expected K1CP",
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
