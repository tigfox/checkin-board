import { get, post } from "./api.js";
import { $, show } from "./dom.js";

try {
  const status = await get("/api/setup", { redirectOn401: false });
  show($("#setup"), status.needs_setup);
  show($("#login"), !status.needs_setup);
  show($("#volunteer-note"), !status.needs_setup && !status.volunteer_set);
} catch (e) {
  // Show the login form anyway; logging in reports the real problem.
  show($("#login"), true);
  $("#login-error").textContent = e.message;
}

$("#setup-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  $("#setup-error").textContent = "";
  try {
    await post("/api/setup", { setup_code: $("#setup-code").value, password: $("#setup-pw").value });
    location.replace("/admin.html");
  } catch (e) {
    $("#setup-error").textContent = e.message;
  }
});

$("#login-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  $("#login-error").textContent = "";
  const role = document.querySelector('input[name="role"]:checked').value;
  try {
    await post("/api/login", { role, password: $("#login-pw").value }, { redirectOn401: false });
    location.replace(role === "admin" ? "/admin.html" : "/keypad.html");
  } catch (e) {
    $("#login-error").textContent = e.message;
    $("#login-pw").select();
  }
});
