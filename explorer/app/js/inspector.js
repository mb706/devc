// The inspector: a panel on the right that shows one reference without
// leaving the current view. It has its own back and forward stack, can be
// resized and closed, and its target is the URL parameter "i" (replaced,
// not pushed, so the browser's history stays with the main view).
//
//   X.inspector.open(ref)              show ref, pushed on the stack
//   X.inspector.close()
//   X.inspector.register(kind, fn)     fn(info, body, ctx) renders a kind
//                                      into body; it may return a cleanup
//                                      function. ctx.open(ref) navigates
//                                      the inspector.
//   X.inspector.current()              the shown reference or ""
//   X.inspector.sync(ref)              (the router) follow the URL
X.inspector = (function () {
  "use strict";
  var U = X.util, esc = U.esc;
  var root = document.getElementById("inspector");
  var stack = [], pos = -1, cleanup = null, renderers = {}, built = false;
  var head, body, btnBack, btnFwd, btnFull, titleEl;

  function build() {
    if (built) return;
    built = true;
    root.innerHTML =
      '<div class="insp-resize" title="Drag to resize"></div>' +
      '<div class="insp-head">' +
      '<button class="btn ghost icon" data-a="back" title="Back (Alt+←)">' + U.icon("back") + "</button>" +
      '<button class="btn ghost icon" data-a="fwd" title="Forward">' + U.icon("fwd") + "</button>" +
      '<div class="insp-title"></div>' +
      '<button class="btn ghost" data-a="full" title="Open in full (or Ctrl+click any reference)">' + U.icon("full") + "<span>Open</span></button>" +
      '<button class="btn ghost icon" data-a="close" title="Close (Esc)">' + U.icon("close") + "</button>" +
      "</div>" +
      '<div class="insp-body"></div>';
    head = root.querySelector(".insp-head");
    body = root.querySelector(".insp-body");
    btnBack = head.querySelector('[data-a="back"]');
    btnFwd = head.querySelector('[data-a="fwd"]');
    btnFull = head.querySelector('[data-a="full"]');
    titleEl = head.querySelector(".insp-title");
    head.addEventListener("click", function (ev) {
      var b = ev.target.closest("[data-a]");
      if (!b) return;
      var a = b.getAttribute("data-a");
      if (a === "back") back();
      else if (a === "fwd") forward();
      else if (a === "close") close();
      else if (a === "full") openFull();
    });
    setupResize(root.querySelector(".insp-resize"));
    applyWidth(X.settings.get("inspectorWidth"));
  }

  function setupResize(handle) {
    handle.addEventListener("pointerdown", function (ev) {
      ev.preventDefault();
      handle.setPointerCapture(ev.pointerId);
      root.classList.add("resizing");
      function move(e) { applyWidth(window.innerWidth - e.clientX); }
      function up() {
        handle.removeEventListener("pointermove", move);
        handle.removeEventListener("pointerup", up);
        root.classList.remove("resizing");
        X.settings.set("inspectorWidth", root.offsetWidth);
      }
      handle.addEventListener("pointermove", move);
      handle.addEventListener("pointerup", up);
    });
  }
  function applyWidth(w) {
    var max = Math.max(360, Math.round(window.innerWidth * 0.72));
    w = Math.max(340, Math.min(max, w || 520));
    root.style.width = w + "px";
  }
  window.addEventListener("resize", function () { if (built) applyWidth(root.offsetWidth); });

  function current() { return pos >= 0 ? stack[pos] : ""; }

  function open(ref) {
    if (!ref) return;
    if (ref === current() && !root.classList.contains("hidden")) return;
    stack = stack.slice(0, pos + 1);
    stack.push(ref);
    pos = stack.length - 1;
    show();
  }
  function back() { if (pos > 0) { pos--; show(); } }
  function forward() { if (pos < stack.length - 1) { pos++; show(); } }
  function close() {
    stack = []; pos = -1;
    if (cleanup) { try { cleanup(); } catch (e) { /* ignore */ } cleanup = null; }
    root.classList.add("hidden");
    document.body.classList.remove("insp-open");
    if (X.router.current && X.router.current.params.i) X.router.setParams({ i: null });
    window.dispatchEvent(new Event("resize"));
  }
  function openFull() {
    var h = X.refs.href(current());
    if (h) X.router.go(h);
  }

  // sync follows the URL: a new target is pushed, the previous one is
  // treated as "back".
  function sync(ref) {
    if (!ref) { if (pos >= 0) close(); return; }
    if (ref === current()) return;
    if (pos > 0 && stack[pos - 1] === ref) { pos--; show(true); return; }
    stack = stack.slice(0, pos + 1);
    stack.push(ref);
    pos = stack.length - 1;
    show(true);
  }

  function show(fromURL) {
    build();
    var ref = current();
    var wasHidden = root.classList.contains("hidden");
    root.classList.remove("hidden");
    document.body.classList.add("insp-open");
    btnBack.disabled = pos <= 0;
    btnFwd.disabled = pos >= stack.length - 1;
    var info = X.refs.resolve(ref);
    btnFull.disabled = !X.refs.href(ref);
    titleEl.innerHTML = X.refs.chip(ref, { cls: "title" }) +
      '<button class="btn ghost icon" data-copy title="Copy the reference">' + U.icon("copy") + "</button>";
    titleEl.querySelector("[data-copy]").onclick = function () { U.copy(ref); };
    if (cleanup) { try { cleanup(); } catch (e) { /* ignore */ } cleanup = null; }
    body.innerHTML = "";
    body.scrollTop = 0;
    var fn = renderers[info.kind] || defaultRenderer;
    try {
      if (info.missing) defaultRenderer(info, body, ctx);
      else cleanup = fn(info, body, ctx) || null;
    } catch (e) {
      console.error(e);
      body.innerHTML = '<div class="error">' + esc(e.message) + "</div>";
    }
    if (!fromURL && X.router.current && X.router.current.params.i !== ref) X.router.setParams({ i: ref });
    if (wasHidden) window.dispatchEvent(new Event("resize"));
  }

  function defaultRenderer(info, el) {
    el.innerHTML = '<div class="insp-pad"><div class="insp-card">' + X.refs.card(info.ref) + "</div>" +
      (info.missing ? "" : (X.refs.href(info.ref) ? '<p class="muted small">Open it in full for more.</p>' : "")) + "</div>";
  }

  var ctx = { open: open, back: back, close: close, current: current };

  function register(kind, fn) { renderers[kind] = fn; }

  // Clicks on references inside the inspector navigate the inspector;
  // the shared handler in refs.js does that already (inspect = open).
  document.addEventListener("keydown", function (ev) {
    if (root.classList.contains("hidden")) return;
    var t = ev.target;
    var typing = t && (t.tagName === "INPUT" || t.tagName === "TEXTAREA" || t.isContentEditable);
    if (ev.key === "Escape" && !typing && !document.querySelector(".modal.on")) close();
    if (ev.altKey && ev.key === "ArrowLeft" && !typing && root.contains(document.activeElement)) { ev.preventDefault(); back(); }
  });

  return { open: open, close: close, back: back, forward: forward, sync: sync, register: register, current: current, root: root };
})();
