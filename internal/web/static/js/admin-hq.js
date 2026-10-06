// HQ tools: health, checkpoints, roster, imports, results (spec 4.2, 4.4).
import { del, get, post } from "./api.js";
import { h } from "./dom.js";
import * as L from "./logic.js";

function upload(label, path, ctx, describe) {
  const input = h("input", { type: "file", accept: ".csv,text/csv" });
  return h("div", { class: "row" }, input, h("button", { type: "button", onclick: async () => {
    if (!input.files.length) return;
    const fd = new FormData();
    fd.append("file", input.files[0]);
    const r = await ctx.run(() => post(path, fd), describe);
    if (r) await ctx.refresh();
  } }, label));
}

export async function renderHQ(sec, ctx) {
  const status = await get("/api/admin/status");
  const now = Date.now();
  sec.append(h("h1", {}, "HQ"));
  sec.append(h("p", {}, h("a", { href: "/board.html" }, "Open the status board"), " · ",
    h("a", { href: "/api/admin/export.csv", download: "results.csv" }, "Download results CSV")));

  sec.append(h("div", { class: "card" }, h("h2", {}, "Checkpoint health"),
    h("table", {}, h("thead", {}, h("tr", {}, ...["Checkpoint", "Heard", "Batches", "Problems", "Link check", ""].map((t) => h("th", {}, t)))),
      h("tbody", {}, ...status.checkpoints.map((c) => h("tr", {},
        h("td", {}, `${c.Name || c.CPCode} (${c.CPCode})`),
        h("td", {}, c.LastHeardAt ? L.formatAgo(now - new Date(c.LastHeardAt).getTime()) : "never"),
        h("td", {}, c.BatchesReceived ?? 0),
        h("td", {}, L.healthFlags(c, now).join("; ") || "OK"),
        h("td", {}, L.linkSummary((status.links || {})[c.CPCode], now)),
        h("td", {}, (c.Missing && c.Missing.length) ? h("button", { type: "button", onclick: async () => {
          await ctx.run(() => post(`/api/admin/status/${encodeURIComponent(c.CPCode)}/rerequest`), (r) => `Asked ${c.CPCode} for ${r.requested} batch(es).`);
        } }, "Re-request") : ""),
      )))),
    status.bad_reports && status.bad_reports.length ? h("details", {}, h("summary", {}, `${status.bad_reports.length} unreadable report(s)`),
      h("ul", {}, ...status.bad_reports.map((b) => h("li", { class: "mono" }, `${b.FromCall}: ${b.Text} (${b.Error})`)))) : null,
  ));

  const { checkpoints } = await get("/api/admin/checkpoints");
  const code = h("input", { placeholder: "Code", maxlength: 6 });
  const name = h("input", { placeholder: "Name" });
  const order = h("input", { type: "number", placeholder: "Order", value: checkpoints.length + 1 });
  const call = h("input", { placeholder: "Expected callsign" });
  sec.append(h("div", { class: "card" }, h("h2", {}, "Checkpoints"),
    h("p", { class: "muted" }, "Once this list exists, HQ only requests missing batches from listed checkpoints. Set the expected callsign so gap requests go to the right station."),
    h("table", {}, h("tbody", {}, ...checkpoints.map((c) => h("tr", {},
      h("td", { class: "mono" }, c.CourseOrder), h("td", { class: "mono" }, c.Code), h("td", {}, c.Name),
      h("td", { class: "mono" }, c.ExpectedCall || "—"),
      h("td", {}, h("button", { type: "button", class: "danger", onclick: async () => {
        if (!confirm(`Remove ${c.Code}? Its entries are kept.`)) return;
        await ctx.run(() => del(`/api/admin/checkpoints/${c.ID}`));
        await ctx.refresh();
      } }, "Remove")))))),
    h("div", { class: "row" }, order, code, name, call, h("button", { type: "button", onclick: async () => {
      const ok = await ctx.run(() => post("/api/admin/checkpoints", {
        code: code.value, name: name.value, course_order: Number(order.value), expected_call: call.value,
      }));
      if (ok) await ctx.refresh();
    } }, "Add")),
  ));

  const { runners } = await get("/api/admin/runners");
  sec.append(h("div", { class: "card" }, h("h2", {}, `Roster (${runners.length} runners)`),
    h("p", { class: "muted" }, "CSV with the bib in the first column. Only a column headed \"category\" is kept; names and other personal data are discarded."),
    upload("Import roster", "/api/admin/runners/import", ctx, (r) => `Roster loaded: ${r.runners} runners.`)));

  sec.append(h("div", { class: "card" }, h("h2", {}, "Recovery imports"),
    h("p", {}, "Checkpoint export (preferred: exact batches, safe to repeat):"),
    upload("Import checkpoint export", "/api/admin/recovery/import", ctx,
      (r) => `Imported ${r.Batches} batch(es): ${r.Entries} new, ${r.Duplicates} already held.`),
    h("p", {}, "Bib journal (when a checkpoint's database is lost):"),
    upload("Import journal", "/api/admin/recovery/journal", ctx,
      (r) => `Journal: ${r.Added} added, ${r.Voided} voided, ${r.Skipped} unreadable line(s), ${r.Deferred} deferred.`),
  ));
}
