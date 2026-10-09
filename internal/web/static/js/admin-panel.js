// Node panel: the e-ink display and its two-button menu (spec 8.4).
import { get, post, put } from "./api.js";
import { h } from "./dom.js";

const ROLE_LABELS = { both: "Both", checkpoint: "Checkpoint", hq: "HQ" };
const PREVIEW_MS = 15000;

export async function renderPanel(sec, ctx) {
  const [settings, menuView] = await Promise.all([get("/api/admin/panel"), get("/api/admin/panel/menu")]);
  sec.append(h("h1", {}, "Node panel"));
  sec.append(h("p", { class: "muted" },
    "The e-ink display on the node shows its status, refreshed every few minutes. Its two buttons open a menu: ",
    "top moves to the next item, bottom selects. Lifecycle items always ask for confirmation. ",
    "The display rests at least 3 minutes between full refreshes, to protect it."));

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
  sec.append(menuCard(menuView, ctx));
}

function settingsCard(s, ctx) {
  const enabled = h("input", { type: "checkbox", id: "pn-enabled", checked: s.enabled });
  const controller = h("select", { id: "pn-controller" },
    h("option", { value: "", selected: s.controller === "" }, "Detect at the node"),
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
    h("p", { class: "muted" }, s.controller ? `Using ${s.controller.toUpperCase()}.` : "Not known yet: at the node, the panel shows a setup screen with each controller in turn; press a button when it's readable."),
    h("label", { for: "pn-refresh" }, `Refresh the status every (minutes, ${s.min_refresh}-${s.max_refresh})`), refresh,
    h("label", { for: "pn-rotation" }, "Rotation"), rotation,
    h("div", { class: "row" },
      h("button", { type: "button", class: "primary", onclick: async () => {
        const ok = await ctx.run(() => put("/api/admin/panel", {
          enabled: enabled.checked, controller: controller.value, refresh_min: Number(refresh.value), rotation: Number(rotation.value),
        }), "Panel settings saved.");
        if (ok) await ctx.refresh();
      } }, "Save"),
      h("button", { type: "button", onclick: async () => {
        const ok = await ctx.run(() => post("/api/admin/panel/detect"), (r) => r.message);
        if (ok) await ctx.refresh();
      } }, "Detect the controller again")),
    h("div", { class: "row" }, ...patterns));
}

function menuCard(view, ctx) {
  // Work on a copy; nothing is saved until Save.
  let items = view.items.map((it) => ({ ...it }));
  const actions = view.actions;
  const body = h("tbody");
  const rolesFor = (action) => {
    const a = actions.find((x) => x.action === action);
    return a && a.roles !== "both" ? [a.roles] : ["both", "checkpoint", "hq"];
  };
  const lifecycle = (action) => (actions.find((x) => x.action === action) || {}).lifecycle;

  function row(it, i) {
    const label = h("input", { value: it.label, maxlength: view.max_label, "aria-label": "Label" });
    label.addEventListener("input", () => { it.label = label.value; });
    const action = h("select", { "aria-label": "Action" },
      ...actions.map((a) => h("option", { value: a.action, selected: a.action === it.action }, a.action.replace(/_/g, " "))));
    action.addEventListener("change", () => {
      it.action = action.value;
      if (!rolesFor(it.action).includes(it.roles)) it.roles = rolesFor(it.action)[0];
      draw();
    });
    const roles = h("select", { "aria-label": "Shows for" },
      ...rolesFor(it.action).map((r) => h("option", { value: r, selected: r === it.roles }, ROLE_LABELS[r])));
    roles.addEventListener("change", () => { it.roles = roles.value; });
    const confirm = h("input", { type: "checkbox", checked: it.confirm || lifecycle(it.action), disabled: lifecycle(it.action), "aria-label": "Confirm" });
    confirm.addEventListener("change", () => { it.confirm = confirm.checked; });
    const enabled = h("input", { type: "checkbox", checked: it.enabled, "aria-label": "Enabled" });
    enabled.addEventListener("change", () => { it.enabled = enabled.checked; });
    const move = (d) => () => {
      const j = i + d;
      if (j < 0 || j >= items.length) return;
      [items[i], items[j]] = [items[j], items[i]];
      draw();
    };
    return h("tr", {},
      h("td", {}, label), h("td", {}, action), h("td", {}, roles), h("td", {}, confirm), h("td", {}, enabled),
      h("td", { class: "row" },
        h("button", { type: "button", "aria-label": "Move up", onclick: move(-1) }, "↑"),
        h("button", { type: "button", "aria-label": "Move down", onclick: move(1) }, "↓"),
        h("button", { type: "button", class: "danger", onclick: () => { items.splice(i, 1); draw(); } }, "Remove")));
  }
  function draw() {
    body.replaceChildren(...items.map(row));
  }
  draw();

  return h("div", { class: "card" },
    h("h2", {}, "Button menu"),
    h("p", { class: "muted" },
      view.edited ? "Your menu." : "The default menu (not edited yet).",
      " Items show only for the roles chosen and only when they make sense in the race state (e.g. \"Open checkpoint\" before it's open). ",
      "Reset, cleanup, passwords and the callsign are never on the panel."),
    h("div", { class: "scroll" }, h("table", {},
      h("thead", {}, h("tr", {}, ...["Label", "Action", "Shows for", "Confirm", "On", ""].map((t) => h("th", {}, t)))),
      body)),
    h("div", { class: "row" },
      h("button", { type: "button", onclick: () => {
        if (items.length >= view.max_items) return ctx.notify(`At most ${view.max_items} items.`, "bad");
        items.push({ label: "Status", action: "status", roles: "both", confirm: false, enabled: true });
        draw();
      } }, "Add item"),
      h("button", { type: "button", class: "primary", onclick: async () => {
        const ok = await ctx.run(() => put("/api/admin/panel/menu", { items }), "Menu saved. The panel picks it up within 30 s.");
        if (ok) await ctx.refresh();
      } }, "Save menu"),
      h("button", { type: "button", onclick: async () => {
        if (!confirm("Go back to the default menu?")) return;
        const ok = await ctx.run(() => post("/api/admin/panel/menu/reset"), "Default menu restored.");
        if (ok) await ctx.refresh();
      } }, "Reset to default")));
}
