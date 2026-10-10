// Race lifecycle (spec 4.7).
import { get, post } from "./api.js";
import { h } from "./dom.js";
import * as L from "./logic.js";
import { raceConfigCard } from "./admin-raceconfig.js";

export async function renderRace(sec, ctx) {
  const s = ctx.settings;
  sec.append(h("h1", {}, "Race"));
  sec.append(h("p", {}, "State: ", h("strong", {}, L.stateLabel(s.race_state, s.role)),
    s.race_started_at ? ` · started ${L.formatTime(s.race_started_at)}` : ""));
  if (!s.role) {
    sec.append(h("p", { class: "banner warn" }, "Choose this node's role on the Station tab first, or load a race config below."));
    sec.append(await raceConfigCard(ctx));
    return;
  }
  if (s.role === "checkpoint") {
    const st = await get("/api/station");
    sec.append(h("p", {}, L.deliverySummary(st, Date.now())));
    if (s.race_state === "checking_in") {
      sec.append(h("p", { class: "banner info" },
        `Final check-in: ${st.unconfirmed} entr${st.unconfirmed === 1 ? "y" : "ies"} still to confirm. `,
        "This page moves to Checked in by itself. If it can't finish by radio, download the export (Outbox tab) and import it at HQ."));
    }
  }
  const actions = L.actionsFor(s.role, s.race_state).filter((a) => a !== "reset");
  // Before Start: is the radio working? (It warns; it doesn't block.)
  // Filled in after the page renders: the check can take seconds when
  // graywolf is slow.
  if (actions.includes("start")) {
    const slot = h("p", { class: "muted", id: "radio-summary" }, "Radio: checking…");
    sec.append(slot);
    get("/api/admin/radio").then((radio) => {
      const sum = L.radioSummary(radio.report);
      if (sum) slot.replaceWith(h("p", { class: `banner ${sum.kind}`, id: "radio-summary" }, sum.text));
    }).catch(() => slot.replaceWith(h("p", { class: "banner warn", id: "radio-summary" }, "Radio: couldn't check (Station → Radio).")));
  }
  const row = h("div", { class: "row" });
  for (const a of actions) {
    const [label, question] = L.actionLabel(a, s.role);
    row.append(h("button", {
      type: "button", class: "primary",
      onclick: async () => {
        let ask = question;
        if (a === "start") {
          const [r, radio] = await Promise.all([
            get("/api/admin/linkcheck/readiness").catch(() => ({ warnings: [] })),
            get("/api/admin/radio").catch(() => null), // checked now, not when the page opened
          ]);
          const radioFailed = (radio?.report?.items || []).filter((i) => i.status === "fail").map((i) => `Radio: ${i.label}: ${i.detail}`);
          const warnings = [...radioFailed, ...r.warnings];
          if (warnings.length) ask = `${warnings.join("\n")}\n\n${question}`;
        }
        if (!confirm(ask)) return;
        await ctx.run(() => post(`/api/admin/race/${a}`), (r) =>
          a === "cleanup-graywolf" ? `graywolf cleanup: ${r.deleted} deleted, ${r.kept} kept (still needed), ${r.gone} already gone, ${r.failed} failed`
            : a === "secure" ? `Secured. ${r.unconfirmed} entries will go out at the final check-in.` : `${label}: done`);
        await ctx.reload();
        await ctx.refresh();
      },
    }, label));
  }
  sec.append(row);
  sec.append(await raceConfigCard(ctx));
  sec.append(resetCard(s, ctx));
}

function resetCard(s, ctx) {
  const name = s.race_name || "RESET";
  const confirmInput = h("input", { id: "reset-confirm", autocomplete: "off" });
  const ref = h("input", { type: "checkbox", id: "reset-ref" });
  const brand = h("input", { type: "checkbox", id: "reset-brand" });
  const unsent = h("input", { type: "checkbox", id: "reset-unsent" });
  return h("div", { class: "card" },
    h("h2", {}, "Reset this node"),
    h("p", {}, "Clears all race data so the node can be reused. A backup (database, export or results, journal) is written first."),
    s.role === "checkpoint" && s.race_state !== "checked_in"
      ? h("p", { class: "banner warn" }, "This checkpoint hasn't checked in. Entries HQ hasn't confirmed would only exist in the backup.") : null,
    h("label", { for: "reset-confirm" }, `Type "${name}" to confirm`), confirmInput,
    s.role === "hq" ? h("label", {}, ref, " Also clear the checkpoint list and roster") : null,
    s.role === "hq" ? h("label", {}, brand, " Also reset the board branding") : null,
    s.role === "checkpoint" ? h("label", {}, unsent, " Reset even if HQ hasn't confirmed everything") : null,
    h("p", {}, h("button", {
      type: "button", class: "danger",
      onclick: async () => {
        const r = await ctx.run(() => post("/api/admin/race/reset", {
          confirm: confirmInput.value, clear_reference: ref.checked, clear_branding: brand.checked,
          acknowledge_unsent: unsent.checked,
        }), (r) => `Reset done. Backup: ${r.backup_dir}` + (r.warnings && r.warnings.length ? ` · ${r.warnings.join(" · ")}` : ""));
        if (r) {
          await ctx.reload();
          await ctx.refresh();
        }
      },
    }, "Reset node")),
  );
}
