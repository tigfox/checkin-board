// Station settings, graywolf connection, peers (spec 8.1).
import { get, post, put } from "./api.js";
import { h } from "./dom.js";

function field(id, label, value, attrs = {}) {
  return [h("label", { for: id }, label), h("input", { id, value: value ?? "", ...attrs })];
}

export async function renderStation(sec, ctx) {
  const s = ctx.settings;
  const gw = await get("/api/admin/gw");
  sec.append(h("h1", {}, "Station"));

  // graywolf callsign: changes graywolf for everything, so it confirms.
  const call = h("input", { id: "callsign", value: gw.callsign || "", autocapitalize: "characters" });
  sec.append(h("div", { class: "card" },
    h("h2", {}, "Callsign (graywolf)"),
    h("p", { class: "muted" }, "This is graywolf's station callsign: changing it changes it for all of graywolf, not just the race."),
    h("div", { class: "row" }, call, h("button", {
      type: "button",
      onclick: async () => {
        if (!confirm(`Change graywolf's station callsign to ${call.value.toUpperCase()}?`)) return;
        await ctx.run(() => put("/api/admin/callsign", { callsign: call.value, confirm: true }), "Callsign changed in graywolf.");
      },
    }, "Change")),
  ));

  const roleSel = h("select", { id: "role" },
    ...[["", "(not set)"], ["checkpoint", "Checkpoint"], ["hq", "HQ / net control"]]
      .map(([v, t]) => h("option", { value: v, selected: v === s.role }, t)));
  const form = h("form", { class: "card" },
    h("h2", {}, "Race settings"),
    h("label", { for: "role" }, "Role"), roleSel,
    h("div", { class: "grid2" },
      h("div", {}, ...field("race_name", "Race name", s.race_name)),
      h("div", {}, ...field("station_tactical", "Station tactical name (e.g. AID3)", s.station_tactical, { maxlength: 9 })),
      h("div", {}, ...field("checkpoint_code", "Checkpoint code (checkpoint)", s.checkpoint_code, { maxlength: 6 })),
      h("div", {}, ...field("hq_call", "HQ callsign (checkpoint)", s.hq_call)),
      h("div", {}, ...field("hq_local_codes", "Local codes, comma separated (HQ)", (s.hq_local_codes || []).join(","))),
      h("div", {}, ...field("path", "Digipeater path (blank = direct)", s.path)),
      h("div", {}, ...field("gw_channel", "graywolf channel (0 = default)", s.gw_channel, { type: "number", min: 0 })),
      h("div", {}, ...field("max_text_len", "Max message length", s.max_text_len, { type: "number" })),
      h("div", {}, ...field("flush_after_sec", "Batch after (s)", s.flush_after_sec, { type: "number" })),
      h("div", {}, ...field("max_in_flight", "Batches in flight", s.max_in_flight, { type: "number" })),
      h("div", {}, ...field("heartbeat_sec", "Heartbeat every (s)", s.heartbeat_sec, { type: "number" })),
      h("div", {}, ...field("gap_grace_sec", "Gap grace (s, HQ)", s.gap_grace_sec, { type: "number" })),
    ),
    h("p", {}, h("button", { type: "submit", class: "primary" }, "Save settings")),
  );
  form.addEventListener("submit", async (ev) => {
    ev.preventDefault();
    const v = (id) => form.querySelector(`#${id}`).value;
    const n = (id) => Number(v(id));
    const body = {
      role: v("role"), race_name: v("race_name"), station_tactical: v("station_tactical"),
      checkpoint_code: v("checkpoint_code"), hq_call: v("hq_call"),
      hq_local_codes: v("hq_local_codes").split(",").map((c) => c.trim()).filter(Boolean),
      path: v("path"), gw_channel: n("gw_channel"), max_text_len: n("max_text_len"),
      flush_after_sec: n("flush_after_sec"), max_in_flight: n("max_in_flight"),
      heartbeat_sec: n("heartbeat_sec"), gap_grace_sec: n("gap_grace_sec"),
    };
    if (await ctx.run(() => put("/api/admin/settings", body), "Settings saved.")) await ctx.reload();
  });
  sec.append(form);

  sec.append(h("div", { class: "card" },
    h("h2", {}, "graywolf connection"),
    h("p", {}, gw.reachable ? `graywolf ${gw.version} reachable` : "graywolf not reachable",
      gw.connected ? " · live updates on" : " · live updates off", gw.auth_failed ? " · LOGIN FAILED" : ""),
    gw.max_text ? h("p", {}, `Max message length ${gw.max_text}; message retention ${gw.retention_days || "forever"} day(s).`) : null,
    ...(gw.warnings || []).map((w) => h("p", { class: "banner warn" }, w)),
    gw.stream_error ? h("p", { class: "muted mono" }, gw.stream_error) : null,
    gw.skipped_rows ? h("p", { class: "banner warn" }, `${gw.skipped_rows} message(s) skipped after repeated failures. Last: ${gw.last_skipped}`) : null,
  ));

  const { peers } = await get("/api/admin/peers");
  sec.append(h("div", { class: "card" },
    h("h2", {}, "graywolf retries for race contacts"),
    h("p", { class: "muted" }, "During the race the app turns off graywolf's own message retries for these contacts (it retries itself). They're restored after the race."),
    peers.length ? h("ul", {}, ...peers.map((p) => h("li", { class: "mono" }, p.callsign))) : h("p", {}, "None changed."),
    peers.length ? h("button", { type: "button", onclick: async () => {
      await ctx.run(() => post("/api/admin/peers/restore"), "graywolf settings restored.");
    } }, "Restore now") : null,
  ));

  const since = h("input", { type: "datetime-local", id: "reread-since" });
  sec.append(h("div", { class: "card" },
    h("h2", {}, "Re-read graywolf messages"),
    h("p", { class: "muted" }, "Recovery after a wiped database: reads graywolf's stored race messages again from a time. Messages already processed are skipped."),
    h("div", { class: "row" }, since, h("button", { type: "button", onclick: async () => {
      if (!since.value) return;
      await ctx.run(() => post("/api/admin/inbox/reread", { since: new Date(since.value).toISOString() }), "Re-reading graywolf messages.");
    } }, "Re-read")),
  ));
}
