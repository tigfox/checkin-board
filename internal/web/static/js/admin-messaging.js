// Messaging settings: how the node uses the radio (spec 8.1). Shown on
// the Station tab under the race settings.
import { put } from "./api.js";
import { field, h } from "./dom.js";

export function messagingSettingsForm(s, ctx) {
  const num = (id, label, value, hint) => h("div", {}, ...field(id, label, value, { type: "number" }, hint));
  const form = h("form", { class: "card", id: "messaging-settings" },
    h("h2", {}, "Messaging settings"),
    h("p", { class: "muted" }, "Radio tunables. The defaults suit most races; change them only after a link check shows a reason to."),
    h("div", { class: "grid2" },
      h("div", {}, ...field("path", "Digipeater path", s.path, { autocapitalize: "characters" },
        "Blank = direct. e.g. WIDE1-1 or a digipeater's callsign.")),
      h("div", {}, ...field("gw_channel", "graywolf channel", s.gw_channel, { type: "number", min: 0 },
        "0 = graywolf's default transmit channel.")),
      num("heartbeat_sec", "Heartbeat every (s)", s.heartbeat_sec, "A checkpoint tells HQ it's alive this often when idle. 60-3600."),
      num("flush_after_sec", "Batch after (s)", s.flush_after_sec, "How long to collect bibs before sending a report. 5-300."),
      num("max_in_flight", "Batches in flight", s.max_in_flight, "Reports sent before waiting for HQ's acknowledgements. 1-8."),
      num("max_text_len", "Max message length", s.max_text_len, "Characters per radio message. 67-200; over 67 needs graywolf long messages."),
      num("gap_grace_sec", "Gap grace (s, HQ)", s.gap_grace_sec, "How long HQ waits on a missing report before asking for it again. 30-900."),
    ),
    h("p", {}, h("button", { type: "submit", class: "primary" }, "Save messaging settings")),
  );
  form.addEventListener("submit", async (ev) => {
    ev.preventDefault();
    const n = (id) => Number(form.querySelector(`#${id}`).value);
    const body = {
      path: form.querySelector("#path").value, gw_channel: n("gw_channel"), max_text_len: n("max_text_len"),
      flush_after_sec: n("flush_after_sec"), max_in_flight: n("max_in_flight"),
      heartbeat_sec: n("heartbeat_sec"), gap_grace_sec: n("gap_grace_sec"),
    };
    if (await ctx.run(() => put("/api/admin/settings/messaging", body), "Messaging settings saved.")) await ctx.reload();
  });
  return form;
}
