// Small fetch wrapper for the app's JSON API. Errors carry the server's
// code and message; a 401 sends the user to the login page.

export class ApiError extends Error {
  constructor(status, body) {
    super((body && body.error) || `HTTP ${status}`);
    this.status = status;
    this.code = body && body.code;
    this.details = body && body.details;
  }
}

async function parse(resp) {
  const type = resp.headers.get("Content-Type") || "";
  if (resp.status === 204) return null;
  if (type.includes("application/json")) return resp.json();
  return resp.text();
}

export async function api(method, path, body, { redirectOn401 = true } = {}) {
  const init = { method, credentials: "same-origin", headers: {} };
  if (body !== undefined && body !== null) {
    if (body instanceof FormData) {
      init.body = body;
    } else {
      init.headers["Content-Type"] = "application/json";
      init.body = JSON.stringify(body);
    }
  }
  let resp;
  try {
    resp = await fetch(path, init);
  } catch (e) {
    const err = new ApiError(0, { error: "can't reach this node (check Wi-Fi)", code: "network" });
    err.network = true;
    throw err;
  }
  const data = await parse(resp);
  if (resp.ok) return data;
  if (resp.status === 401 && redirectOn401 && !location.pathname.endsWith("/login.html")) {
    location.href = "/login.html";
  }
  throw new ApiError(resp.status, typeof data === "object" ? data : { error: String(data) });
}

export const get = (p, o) => api("GET", p, null, o);
export const post = (p, b, o) => api("POST", p, b, o);
export const put = (p, b, o) => api("PUT", p, b, o);
export const del = (p, o) => api("DELETE", p, null, o);
