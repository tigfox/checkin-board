// HQ status board with the event's branding (spec 8.3).
import { get } from "./api.js";
import { $, banner, clear, h, poll, show } from "./dom.js";
import * as L from "./logic.js";

const HEX = /^#[0-9A-Fa-f]{6}$/;
let board = null;

async function applyBranding() {
  const b = await get("/api/branding");
  const root = document.documentElement.style;
  for (const [k, v] of Object.entries(b.colors || {})) {
    if (HEX.test(v)) root.setProperty(`--brand-${k}`, v); // validated values only
  }
  $("#brand-header").textContent = b.header_text || "Status board";
  document.title = `${b.header_text || "Status board"} · checkin-board`;
  $("#brand-footer").textContent = b.footer_text || "";
  show($("#brand-footer"), !!b.footer_text);
  const logo = $("#brand-logo");
  if (b.has_logo) {
    logo.src = `/api/branding/logo?v=${encodeURIComponent(b.logo_sha256)}`;
    logo.alt = b.header_text || "";
  }
  show(logo, b.has_logo);
}

function renderHealth() {
  const ul = clear($("#health"));
  const now = Date.now();
  for (const c of board.Health || []) {
    const flags = L.healthFlags(c, now);
    const heard = c.LastHeardAt ? `heard ${L.formatAgo(now - new Date(c.LastHeardAt).getTime())}` : "";
    ul.append(h("li", {},
      h("strong", {}, c.Name || c.CPCode), ` (${c.CPCode}) `,
      h("span", { class: "muted" }, heard), " ",
      flags.length ? h("span", { class: "flag" }, flags.join("; ")) : h("span", { class: "ok" }, "OK"),
    ));
  }
}

function renderTable() {
  const q = $("#search").value.trim().replace(/^0+/, "");
  const table = clear($("#board"));
  const cps = board.Checkpoints || [];
  table.append(h("thead", {}, h("tr", {},
    h("th", {}, "Bib"), h("th", {}, "Category"),
    ...cps.map((c) => h("th", { title: c.Defined ? c.Code : "not on HQ's checkpoint list" }, c.Name || c.Code)),
    h("th", {}, "Last seen"),
  )));
  const tbody = h("tbody");
  for (const r of board.Runners || []) {
    if (q && String(r.Bib) !== q) continue;
    const latest = cps.findIndex((c) => c.Code === r.LastSeenCP);
    tbody.append(h("tr", { class: r.InRoster ? "" : "unknown" },
      h("td", { class: "mono" }, r.Bib),
      h("td", {}, r.Category || ""),
      ...r.Cells.map((cell, i) => h("td", { class: `mono${i === latest ? " latest" : ""}`, title: cell.Doubled ? "logged more than once" : "" },
        L.cellText(cell) + (cell.Doubled ? " ×2" : ""))),
      h("td", {}, r.LastSeenAt ? `${r.LastSeenCP} ${L.formatTime(r.LastSeenAt).slice(0, 5)}` : ""),
    ));
  }
  table.append(tbody);
}

async function refresh() {
  try {
    board = await get("/api/admin/board");
    banner($("#error"), "");
    renderHealth();
    renderTable();
    $("#updated").textContent = `updated ${L.formatTime(board.GeneratedAt)}`;
  } catch (e) {
    banner($("#error"), e.message, "bad");
  }
}

$("#search").addEventListener("input", () => board && renderTable());
$("#print").addEventListener("click", () => window.print());
await applyBranding().catch((e) => banner($("#error"), e.message, "bad"));
poll(refresh, 15000);
