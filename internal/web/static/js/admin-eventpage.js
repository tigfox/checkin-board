// Event page editor (phase 12a): set by a race config or written here;
// the operator has the final say. Volunteers read it at /event.html.
import { get, put } from "./api.js";
import { h } from "./dom.js";
import { renderEventPage } from "./eventpage.js";

const MAX_KB = 64; // store.MaxEventPageBytes

export async function eventPageCard(ctx) {
  const page = await get("/api/eventpage");
  // No maxlength: it counts UTF-16 units, not bytes; the node checks the
  // real limit (MAX_BYTES) and says so.
  const text = h("textarea", { id: "event-text", rows: 10 }, page.text || "");
  const preview = h("div", { class: "card", id: "event-preview" }, ...renderEventPage(page.text));
  text.addEventListener("input", () => preview.replaceChildren(...renderEventPage(text.value)));
  return h("div", { class: "card" },
    h("h2", {}, "Event page"),
    h("p", { class: "muted" }, `Course notes, frequencies, contacts (up to ${MAX_KB} KB). Volunteers see it from the keypad (",
      h("a", { href: "/event.html" }, "Event info"), "). Headings start with #, list lines with -, tables with |, and **bold** works. It's shown as text: no links off the node.`),
    text,
    h("p", {}, h("button", { type: "button", class: "primary", id: "event-save", onclick: async () => {
      await ctx.run(() => put("/api/admin/eventpage", { text: text.value }), "Event page saved.");
    } }, "Save event page")),
    h("p", { class: "muted" }, "Preview:"), preview);
}
