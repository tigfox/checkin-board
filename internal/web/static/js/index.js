// Sends the visitor to the page for their login, or to log in.
import { get } from "./api.js";

try {
  const s = await get("/api/session", { redirectOn401: false });
  location.replace(s.role === "admin" ? "/admin.html" : "/keypad.html");
} catch {
  location.replace("/login.html");
}
