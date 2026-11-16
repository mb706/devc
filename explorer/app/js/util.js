// Small helpers every part uses: escaping, building DOM, storage that never
// throws, text helpers and number formatting. Everything of the app hangs
// off the one global X; each file adds one member.
var X = window.X = window.X || {};

X.util = (function () {
  "use strict";

  var ESC = { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" };
  function esc(s) {
    return s == null ? "" : String(s).replace(/[&<>"']/g, function (c) { return ESC[c]; });
  }

  // h("div.cls", {attr: v}, child, ...) builds an element. Children are
  // nodes, strings (as text) or arrays of those; null is skipped.
  function h(spec, attrs) {
    var m = /^([a-z0-9]+)?((?:\.[\w-]+)*)$/i.exec(spec) || [];
    var el = document.createElement(m[1] || "div");
    if (m[2]) el.className = m[2].slice(1).replace(/\./g, " ");
    var start = 1;
    if (attrs && typeof attrs === "object" && !(attrs instanceof Node) && !Array.isArray(attrs)) {
      start = 2;
      Object.keys(attrs).forEach(function (k) {
        var v = attrs[k];
        if (v == null || v === false) return;
        if (k === "html") el.innerHTML = v;
        else if (k === "text") el.textContent = v;
        else if (k.slice(0, 2) === "on") el.addEventListener(k.slice(2), v);
        else if (k === "class") el.className += (el.className ? " " : "") + v;
        else el.setAttribute(k, v === true ? "" : v);
      });
    }
    for (var i = start; i < arguments.length; i++) append(el, arguments[i]);
    return el;
  }
  function append(el, c) {
    if (c == null || c === false) return;
    if (Array.isArray(c)) { c.forEach(function (x) { append(el, x); }); return; }
    el.appendChild(c instanceof Node ? c : document.createTextNode(String(c)));
  }

  function icon(name, cls) {
    return '<svg class="ic' + (cls ? " " + cls : "") + '" aria-hidden="true"><use href="#i-' + name + '"/></svg>';
  }

  // Storage: the page must work when localStorage is missing or throws.
  function load(key, fallback) {
    try {
      var v = window.localStorage.getItem("explorer." + key);
      return v == null ? fallback : JSON.parse(v);
    } catch (e) { return fallback; }
  }
  function save(key, value) {
    try { window.localStorage.setItem("explorer." + key, JSON.stringify(value)); } catch (e) { /* unavailable */ }
  }

  // The first sentence of a comment or summary: up to the first period
  // followed by a space or the end, at most one paragraph.
  function firstSentence(text) {
    if (!text) return "";
    var p = firstParagraph(text).replace(/\s+/g, " ").trim();
    var m = /^(.*?[.!?])(\s|$)/.exec(p);
    var s = m ? m[1] : p;
    // "e.g." and similar abbreviations end too early; accept the longer cut.
    if (m && /\b(e\.g|i\.e|etc|vs|cf)\.$/.test(s)) {
      var rest = /^(.*?[.!?])(\s|$)/.exec(p.slice(s.length + 1));
      if (rest) s = s + " " + rest[1];
    }
    return s;
  }
  function firstParagraph(text) {
    if (!text) return "";
    var lines = String(text).split("\n"), out = [];
    for (var i = 0; i < lines.length; i++) {
      var l = lines[i];
      if (!l.trim()) { if (out.length) break; continue; }
      if (/^[\t ]/.test(l) && out.length) break; // a code block follows
      out.push(l.trim());
    }
    return out.join(" ");
  }

  function fmt(n) {
    if (n == null) return "";
    return String(Math.round(n)).replace(/\B(?=(\d{3})+(?!\d))/g, ",");
  }
  function plural(n, one, many) { return fmt(n) + " " + (n === 1 ? one : (many || one + "s")); }
  function pct(x) { return (x * 100 < 1 && x > 0) ? "<1%" : Math.round(x * 100) + "%"; }

  function debounce(fn, ms) {
    var t = null;
    return function () {
      var args = arguments, self = this;
      clearTimeout(t);
      t = setTimeout(function () { fn.apply(self, args); }, ms);
    };
  }

  // copy puts text on the clipboard; the fallback serves browsers that deny
  // the asynchronous API on file://.
  function copy(text) {
    function fallback() {
      var ta = h("textarea", { style: "position:fixed;left:-9999px;top:0" });
      ta.value = text;
      document.body.appendChild(ta);
      ta.select();
      try { document.execCommand("copy"); } catch (e) { /* nothing to do */ }
      ta.remove();
    }
    try {
      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(text).catch(fallback);
      } else fallback();
    } catch (e) { fallback(); }
    toast("Copied " + (text.length > 60 ? text.slice(0, 57) + "…" : text));
  }

  var toastTimer = null;
  function toast(msg) {
    var el = document.getElementById("toast");
    if (!el) { el = h("div"); el.id = "toast"; document.body.appendChild(el); }
    el.textContent = msg;
    el.classList.add("on");
    clearTimeout(toastTimer);
    toastTimer = setTimeout(function () { el.classList.remove("on"); }, 1600);
  }

  // lastElem("internal/box") is "box".
  function lastElem(path) { var i = path.lastIndexOf("/"); return i < 0 ? path : path.slice(i + 1); }
  function dirOf(path) { var i = path.lastIndexOf("/"); return i < 0 ? "" : path.slice(0, i); }

  // byKey sorts with a key function, stable for equal keys.
  function sortBy(arr, key) {
    return arr.map(function (x, i) { return [key(x), i, x]; })
      .sort(function (a, b) { return a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : a[1] - b[1]; })
      .map(function (t) { return t[2]; });
  }
  function groupBy(arr, key) {
    var m = new Map();
    arr.forEach(function (x) {
      var k = key(x);
      if (!m.has(k)) m.set(k, []);
      m.get(k).push(x);
    });
    return m;
  }

  // isModifiedClick: a click that should open in full rather than inspect.
  function isModifiedClick(ev) { return ev.ctrlKey || ev.metaKey; }

  return {
    esc: esc, h: h, icon: icon, load: load, save: save,
    firstSentence: firstSentence, firstParagraph: firstParagraph,
    fmt: fmt, plural: plural, pct: pct, debounce: debounce, copy: copy, toast: toast,
    lastElem: lastElem, dirOf: dirOf, sortBy: sortBy, groupBy: groupBy, isModifiedClick: isModifiedClick
  };
})();
