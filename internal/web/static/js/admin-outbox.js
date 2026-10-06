// Checkpoint outbox and export (spec 4.1, 4.4).
import { get } from "./api.js";
import { h } from "./dom.js";
import * as L from "./logic.js";

export async function renderOutbox(sec) {
  const o = await get("/api/admin/outbox");
  sec.append(h("h1", {}, "Outbox"));
  sec.append(h("p", {},
    `${o.stats.Queued} waiting to batch · ${o.stats.PendingBatches} batch(es) awaiting HQ · ${o.stats.RejectedBatches} parked`,
    o.last_hq_contact ? ` · last HQ contact ${L.formatAgo(Date.now() - new Date(o.last_hq_contact).getTime())}` : " · no HQ contact yet"));
  if (o.refusal) sec.append(h("p", { class: "banner bad" }, `graywolf refused a send: ${o.refusal.reason}. Check the path and channel settings.`));
  const rows = (o.pending || []).map((b) => h("tr", {},
    h("td", { class: "mono" }, b.seq), h("td", {}, b.state), h("td", {}, b.attempts),
    h("td", { class: "mono" }, b.msg_id || ""), h("td", { class: "mono" }, L.formatTime(b.next_tx_at)),
    h("td", { class: "mono" }, b.text)));
  sec.append(h("table", {},
    h("thead", {}, h("tr", {}, ...["Seq", "State", "Tries", "msgid", "Next try", "Text"].map((t) => h("th", {}, t)))),
    h("tbody", {}, ...rows)));
  sec.append(h("div", { class: "card" },
    h("h2", {}, "Export for HQ"),
    h("p", { class: "muted" }, "The offline path: carry this file to HQ's admin page (HQ tab → Import checkpoint export)."),
    h("a", { href: "/api/admin/recovery/export.csv", download: "checkpoint-export.csv" }, "Download checkpoint export"),
  ));
}
