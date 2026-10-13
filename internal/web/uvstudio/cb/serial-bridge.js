/* checkin-board: Web Serial for UV Studio, over this node's radio.
 *
 * UV Studio talks to a radio through navigator.serial, which reaches only
 * the browser's own USB ports. On a checkin-board node the UV-K5 is on
 * the Pi, so this replaces navigator.serial with one port, "this node's
 * radio", whose bytes travel over a WebSocket to the node
 * (/api/admin/uvstudio/ws). While UV Studio has the radio, checkin-board
 * holds race reports; closing the page (or 10 minutes without a click or
 * key) gives it back. Works over plain HTTP, in any modern browser. */
(function () {
  "use strict";

  const VENDOR = 0x36b7, PRODUCT = 0xffff; // the UV-K5's USB ids
  const ACTIVE_EVERY_MS = 30000;
  let raceConfirmed = false;

  function domError(name, message) {
    try { return new DOMException(message, name); } catch (_) { const e = new Error(message); e.name = name; return e; }
  }

  // banner shows what the bridge is doing at the top of the page.
  function banner(text, kind) {
    let el = document.getElementById("cb-bridge-banner");
    if (!el) {
      el = document.createElement("div");
      el.id = "cb-bridge-banner";
      el.setAttribute("role", "status");
      (document.body || document.documentElement).prepend(el);
    }
    el.textContent = text;
    el.className = kind || "";
  }

  // confirmRace asks before taking the radio during a race.
  async function confirmRace() {
    if (raceConfirmed) return true;
    try {
      const r = await fetch("/api/admin/settings", { credentials: "same-origin" });
      if (r.status === 401) { location.href = "/login.html"; return false; }
      const s = await r.json();
      if (s.race_state !== "active") return true;
    } catch (_) { return true; } // the server asks again if it must
    raceConfirmed = confirm("The race is running. While UV Studio has the radio, this node's race reports wait " +
      "(they go out when you close UV Studio). Continue?");
    return raceConfirmed;
  }

  class BridgedPort extends EventTarget {
    constructor() {
      super();
      this.readable = null;
      this.writable = null;
      this._ws = null;
    }

    getInfo() { return { usbVendorId: VENDOR, usbProductId: PRODUCT }; }

    async open() {
      if (this._ws) throw domError("InvalidStateError", "The port is already open.");
      if (!(await confirmRace())) throw domError("NotAllowedError", "Not taking the radio during the race.");
      const proto = location.protocol === "https:" ? "wss:" : "ws:";
      const ws = new WebSocket(`${proto}//${location.host}/api/admin/uvstudio/ws${raceConfirmed ? "?confirm=1" : ""}`);
      ws.binaryType = "arraybuffer";
      banner("Taking the radio from checkin-board…");
      let controller = null;
      const ready = new Promise((resolve, reject) => {
        ws.onmessage = (ev) => {
          if (typeof ev.data !== "string") { if (controller) controller.enqueue(new Uint8Array(ev.data)); return; }
          let m = {};
          try { m = JSON.parse(ev.data); } catch (_) { return; }
          if (m.type === "ready") {
            banner(`UV Studio has this node's radio (${m.port}). Race reports wait until you close this page.`, "busy");
            resolve();
          } else if (m.type === "error") {
            banner(m.message, "bad");
            reject(domError("NetworkError", m.message));
          } else if (m.type === "idle" || m.type === "closed") {
            banner(m.message ? `Radio given back to checkin-board: ${m.message}` : "Radio given back to checkin-board.", "");
          }
        };
        ws.onerror = () => reject(domError("NetworkError", "Can't reach this node's radio bridge."));
        ws.onclose = (ev) => {
          reject(domError("NetworkError", ev.reason || "The node refused the radio (is it in use, or not a UV-K5 node?)"));
          this._lost();
        };
      });
      this._ws = ws;
      try {
        await ready;
      } catch (e) {
        this._ws = null;
        throw e;
      }
      this.readable = new ReadableStream({
        start: (c) => { controller = c; },
        cancel: () => {},
      }, { highWaterMark: 64 });
      this._controller = () => controller;
      this.writable = new WritableStream({
        write: (chunk) => {
          if (ws.readyState !== WebSocket.OPEN) throw domError("NetworkError", "The radio bridge closed.");
          ws.send(chunk instanceof Uint8Array ? chunk : new Uint8Array(chunk));
        },
      });
      this._activity = setInterval(() => {
        if (this._busy && ws.readyState === WebSocket.OPEN) { ws.send(JSON.stringify({ type: "active" })); this._busy = false; }
      }, ACTIVE_EVERY_MS);
      this._onUse = () => { this._busy = true; };
      for (const t of ["pointerdown", "keydown"]) document.addEventListener(t, this._onUse, true);
    }

    // _lost: the bridge closed (page closed, radio unplugged, idle).
    _lost() {
      const ws = this._ws;
      if (!ws) return;
      this._ws = null;
      this._cleanup();
      const c = this._controller && this._controller();
      if (c) { try { c.error(domError("NetworkError", "The radio bridge closed.")); } catch (_) {} }
      serial.dispatchEvent(new Event("disconnect"));
    }

    _cleanup() {
      clearInterval(this._activity);
      if (this._onUse) for (const t of ["pointerdown", "keydown"]) document.removeEventListener(t, this._onUse, true);
    }

    async close() {
      const ws = this._ws;
      this._ws = null;
      this._cleanup();
      this.readable = null;
      this.writable = null;
      if (ws) {
        ws.onclose = null;
        ws.close(1000, "closed by UV Studio");
        banner("Radio given back to checkin-board.", "");
      }
    }

    async setSignals() {}
    async getSignals() { return { dataCarrierDetect: true, clearToSend: true, ringIndicator: false, dataSetReady: true }; }
    async forget() {}
  }

  const port = new BridgedPort();

  class BridgedSerial extends EventTarget {
    async requestPort() { return port; }
    async getPorts() { return [port]; }
    // UV Studio reads event.target.getInfo() on connect events.
    getInfo() { return port.getInfo(); }
  }
  const serial = new BridgedSerial();

  Object.defineProperty(navigator, "serial", { value: serial, configurable: true, enumerable: true });
  window.addEventListener("pagehide", () => { port.close(); });
})();
