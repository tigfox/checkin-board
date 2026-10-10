// Race config files (phase 12a): load a race's settings from one file
// (stations, HQ's checkpoint list, event page, graywolf settings). Loading
// only sets values; every setting stays editable on this device.
import { del, get, post } from "./api.js";
import { h } from "./dom.js";

const path = (name) => `/api/admin/raceconfigs/${encodeURIComponent(name)}`;

function changeRows(changes) {
  return changes.map((c) => h("tr", {}, h("td", {}, c.label), h("td", { class: "mono" }, c.from || "(empty)"),
    h("td", { class: "mono" }, c.to || "(empty)")));
}

function changeTable(changes) {
  return h("div", { class: "scroll" }, h("table", {},
    h("thead", {}, h("tr", {}, h("th", {}, "Setting"), h("th", {}, "Now"), h("th", {}, "From the file"))),
    h("tbody", {}, ...changeRows(changes))));
}

async function showPlan(out, name, station, label, ctx) {
  out.replaceChildren(h("p", { class: "muted" }, "Working out the changes…"));
  const plan = await ctx.run(() => post(`${path(name)}/preview`, { station }));
  if (!plan) {
    out.replaceChildren();
    return;
  }
  const app = [...plan.settings, ...(plan.checkpoints ? [plan.checkpoints] : []), ...(plan.event_page ? [plan.event_page] : [])];
  const withGW = h("input", { type: "checkbox", id: "rc-graywolf" });
  out.replaceChildren(
    ...(plan.problems || []).map((p) => h("p", { class: "banner warn" }, p)),
    h("h3", {}, "This node's settings"),
    app.length ? changeTable(app) : h("p", { class: "muted" }, "No changes: this node already matches the file."),
    h("h3", {}, "graywolf"),
    plan.graywolf.length
      ? h("div", {}, changeTable(plan.graywolf),
        h("label", { class: "row" }, withGW,
          " Also apply these graywolf changes. They change graywolf for everything on this node; its current values are saved for Restore graywolf settings."))
      : h("p", { class: "muted" }, "No graywolf changes."),
    h("p", {}, h("button", {
      type: "button", class: "primary", id: "rc-apply", disabled: !plan.can_apply,
      onclick: async () => {
        const gw = withGW.checked && plan.graywolf.length > 0;
        if (!confirm(`Load ${name} as ${label}${gw ? ", including the graywolf changes" : ""}? Every setting stays editable afterwards.`)) return;
        // The token makes the node apply exactly this preview (or refuse).
        const r = await ctx.run(() => post(`${path(name)}/apply`, { station, graywolf: gw, token: plan.token }), "Race config loaded.");
        if (!r) return;
        await ctx.reload();
        await ctx.refresh();
        if (r.graywolf_errors?.length) ctx.notify(`Race config loaded, but some graywolf changes failed: ${r.graywolf_errors.join("; ")}`, "warn");
      },
    }, "Load this race config")),
  );
}

async function showStations(out, name, ctx) {
  const info = await ctx.run(() => get(path(name)));
  if (!info) return;
  const pick = h("select", { id: "rc-station" },
    ...info.stations.map((st) => h("option", { value: st.id, selected: st.id === info.suggested }, st.label)));
  const plan = h("div", { id: "rc-plan" });
  // A preview belongs to one station: choosing another clears it.
  pick.addEventListener("change", () => plan.replaceChildren());
  const label = () => pick.selectedOptions[0]?.textContent || pick.value;
  out.replaceChildren(
    h("h3", {}, `${name}${info.race_name ? ` · ${info.race_name}` : ""}`),
    h("label", { for: "rc-station" }, info.suggested ? "This node is (matched by its callsign)" : "This node is"),
    pick,
    h("p", {}, h("button", { type: "button", id: "rc-preview", onclick: () => showPlan(plan, name, pick.value, label(), ctx) }, "Preview changes")),
    plan);
}

export async function raceConfigCard(ctx) {
  const list = await get("/api/admin/raceconfigs");
  const loader = h("div", { id: "rc-load" });
  const file = h("input", { type: "file", accept: ".json,application/json", id: "rc-file" });
  const rows = list.files.map((f) => h("tr", {},
    h("td", { class: "mono" }, f.name),
    h("td", {}, f.error ? h("span", { class: "error" }, f.error) : `${f.race_name || "(no race name)"} · ${f.stations} station(s)`),
    h("td", {},
      f.error ? null : h("button", { type: "button", onclick: () => showStations(loader, f.name, ctx) }, "Load…"), " ",
      h("button", { type: "button", class: "danger", onclick: async () => {
        if (!confirm(`Delete ${f.name} from this node?`)) return;
        if (await ctx.run(() => del(path(f.name)), "Deleted.")) await ctx.refresh();
      } }, "Delete"))));
  return h("div", { class: "card", id: "race-config" },
    h("h2", {}, "Race config"),
    h("p", { class: "muted" }, "Load this race's settings from one file: this node's role, names, codes and HQ callsign, the messaging settings, HQ's checkpoint list, the event page, and (if you choose) graywolf settings. Loading only sets values: every setting stays editable on this device."),
    list.can_apply ? null : h("p", { class: "banner info" }, "The race has started: a race config loads only before the race starts (or after a reset). Settings can still be edited on the Station page."),
    list.files.length
      ? h("div", { class: "scroll" }, h("table", {}, h("tbody", {}, ...rows)))
      : h("p", { class: "muted" }, "No race config files on this node yet. Upload one, or copy files into /var/lib/checkin-board/race-configs/."),
    h("div", { class: "row" }, file, h("button", { type: "button", onclick: async () => {
      if (!file.files.length) return;
      const send = (replace) => {
        const fd = new FormData();
        fd.append("file", file.files[0]);
        if (replace) fd.append("replace", "1");
        return post("/api/admin/raceconfigs", fd);
      };
      let r;
      try {
        r = await send(false);
      } catch (e) {
        if (e.code !== "exists") {
          ctx.notify(e.message, "bad");
          return;
        }
        if (!confirm("A race config with that name is already on this node. Replace it?")) return;
        r = await ctx.run(() => send(true));
      }
      if (r) {
        ctx.notify(`Saved ${r.name}.`);
        await ctx.refresh();
      }
    } }, "Upload")),
    ctx.settings.role === "hq"
      ? h("p", {}, h("a", { href: "/api/admin/raceconfig/export", download: "race-config.json" }, "Export this race's setup"),
        " (HQ's checkpoint list, callsigns, messaging settings and event page, to hand to the checkpoints)")
      : null,
    list.graywolf_backup
      ? h("p", {}, h("button", { type: "button", id: "rc-restore", onclick: async () => {
        if (!confirm("Put graywolf's settings back as they were before a race config changed them?")) return;
        const r = await ctx.run(() => post("/api/admin/graywolf/restore"),
          (res) => ["graywolf settings restored.", ...(res.notes || [])].join(" "));
        if (r) await ctx.refresh();
      } }, "Restore graywolf settings"))
      : null,
    loader);
}
