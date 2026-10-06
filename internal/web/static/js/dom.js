// DOM helpers. Text is always set with textContent, never innerHTML, so
// race data (bibs, names, callsigns, branding text) can't inject markup.

export function $(sel, root = document) {
  return root.querySelector(sel);
}

// h builds an element: h("button", {class: "x", onclick: fn}, "text", child)
export function h(tag, attrs = {}, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k.startsWith("on") && typeof v === "function") el.addEventListener(k.slice(2), v);
    else if (k === "class") el.className = v;
    else if (k === "dataset") Object.assign(el.dataset, v);
    else el.setAttribute(k, v === true ? "" : String(v));
  }
  for (const c of children.flat()) {
    if (c === undefined || c === null || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}

export function clear(el) {
  while (el.firstChild) el.removeChild(el.firstChild);
  return el;
}

export function show(el, on = true) {
  el.classList.toggle("hidden", !on);
}

// poll runs fn every ms without overlapping runs (spec 8.2).
export function poll(fn, ms) {
  let stopped = false;
  let timer;
  const tick = async () => {
    if (stopped) return;
    try {
      await fn();
    } finally {
      if (!stopped) timer = setTimeout(tick, ms);
    }
  };
  tick();
  return () => {
    stopped = true;
    clearTimeout(timer);
  };
}

// banner shows (or clears with msg "") a message in a banner element.
export function banner(el, msg, kind = "info", ...extra) {
  clear(el);
  if (!msg) {
    show(el, false);
    return;
  }
  el.className = `banner ${kind}`;
  el.append(msg, ...extra);
  show(el, true);
}
