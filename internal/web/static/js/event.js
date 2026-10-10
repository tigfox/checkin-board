// Event page view (phase 12a): the race's notes, frequencies and contacts,
// for volunteers and admins.
import { get } from "./api.js";
import { $, h } from "./dom.js";
import { renderEventPage } from "./eventpage.js";

try {
  const page = await get("/api/eventpage");
  $("#event").replaceChildren(...renderEventPage(page.text));
} catch (e) {
  $("#event").replaceChildren(h("p", { class: "error" }, e.message));
}
