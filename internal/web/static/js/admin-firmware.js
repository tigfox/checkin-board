// UV-K5 extras on the Station page: UV Studio (the radio's screen,
// keypad and RF log, through this node) and the radio's firmware slots,
// written by the node itself from files kept on it.
import { ApiError, del, get, post } from "./api.js";
import { h } from "./dom.js";
import * as L from "./logic.js";

// POLL_MS is how often a running slot write is read.
const POLL_MS = 1000;

export function uvStudioCard() {
  return h("div", { class: "card", id: "uvstudio-card" },
    h("h2", {}, "UV Studio"),
    h("p", { class: "muted" }, "The radio's live screen and keypad, its RF log (with CSV export), and calibration and boot-logo backup, from this browser. ",
      "UV Studio has the radio while it's open: race reports wait, and go out when you close it (or after 10 minutes without a click)."),
    h("p", {}, h("a", { href: "/uvstudio/", target: "_blank", rel: "noopener", id: "uvstudio-open" }, "Open UV Studio")),
    h("p", { class: "muted" }, "UV Studio is by Armel FAUVEAU (Apache License 2.0). Its firmware tools are left out: the node writes firmware slots from the card below."));
}

// SLOTS are the radio's writable firmware slots (slot 0 is the radio's
// own backup of its main firmware: never written).
const SLOTS = [1, 2, 3, 4];

export async function firmwareCard(ctx) {
  const card = h("div", { class: "card", id: "firmware-card" });
  let timer = null;
  const render = async () => {
    let view;
    try {
      view = await get("/api/admin/firmware");
    } catch (e) {
      card.replaceChildren(h("h2", {}, "Radio firmware slots"), h("p", { class: "error" }, e.message));
      return;
    }
    const busy = view.job.state === "writing" || view.job.state === "reading";
    card.replaceChildren(
      h("h2", {}, "Radio firmware slots"),
      h("p", { class: "muted" }, "The radio keeps other firmware (the APRS one, an FM/voice one) in slots 1-4 of its external memory, and switches to one from its menu. ",
        "This node writes firmware files into those slots with the radio switched on as usual. The radio's main firmware is never written. Once a write starts, this page can be closed."),
      jobPanel(view.job, ctx, render),
      slotsTable(view.job, busy, ctx, render),
      h("h3", {}, "Firmware files on this node"),
      view.files.length
        ? h("div", { class: "scroll" }, h("table", { id: "firmware-files" }, h("tbody", {}, ...view.files.map((f) => fileRow(f, view.job, busy, ctx, render)))))
        : h("p", { class: "muted" }, "No firmware files on this node yet. Upload one, copy it into /var/lib/checkin-board/firmware/, or add it with install.sh --firmware."),
      uploadRow(ctx, render),
    );
    clearTimeout(timer);
    if (view.job.state === "writing" && card.isConnected !== false) timer = setTimeout(render, POLL_MS);
  };
  await render();
  return card;
}

// withRaceConfirm runs call(confirm), asking first if the race is on
// (the node asks too, should the page's settings be stale).
async function withRaceConfirm(ctx, call) {
  if (ctx.settings?.race_state === "active") {
    if (!confirm("The race is running: this node's race reports wait while it has the radio (about a minute for a write). Go ahead?")) return null;
    return ctx.run(() => call(true));
  }
  try {
    return await call(false);
  } catch (e) {
    if (!(e instanceof ApiError) || e.code !== "confirm_required") {
      ctx.notify(e.message, "bad");
      return null;
    }
    if (!confirm(e.message.replace(/; confirm to continue$/, ". Go ahead?"))) return null;
    return ctx.run(() => call(true));
  }
}

function slotsTable(job, busy, ctx, render) {
  const read = h("button", { type: "button", id: "slots-read", disabled: busy, onclick: async () => {
    read.disabled = true;
    read.textContent = "Reading…";
    await withRaceConfirm(ctx, (c) => post("/api/admin/firmware-slots/read", { confirm: c }));
    await render();
  } }, job.slots ? "Read again" : "Read slots");
  return h("div", { id: "firmware-slots" },
    h("h3", {}, "Slots"),
    job.slots
      ? h("table", {}, h("tbody", {},
        h("tr", {}, h("th", {}, "Slot"), h("th", {}, "Firmware"), h("th", {}, "Version"), h("th", {}, "Size"), h("th", {}, "State")),
        ...job.slots.map((sl) => h("tr", { dataset: { slot: sl.slot } },
          h("td", {}, String(sl.slot)), h("td", {}, sl.name || "—"), h("td", {}, sl.version || ""),
          h("td", {}, sl.size ? `${Math.round(sl.size / 1024)} KB` : ""), h("td", {}, sl.state)))))
      : h("p", { class: "muted" }, "Not read yet. Reading takes the radio for a second or two."),
    h("p", {}, read));
}

function fileRow(f, job, busy, ctx, render) {
  const slot = h("select", { "aria-label": `Slot for ${f.name}` }, ...SLOTS.map((n) => h("option", { value: n }, `Slot ${n}`)));
  const name = h("input", { value: f.slot_name || "", maxlength: 15, placeholder: "name on the radio", "aria-label": "Name on the radio" });
  const version = h("input", { value: f.slot_version || "", maxlength: 15, placeholder: "version", "aria-label": "Version", size: 8 });
  return h("tr", {},
    h("td", { class: "mono" }, f.name, h("br"), h("span", { class: "muted", title: f.sha256 }, `${Math.round(f.size / 1024)} KB · ${f.sha256 ? f.sha256.slice(0, 12) : f.error || ""}`)),
    h("td", {}, slot), h("td", {}, name), h("td", {}, version),
    h("td", {}, h("button", { type: "button", class: "primary", disabled: busy || !!f.error, dataset: { write: f.name }, onclick: async () => {
      const n = Number(slot.value);
      const current = (job.slots || []).find((s) => s.slot === n);
      if (!name.value.trim()) {
        ctx.notify("Give the slot a name: it shows on the radio.", "bad");
        return;
      }
      if (!confirm(L.slotWriteConfirm(f.name, n, name.value.trim(), current))) return;
      const r = await withRaceConfirm(ctx, (c) => post(`/api/admin/firmware/${encodeURIComponent(f.name)}/write`,
        { slot: n, name: name.value, version: version.value, confirm: c }));
      if (r) await render();
    } }, "Write")),
    h("td", {}, h("button", { type: "button", class: "danger", disabled: busy, onclick: async () => {
      if (!confirm(`Delete ${f.name} from this node?`)) return;
      if (await ctx.run(() => del(`/api/admin/firmware/${encodeURIComponent(f.name)}`), "Deleted.")) await render();
    } }, "Delete")));
}

function jobPanel(job, ctx, render) {
  const s = L.slotJobSummary(job);
  if (!s) return null;
  return h("div", { id: "slot-job" },
    h("p", { class: `banner ${s.kind}` }, s.text),
    job.state === "writing" && job.total ? h("progress", { max: job.total, value: job.done }) : null,
    job.state === "writing"
      ? h("p", {}, h("button", { type: "button", onclick: async () => {
        if (await ctx.run(() => post("/api/admin/firmware-slots/cancel"), "Cancelled.")) await render();
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
