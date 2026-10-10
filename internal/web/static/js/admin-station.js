// Station tab: graywolf callsign, race and messaging settings, graywolf
// connection, peers (spec 8.1).
import { get, post, put } from "./api.js";
import { field, h, show } from "./dom.js";
import { messagingSettingsForm } from "./admin-messaging.js";
import * as L from "./logic.js";

// MAX_STATION_NAME matches store.MaxTacticalLen.
const MAX_STATION_NAME = 25;

export async function renderStation(sec, ctx) {
  const s = ctx.settings;
  const gw = await get("/api/admin/gw");
  sec.append(h("h1", {}, "Station"));
  sec.append(h("p", {}, h("a", { href: "/guide", id: "guide-link" }, "Station guide"),
    ": what each setting does, radio and graywolf setup, Pi Zero notes, and the 2 m band plan."));
  if (s.role === "checkpoint" && L.sameStation(gw.callsign, s.hq_call)) {
    sec.append(h("p", { class: "banner warn" },
      `The HQ callsign ${s.hq_call.toUpperCase()} is this station's own callsign: graywolf ignores messages from its own call, so this checkpoint would never hear HQ. Give each station its own callsign-SSID.`));
  }

  // graywolf callsign: changes graywolf for everything, so it confirms.
  const call = h("input", { id: "callsign", value: gw.callsign || "", autocapitalize: "characters" });
  sec.append(h("div", { class: "card" },
    h("h2", {}, "Callsign (graywolf)"),
    h("p", { class: "muted" }, "This is the Amateur Operator's callsign. This is used for all messaging at this station."),
    h("div", { class: "row" }, call, h("button", {
      type: "button",
      onclick: async () => {
        if (!confirm(`Change the Amateur Operator's callsign to ${call.value.toUpperCase()}? It is used for all messaging at this station.`)) return;
        const r = await ctx.run(() => put("/api/admin/callsign", { callsign: call.value, confirm: true }), "Callsign changed in graywolf.");
        if (r) await afterSave(ctx, r, "Callsign changed in graywolf.");
      },
    }, "Change")),
  ));

  sec.append(raceSettingsForm(s, ctx), messagingSettingsForm(s, ctx));

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

// afterSave re-reads the settings (without re-rendering, so unsaved
// edits in other cards survive) and adds any warnings the save returned
// (e.g. HQ callsign = this station's own) to the saved message.
async function afterSave(ctx, r, saved) {
  await ctx.reload();
  if (r.warnings?.length) ctx.notify(`${saved} ${r.warnings.join(" ")}`, "warn");
}

// raceSettingsForm is who this node is in the race. Fields for the
// other role are hidden; the radio tunables are in Messaging settings.
function raceSettingsForm(s, ctx) {
  const roleSel = h("select", { id: "role" },
    ...[["", "(not set)"], ["checkpoint", "Checkpoint"], ["hq", "HQ / net control"]]
      .map(([v, t]) => h("option", { value: v, selected: v === s.role }, t)));
  const forRole = (role, ...children) => h("div", { dataset: { role } }, ...children);
  const form = h("form", { class: "card", id: "race-settings" },
    h("h2", {}, "Race settings"),
    h("label", { for: "role" }, "Role"), roleSel,
    h("div", { class: "grid2" },
      h("div", {}, ...field("race_name", "Race name", s.race_name)),
      h("div", {}, ...field("station_tactical", "Station name", s.station_tactical, { maxlength: MAX_STATION_NAME },
        `What people call this station, e.g. "Ridge Aid #3". Shown on the keypad and panel; never sent on air. Up to ${MAX_STATION_NAME} characters.`)),
      forRole("checkpoint", ...field("checkpoint_code", "Checkpoint code", s.checkpoint_code, { maxlength: 6, autocapitalize: "characters" },
        "Sent in every radio report, e.g. AS5. Must match HQ's checkpoint list. A-Z and 0-9, up to 6.")),
      forRole("checkpoint", ...field("hq_call", "HQ callsign", s.hq_call, { autocapitalize: "characters" },
        "HQ's station callsign (optional -SSID). Reports go here; only HQ can ask this station to resend.")),
      forRole("hq", ...field("hq_local_codes", "Local codes, comma separated", (s.hq_local_codes || []).join(","), { autocapitalize: "characters" },
        "Codes HQ logs on its own keypad, e.g. START,FIN. Not sent on air.")),
    ),
    h("p", {}, h("button", { type: "submit", class: "primary" }, "Save race settings")),
  );
  const showRoleFields = () => {
    for (const el of form.querySelectorAll("[data-role]")) show(el, el.dataset.role === roleSel.value);
  };
  roleSel.addEventListener("change", showRoleFields);
  showRoleFields();
  form.addEventListener("submit", async (ev) => {
    ev.preventDefault();
    const v = (id) => form.querySelector(`#${id}`).value;
    const body = {
      role: v("role"), race_name: v("race_name"), station_tactical: v("station_tactical"),
      checkpoint_code: v("checkpoint_code"), hq_call: v("hq_call"),
      hq_local_codes: v("hq_local_codes").split(",").map((c) => c.trim()).filter(Boolean),
    };
    const r = await ctx.run(() => put("/api/admin/settings/race", body), "Race settings saved.");
    if (r) await afterSave(ctx, r, "Race settings saved.");
  });
  return form;
}
