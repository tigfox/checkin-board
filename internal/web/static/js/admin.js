// Admin page shell: tabs, shared state, and the sections (spec 8.1).
import { get, post } from "./api.js";
import { $, banner, h, poll, show } from "./dom.js";
import * as L from "./logic.js";
import { renderRace } from "./admin-race.js";
import { renderStation } from "./admin-station.js";
import { renderOutbox } from "./admin-outbox.js";
import { renderHQ } from "./admin-hq.js";
import { renderBranding } from "./admin-branding.js";
import { renderPasswords } from "./admin-passwords.js";

const renderers = {
  "tab-race": renderRace, "tab-station": renderStation, "tab-outbox": renderOutbox,
  "tab-hq": renderHQ, "tab-branding": renderBranding, "tab-passwords": renderPasswords,
};

export const ctx = {
  settings: null,
  async reload() {
    ctx.settings = await get("/api/admin/settings");
    $("#title").textContent = ctx.settings.race_name ? `${ctx.settings.race_name} · Admin` : "Admin";
    $("#state").textContent = L.stateLabel(ctx.settings.race_state);
    show($("#board-link"), ctx.settings.role === "hq");
    buildTabs();
  },
  refresh: () => select(current),
  notify(msg, kind = "info") {
    banner($("#banner"), msg, kind);
    if (msg) window.scrollTo({ top: 0, behavior: "smooth" });
  },
  // run wraps an action: shows success or the server's error.
  async run(fn, okMsg) {
    try {
      const r = await fn();
      if (okMsg) ctx.notify(typeof okMsg === "function" ? okMsg(r) : okMsg, "info");
      return r;
    } catch (e) {
      ctx.notify(e.message, "bad");
      return undefined;
    }
  },
};

let current = "tab-race";
let renderGen = 0;

const visibleFor = (sec) => !sec.dataset.role || sec.dataset.role === ctx.settings.role;

function buildTabs() {
  const sections = [...document.querySelectorAll("main > section")];
  if (!sections.some((sec) => sec.id === current && visibleFor(sec))) current = "tab-race";
  const nav = $("#tabs");
  nav.replaceChildren();
  for (const sec of sections) {
    const visible = visibleFor(sec);
    show(sec, visible && sec.id === current);
    if (!visible) continue;
    nav.append(h("button", {
      type: "button", role: "tab", "aria-selected": String(sec.id === current),
      onclick: () => select(sec.id),
    }, sec.dataset.tab));
  }
}

// select renders a tab off-screen and swaps it in, so a double click
// (or a refresh racing a click) can't interleave two renders.
export async function select(id) {
  current = id;
  buildTabs();
  const gen = ++renderGen;
  $(`#${id}`).replaceChildren(h("p", { class: "muted" }, "Loading…"));
  const body = h("div");
  try {
    await renderers[id](body, ctx);
  } catch (e) {
    body.append(h("p", { class: "error" }, e.message));
  }
  if (gen === renderGen && current === id) $(`#${id}`).replaceChildren(body);
}

$("#logout").addEventListener("click", async () => {
  await post("/api/logout");
  location.href = "/login.html";
});

try {
  await ctx.reload();
  await select(current);
} catch (e) {
  ctx.notify(`Can't load settings: ${e.message}. Reload the page to try again.`, "bad");
}

// Follow lifecycle changes made elsewhere (the engine finishing a final
// check-in, another admin). Only a state change re-renders the Race tab.
poll(async () => {
  if (!ctx.settings) return;
  const before = ctx.settings.race_state;
  try {
    await ctx.reload();
  } catch {
    return; // the next tick retries; actions report their own errors
  }
  if (ctx.settings.race_state !== before && current === "tab-race") await select(current);
}, 10000);
