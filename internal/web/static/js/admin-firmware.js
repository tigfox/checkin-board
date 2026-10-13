// UV-K5 extras on the Station page: UV Studio (the radio's screen,
// keypad and RF log, through this node) and the radio's firmware,
// flashed by the node itself from files kept on it.
import { ApiError, del, get, post } from "./api.js";
import { h } from "./dom.js";
import * as L from "./logic.js";

// POLL_MS is how often a running flash is read.
const POLL_MS = 1000;

export function uvStudioCard() {
  return h("div", { class: "card", id: "uvstudio-card" },
    h("h2", {}, "UV Studio"),
    h("p", { class: "muted" }, "The radio's live screen and keypad, its RF log (with CSV export), and calibration and boot-logo backup, from this browser. ",
      "UV Studio has the radio while it's open: race reports wait, and go out when you close it (or after 10 minutes without a click)."),
    h("p", {}, h("a", { href: "/uvstudio/", target: "_blank", rel: "noopener", id: "uvstudio-open" }, "Open UV Studio")),
    h("p", { class: "muted" }, "UV Studio is by Armel FAUVEAU (Apache License 2.0). Its firmware tools are left out: flash from the Firmware card below."));
}

export async function firmwareCard(ctx) {
  const card = h("div", { class: "card", id: "firmware-card" });
  let timer = null;
  const render = async () => {
    let view;
    try {
      view = await get("/api/admin/firmware");
    } catch (e) {
      card.replaceChildren(h("h2", {}, "Radio firmware"), h("p", { class: "error" }, e.message));
      return;
    }
    const running = view.job.state === "waiting" || view.job.state === "flashing";
    card.replaceChildren(
      h("h2", {}, "Radio firmware"),
      h("p", { class: "muted" }, "Firmware files on this node, flashed into the radio by the node itself: the browser can be closed once flashing starts. ",
        "Files can also be copied into /var/lib/checkin-board/firmware/ or added with install.sh --firmware."),
      jobPanel(view.job, ctx, render),
      view.files.length
        ? h("div", { class: "scroll" }, h("table", { id: "firmware-files" }, h("tbody", {}, ...view.files.map((f) => fileRow(f, running, ctx, render)))))
        : h("p", { class: "muted" }, "No firmware files on this node yet."),
      uploadRow(ctx, render),
    );
    clearTimeout(timer);
    if (running && card.isConnected !== false) timer = setTimeout(render, POLL_MS);
  };
  await render();
  return card;
}

function fileRow(f, running, ctx, render) {
  return h("tr", {},
    h("td", { class: "mono" }, f.name),
    h("td", {}, `${Math.round(f.size / 1024)} KB`),
    h("td", { class: "mono muted", title: f.sha256 }, f.sha256 ? f.sha256.slice(0, 12) : f.error || ""),
    h("td", {}, h("button", { type: "button", class: "primary", disabled: running || !!f.error, dataset: { flash: f.name }, onclick: async () => {
      if (!confirm(L.flashConfirm(f.name))) return;
      const start = (confirmRace) => post(`/api/admin/firmware/${encodeURIComponent(f.name)}/flash`, { confirm: confirmRace });
      try {
        await start(false);
      } catch (e) {
        if (!(e instanceof ApiError) || e.code !== "confirm_required") {
          ctx.notify(e.message, "bad");
          return;
        }
        if (!confirm(e.message.replace(/; confirm to continue$/, ". Flash anyway?"))) return;
        if (!(await ctx.run(() => start(true)))) return;
      }
      await render();
    } }, "Flash")),
    h("td", {}, h("button", { type: "button", class: "danger", disabled: running, onclick: async () => {
      if (!confirm(`Delete ${f.name} from this node?`)) return;
      if (await ctx.run(() => del(`/api/admin/firmware/${encodeURIComponent(f.name)}`), "Deleted.")) await render();
    } }, "Delete")));
}

function jobPanel(job, ctx, render) {
  const s = L.flashJobSummary(job);
  if (!s) return null;
  return h("div", { id: "flash-job" },
    h("p", { class: `banner ${s.kind}` }, s.text),
    job.state === "flashing" ? h("progress", { max: job.pages, value: job.page }) : null,
    job.state === "waiting"
      ? h("p", {}, h("button", { type: "button", onclick: async () => {
        if (await ctx.run(() => post("/api/admin/firmware-flash/cancel"), "Cancelled: the radio is back with checkin-board.")) await render();
      } }, "Cancel"))
      : null);
}

function uploadRow(ctx, render) {
  const file = h("input", { type: "file", accept: ".bin", id: "firmware-file" });
  return h("div", { class: "row" }, file, h("button", { type: "button", onclick: async () => {
    if (!file.files.length) return;
    const send = (replace) => {
      const fd = new FormData();
      fd.append("file", file.files[0]);
      if (replace) fd.append("replace", "1");
      return post("/api/admin/firmware", fd);
    };
    let r;
    try {
      r = await send(false);
    } catch (e) {
      if (e.code !== "exists") {
        ctx.notify(e.message, "bad");
        return;
      }
      if (!confirm("A firmware file with that name is already on this node. Replace it?")) return;
      r = await ctx.run(() => send(true));
    }
    if (r) {
      ctx.notify(`Saved ${r.name}.`);
      await render();
    }
  } }, "Upload"));
}
