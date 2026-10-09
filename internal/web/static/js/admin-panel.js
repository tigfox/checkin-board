// Node panel: the e-ink display and its two-button menu (spec 8.4).
import { get, post, put } from "./api.js";
import { h } from "./dom.js";

const PREVIEW_MS = 15000;

export async function renderPanel(sec, ctx) {
  const settings = await get("/api/admin/panel");
  sec.append(h("h1", {}, "Node panel"));
  sec.append(h("p", { class: "muted" },
    "The e-ink display on the node shows its status, refreshed every few minutes. ",
    "It rests at least 3 minutes between refreshes, to protect it. ",
    "(The bonnet's buttons and menu are shelved: the display can't update fast enough for a menu.)"));

  // Preview: what the status screen shows now.
  const img = h("img", { class: "panel-preview", alt: "Panel status screen preview", width: 500, height: 244 });
  const reload = () => { img.src = `/api/admin/panel/preview.png?t=${Date.now()}`; };
  reload();
  sec.append(h("div", { class: "card" }, h("h2", {}, "Status screen"), img,
    h("p", { class: "muted" }, "The node's own address appears where the preview says <node address>."),
    h("div", { class: "row" },
      h("button", { type: "button", onclick: () => ctx.run(() => post("/api/admin/panel/refresh"), (r) => r.message) }, "Refresh the display"))));
  const follow = async () => {
    await new Promise((resolve) => setTimeout(resolve, PREVIEW_MS));
    if (!sec.isConnected) return;
    if (sec.offsetParent !== null) reload(); // only while the tab is on screen
    follow();
  };
  follow();

  sec.append(settingsCard(settings, ctx));
}

function settingsCard(s, ctx) {
  const enabled = h("input", { type: "checkbox", id: "pn-enabled", checked: s.enabled });
  const controller = h("select", { id: "pn-controller" },
    ...s.controllers.map((c) => h("option", { value: c, selected: c === s.controller }, c.toUpperCase())));
  const refresh = h("input", { type: "number", id: "pn-refresh", min: s.min_refresh, max: s.max_refresh, value: s.refresh_min });
  const rotation = h("select", { id: "pn-rotation" },
    h("option", { value: "0", selected: s.rotation === 0 }, "0°"),
    h("option", { value: "180", selected: s.rotation === 180 }, "180° (upside down)"));
  const patterns = s.controllers.map((c) => h("button", { type: "button", onclick: () =>
    ctx.run(() => post("/api/admin/panel/test-pattern", { controller: c }), (r) => r.message) }, `Test pattern: ${c.toUpperCase()}`));
  return h("div", { class: "card" },
    h("h2", {}, "Display"),
    h("label", {}, enabled, " Panel on"),
    h("label", { for: "pn-controller" }, "Display controller (bonnet revisions differ)"), controller,
    h("p", { class: "muted" }, "SSD1680Z is the current bonnet. If the display stays blank or garbled, show a test pattern with each controller and choose the one that draws it cleanly."),
    h("label", { for: "pn-refresh" }, `Refresh the status every (minutes, ${s.min_refresh}-${s.max_refresh})`), refresh,
    h("label", { for: "pn-rotation" }, "Rotation"), rotation,
    h("div", { class: "row" },
      h("button", { type: "button", class: "primary", onclick: async () => {
        const ok = await ctx.run(() => put("/api/admin/panel", {
          enabled: enabled.checked, controller: controller.value, refresh_min: Number(refresh.value), rotation: Number(rotation.value),
        }), "Panel settings saved.");
        if (ok) await ctx.refresh();
      } }, "Save"),
      ),
    h("div", { class: "row" }, ...patterns));
}

