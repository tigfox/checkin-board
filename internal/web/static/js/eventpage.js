// Event page rendering (phase 12a): the Markdown subset from
// logic.js markdownBlocks, built with text nodes only (no innerHTML), so
// a shared race file can't put markup or script on the node.
import { h } from "./dom.js";
import * as L from "./logic.js";

function inline(spans) {
  return spans.map((s) => (s.bold ? h("strong", {}, s.text) : s.text));
}

export function renderEventPage(text) {
  const blocks = L.markdownBlocks(text);
  if (!blocks.length) return [h("p", { class: "muted" }, "No event page yet.")];
  return blocks.map((b) => {
    switch (b.type) {
      case "heading":
        return h(`h${b.level + 1}`, {}, ...inline(b.spans));
      case "list":
        return h("ul", {}, ...b.items.map((it) => h("li", {}, ...inline(it))));
      case "table":
        return h("div", { class: "scroll" }, h("table", {},
          h("thead", {}, h("tr", {}, ...b.head.map((c) => h("th", {}, ...inline(c))))),
          h("tbody", {}, ...b.rows.map((r) => h("tr", {}, ...r.map((c) => h("td", {}, ...inline(c))))))));
      default:
        return h("p", {}, ...inline(b.spans));
    }
  });
}
