/* checkin-board: runs before UV Studio's scripts (VENDORED.md).
 * - Firmware-writing tools are removed: the node writes the radio's
 *   firmware slots itself (Admin → Station → Radio firmware slots), so a
 *   dropped Wi-Fi link can't interrupt a write, and the main firmware is
 *   never written from here. Removed before UV Studio reads its menu, so
 *   their routes don't exist at all.
 * - A link back to checkin-board. */
(function () {
  "use strict";
  const removed = ["flash", "firmware-slots", "apps", "external-flash", "factory-reset"];
  for (const route of removed) {
    document.querySelectorAll(`.nav-item[data-route="${route}"]`).forEach((el) => el.remove());
  }
  // A nav group left empty by the removals goes too.
  document.querySelectorAll(".nav-group").forEach((g) => {
    if (!g.querySelector(".nav-item")) g.remove();
  });
  if (removed.includes(decodeURIComponent(location.hash.slice(1)))) history.replaceState(null, "", "#live");

  const back = document.createElement("a");
  back.id = "cb-back";
  back.href = "/admin.html";
  back.textContent = "← checkin-board";
  back.title = "Back to this node's admin pages (closes UV Studio's hold on the radio)";
  document.body.prepend(back);
})();
