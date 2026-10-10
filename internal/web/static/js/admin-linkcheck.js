// Deployment link check (spec 4.8).
import { get, post } from "./api.js";
import { h, show } from "./dom.js";
import * as L from "./logic.js";

const POLL_MS = 2000;
// OTHER is the "Other callsign…" choice (not a valid callsign).
const OTHER = "*other";

function level(v) {
  return typeof v === "number" ? `${v} dBFS${L.levelNote(v)}` : "—";
}

function checkRow(c, ctx, refresh) {
  const active = c.state === "requested" || c.state === "running";
  const result = active ? L.linkProgress(c)
    : c.state === "cancelled" ? `Cancelled: ${c.error}` : c.verdict;
  return h("tr", {},
    h("td", { class: "mono" }, L.formatTime(c.requested_at)),
    h("td", { class: "mono" }, c.peer_call),
    h("td", {}, h("strong", {}, result)),
    h("td", {}, active ? "" : `${c.uplink}/${c.count}`),
    h("td", {}, active ? "" : `${c.round_trip}/${c.count}`),
    h("td", {}, active ? "" : c.reply_received ? "yes" : "no"),
    h("td", {}, typeof c.median_rtt_ms === "number" ? `${(c.median_rtt_ms / 1000).toFixed(1)} s` : "—"),
    h("td", {}, level(c.remote_level)),
    h("td", {}, level(c.local_level)),
    h("td", { class: "mono" }, c.via || "direct"),
    h("td", {}, active ? h("button", { type: "button", onclick: async () => {
      await ctx.run(() => post(`/api/admin/linkcheck/${c.id}/cancel`), "Link check cancelled.");
      await refresh();
    } }, "Cancel") : [c.advice, c.state === "done" && c.error ? ` (${c.error})` : ""].join("")),
  );
}

export async function renderLinkCheck(sec, ctx) {
  const s = ctx.settings;
  const isHQ = s.role === "hq";
  sec.append(h("h1", {}, "Link check"));
  sec.append(h("p", { class: "muted" },
    "Run at each station's location before the race: a few short test messages over the radio confirm the link to ",
    isHQ ? "a checkpoint" : "HQ", " works. Each run uses about a dozen transmissions; runs are at least 2 minutes apart. ",
    "Audio levels measure the sound card input, not RF signal strength."));

  const list = await get("/api/admin/linkcheck");
  const count = h("input", { type: "number", id: "lc-count", min: 1, max: list.max_count, value: list.default_count });
  let targetCall = () => "";
  const form = h("div", { class: "card" }, h("h2", {}, "Run a link check"));
  if (isHQ) {
    const { checkpoints } = await get("/api/admin/status");
    const other = h("input", { id: "lc-other", placeholder: "Callsign, e.g. KD2DCM-7", autocapitalize: "characters" });
    const otherRow = h("div", { class: "hidden" }, h("label", { for: "lc-other" }, "Other callsign"), other);
    const pick = h("select", { id: "lc-to" },
      h("option", { value: "", disabled: true, selected: true }, "Choose a checkpoint"),
      ...L.linkTargets(checkpoints, Date.now()).map((t) => h("option", { value: t.value, disabled: t.disabled }, t.label)),
      h("option", { value: OTHER }, "Other callsign…"));
    pick.addEventListener("change", () => show(otherRow, pick.value === OTHER));
    targetCall = () => (pick.value === OTHER ? other.value : pick.value);
    form.append(h("label", { for: "lc-to" }, "Checkpoint to probe"), pick, otherRow);
  } else {
    form.append(h("p", {}, "Probes HQ (", h("span", { class: "mono" }, s.hq_call || "HQ callsign not set"), ")."));
  }
  form.append(h("label", { for: "lc-count" }, `Probes (up to ${list.max_count}; use more on a doubtful link for a steadier verdict)`), count,
    h("p", {}, h("button", { type: "button", class: "primary", onclick: async () => {
      const body = { to: targetCall(), count: Number(count.value) };
      if (isHQ && !body.to) {
        ctx.notify("Choose a checkpoint to probe, or enter another callsign.", "bad");
        return;
      }
      if (s.race_state === "active") {
        if (!confirm("The race is running. A link check uses airtime the race needs. Run it anyway?")) return;
        body.confirm = true;
      }
      const r = await ctx.run(() => post("/api/admin/linkcheck", body), "Link check started.");
      if (r) await ctx.refresh();
    } }, "Run link check")));
  sec.append(form);

  const tbody = h("tbody");
  const head = ["Started", "Peer", "Result", "Heard there", "ACKed", "Reply", "Round trip", "Level there", "Level here", "Path", ""];
  sec.append(h("div", { class: "card" }, h("h2", {}, "This station's checks"),
    h("div", { class: "scroll" }, h("table", {}, h("thead", {}, h("tr", {}, ...head.map((t) => h("th", {}, t)))), tbody))));

  const respBody = h("tbody");
  sec.append(h("div", { class: "card" }, h("h2", {}, "Checks other stations ran towards this one"),
    h("div", { class: "scroll" }, h("table", {},
      h("thead", {}, h("tr", {}, ...["Last heard", "From", "Code", "Heard", "Level here", "Path", "Reply", "Verdict"].map((t) => h("th", {}, t)))),
      respBody))));

  const fill = (data) => {
    tbody.replaceChildren(...data.checks.map((c) => checkRow(c, ctx, refresh)));
    if (!data.checks.length) tbody.append(h("tr", {}, h("td", { colspan: head.length }, "None yet.")));
    respBody.replaceChildren(...data.responses.map((r) => h("tr", {},
      h("td", { class: "mono" }, L.formatTime(r.last_heard_at)),
      h("td", { class: "mono" }, r.peer_call, r.unknown_peer ? " (not on the checkpoint list)" : ""),
      h("td", { class: "mono" }, r.prober_code),
      h("td", {}, `${r.heard}/${r.total}`),
      h("td", {}, level(r.level)),
      h("td", { class: "mono" }, r.via || "direct"),
      h("td", {}, r.reply_acked ? "sent, ACKed" : r.replied ? "sent" : "pending"),
      h("td", {}, r.verdict))));
    if (!data.responses.length) respBody.append(h("tr", {}, h("td", { colspan: 8 }, "None yet.")));
  };
  async function refresh() {
    fill(await get("/api/admin/linkcheck"));
  }
  fill(list);

  // Follow running checks while this tab's content is on screen: a tab
  // switch or re-render detaches it, which ends the loop.
  const follow = async () => {
    await new Promise((resolve) => setTimeout(resolve, POLL_MS));
    if (!sec.isConnected) return;
    const data = await get("/api/admin/linkcheck").catch(() => null);
    if (data && sec.isConnected) fill(data);
    follow();
  };
  follow();
}
