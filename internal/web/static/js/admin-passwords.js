// Passwords (spec 7.2).
import { put } from "./api.js";
import { h } from "./dom.js";

export async function renderPasswords(sec, ctx) {
  sec.append(h("h1", {}, "Passwords"));
  const vol = h("input", { type: "password", id: "vol-new", autocomplete: "new-password", minlength: 6 });
  sec.append(h("div", { class: "card" },
    h("h2", {}, "Volunteer password (keypad)"),
    h("p", { class: "muted" }, "Shared by everyone on the keypad. Changing it logs all volunteers out."),
    h("label", { for: "vol-new" }, "New volunteer password (6+ characters)"), vol,
    h("p", {}, h("button", { type: "button", class: "primary", onclick: async () => {
      const ok = await ctx.run(() => put("/api/admin/password/volunteer", { new: vol.value }), "Volunteer password set.");
      if (ok !== undefined) vol.value = "";
    } }, "Set volunteer password"))));
  const cur = h("input", { type: "password", id: "adm-cur", autocomplete: "current-password" });
  const next = h("input", { type: "password", id: "adm-new", autocomplete: "new-password", minlength: 10 });
  sec.append(h("div", { class: "card" },
    h("h2", {}, "Admin password"),
    h("p", { class: "muted" }, "Changing it logs every admin out, including you."),
    h("label", { for: "adm-cur" }, "Current admin password"), cur,
    h("label", { for: "adm-new" }, "New admin password (10+ characters)"), next,
    h("p", {}, h("button", { type: "button", class: "primary", onclick: async () => {
      const ok = await ctx.run(() => put("/api/admin/password/admin", { current: cur.value, new: next.value }));
      if (ok !== undefined) location.href = "/login.html";
    } }, "Change admin password"))));
}
