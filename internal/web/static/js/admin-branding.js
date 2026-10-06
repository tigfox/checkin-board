// Status-board branding editor with live contrast check (spec 8.3).
import { del, get, post, put } from "./api.js";
import { clear, h } from "./dom.js";

const HEX = /^#[0-9A-Fa-f]{6}$/;
const COLORS = [["primary", "Header bar"], ["accent", "Highlights"], ["background", "Page background"], ["text", "Text"]];

export async function renderBranding(sec, ctx) {
  const cur = await get("/api/admin/branding");
  const s = cur.settings;
  const d = cur.defaults;
  sec.append(h("h1", {}, "Board branding"));
  sec.append(h("p", { class: "muted" }, "Applies to the HQ status board only; the keypad keeps the plain theme."));

  const header = h("input", { id: "b-header", maxlength: 80, value: s.header_text, placeholder: "(race name)" });
  const footer = h("input", { id: "b-footer", maxlength: 200, value: s.footer_text });
  const pickers = {};
  const grid = h("div", { class: "grid2" });
  for (const [k, label] of COLORS) {
    pickers[k] = h("input", { type: "color", id: `b-${k}`, value: s.colors[k] || d[k] });
    grid.append(h("div", {}, h("label", { for: `b-${k}` }, label), pickers[k]));
  }
  const preview = h("div", { class: "card", id: "b-preview" });
  const readout = h("ul", { id: "b-contrast" });
  // After "Reset to default" the colours are saved empty, so the board
  // follows the defaults; touching a picker makes them explicit again.
  let useDefaults = COLORS.every(([k]) => !s.colors[k]);
  const body = () => ({
    header_text: header.value, footer_text: footer.value,
    colors: Object.fromEntries(COLORS.map(([k]) => [k, useDefaults ? "" : pickers[k].value.toUpperCase()])),
  });
  const shown = () => Object.fromEntries(COLORS.map(([k]) => [k, pickers[k].value.toUpperCase()]));

  function paint() {
    const b = body();
    const c = shown();
    if (![c.primary, c.background, c.text, c.accent].every((v) => HEX.test(v))) return;
    clear(preview);
    const bar = h("div", {}, b.header_text || ctx.settings.race_name || "Status board");
    bar.style.background = c.primary; // CSSOM, validated hex only
    bar.style.color = c.background;
    bar.style.padding = "8px";
    const page = h("div", {}, "Bib 101 · AS5 ", h("strong", {}, "13:05"));
    page.style.background = c.background;
    page.style.color = c.text;
    page.style.padding = "8px";
    page.lastChild.style.color = c.accent;
    preview.append(h("strong", {}, "Preview"), bar, page, b.footer_text ? h("p", {}, b.footer_text) : null);
  }

  let timer;
  let checkGen = 0;
  async function check() {
    paint();
    clearTimeout(timer);
    timer = setTimeout(async () => {
      const gen = ++checkGen;
      const r = await post("/api/admin/branding/check", body()).catch(() => null);
      if (gen !== checkGen) return; // a newer check is on its way
      clear(readout);
      if (!r) return;
      for (const c of r.contrasts || []) {
        readout.append(h("li", { class: c.ok ? "ok" : "error" }, `${c.pair}: ${c.ratio.toFixed(1)}:1 (needs ${c.min}:1)`));
      }
      for (const p of r.problems || []) readout.append(h("li", { class: "error" }, p));
    }, 250);
  }
  for (const el of [header, footer]) el.addEventListener("input", check);
  for (const el of Object.values(pickers)) {
    el.addEventListener("input", () => {
      useDefaults = false;
      check();
    });
  }

  sec.append(h("div", { class: "card" },
    h("label", { for: "b-header" }, "Header text"), header,
    h("label", { for: "b-footer" }, "Footer text"), footer,
    grid, preview, h("h3", {}, "Readability"), readout,
    h("div", { class: "row" },
      h("button", { type: "button", class: "primary", onclick: () => ctx.run(() => put("/api/admin/branding", body()), "Branding saved.") }, "Save"),
      h("button", { type: "button", onclick: async () => {
        for (const [k] of COLORS) pickers[k].value = d[k];
        useDefaults = true;
        header.value = "";
        footer.value = "";
        await check();
      } }, "Reset to default")),
  ));

  const file = h("input", { type: "file", accept: "image/png,image/jpeg,image/webp" });
  sec.append(h("div", { class: "card" },
    h("h2", {}, "Logo"),
    h("p", { class: "muted" }, "PNG, JPEG or WebP, up to 1 MB. It is resized to at most 512 px tall. SVG isn't accepted."),
    h("div", { class: "row" }, file,
      h("button", { type: "button", onclick: async () => {
        if (!file.files.length) return;
        const fd = new FormData();
        fd.append("file", file.files[0]);
        await ctx.run(() => put("/api/admin/branding/logo", fd), (r) => `Logo saved (${r.width}×${r.height}).`);
      } }, "Upload"),
      h("button", { type: "button", class: "danger", onclick: () => ctx.run(() => del("/api/admin/branding/logo"), "Logo removed.") }, "Remove logo")),
  ));
  await check();
}
