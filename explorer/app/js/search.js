// Search: "/" or Ctrl+K. Two modes:
//  - Names: symbols, files, packages, commands, RPC methods, routes, event
//    types, configuration keys, environment variables, and whatever later
//    parts register (X3: concerns, flows, terms, document headings),
//    grouped by kind. Ranking: exact, prefix, word start, substring,
//    subsequence; exported and shorter names first.
//  - Text in code: a regular expression or plain text over every source
//    file; loads all per-package data first, with progress.
//
//   X.search.open(query, mode)       mode "names" | "text"
//   X.search.addSource({kind, title, order, items() -> [{ref, name,
//     keys: [string], detail, exported, weight}]})
//   X.search.query(q) -> [{source, item, score}]   (used by the check)
X.search = (function () {
  "use strict";
  var U = X.util, D = X.data, R = X.refs, esc = U.esc;
  var sources = [], built = false, modal = null, input, list, status, mode = "names", sel = 0, flat = [];
  var expanded = {};

  function addSource(s) { sources.push(s); sources.sort(function (a, b) { return (a.order || 0) - (b.order || 0); }); built = false; }

  // ----- the built-in sources

  addSource({
    kind: "package", title: "Packages", order: 10,
    items: function () {
      return D.core.packages.map(function (p) {
        return { ref: "pkg:" + p.path, name: p.path, keys: [U.lastElem(p.path), p.path, p.name], detail: U.firstSentence(p.role || p.doc), exported: true, weight: 40 };
      });
    }
  });
  addSource({
    kind: "symbol", title: "Symbols", order: 20,
    items: function () {
      var showTests = X.settings.get("tests");
      return D.core.symbols.filter(function (s) { return showTests || !s.test; }).map(function (s) {
        var q = s.ref.slice(s.package.length + 1);
        var w = s.kind === "field" ? -15 : s.kind === "type" ? 10 : s.kind === "func" || s.kind === "method" ? 5 : 0;
        if (s.test) w -= 40;
        return {
          ref: s.ref, name: q, keys: [s.name, q, U.lastElem(s.package) + "." + q, s.ref],
          detail: s.package, exported: s.exported, weight: w
        };
      });
    }
  });
  addSource({
    kind: "file", title: "Files", order: 30,
    items: function () {
      var showTests = X.settings.get("tests");
      return D.core.files.filter(function (f) { return showTests || !f.test; }).map(function (f) {
        return { ref: "file:" + f.path, name: f.path, keys: [U.lastElem(f.path), f.path], detail: U.firstSentence(f.doc), exported: !f.test, weight: 0 };
      });
    }
  });
  addSource({
    kind: "command", title: "Commands", order: 40,
    items: function () {
      return D.core.commands.map(function (c) {
        var tail = c.path.split(" ").slice(1).join(" ");
        return { ref: "cmd:" + c.path, name: c.path, keys: [tail || c.path, c.path, U.lastElem(c.path.replace(/ /g, "/"))], detail: c.short, exported: !c.hidden, weight: 20 };
      });
    }
  });
  addSource({
    kind: "rpc", title: "RPC methods", order: 50,
    items: function () { return D.core.rpc.map(function (m) { return { ref: "rpc:" + m.name, name: m.name, keys: [m.name, m.const], detail: U.firstSentence(m.doc), exported: true, weight: 20 }; }); }
  });
  addSource({
    kind: "route", title: "Routes", order: 60,
    items: function () { return D.core.routes.map(function (r) { return { ref: "http:" + r.key, name: r.key, keys: [r.path, r.key], detail: r.handler || "", exported: true, weight: 10 }; }); }
  });
  addSource({
    kind: "event", title: "Event types", order: 70,
    items: function () { return D.core.events.map(function (e) { return { ref: "event:" + e.name, name: e.name, keys: [e.name, e.const], detail: U.firstSentence(e.doc), exported: true, weight: 10 }; }); }
  });
  addSource({
    kind: "config", title: "Configuration keys", order: 80,
    items: function () {
      return D.core.config.map(function (c) {
        var last = c.key.split(".").pop();
        return { ref: "config:" + c.key, name: c.key, keys: [last, c.key], detail: U.firstSentence(c.doc), exported: true, weight: 0 };
      });
    }
  });
  addSource({
    kind: "env", title: "Environment variables", order: 90,
    items: function () { return D.core.env.map(function (e) { return { ref: "env:" + e.name, name: e.name, keys: [e.name], detail: U.plural((e.uses || []).length, "use"), exported: true, weight: 0 }; }); }
  });

  // ----- ranking

  var prepared = [];
  function build() {
    prepared = sources.map(function (s) {
      var items;
      try { items = s.items() || []; } catch (e) { console.error(e); items = []; }
      return {
        source: s, items: items.map(function (it) {
          return { it: it, keys: (it.keys || [it.name]).filter(Boolean).map(function (k) { return { o: k, l: k.toLowerCase() }; }) };
        })
      };
    });
    built = true;
  }
  function wordStart(o, i) {
    if (i === 0) return true;
    var p = o[i - 1], c = o[i];
    if ("./_- :~#".indexOf(p) >= 0) return true;
    return c >= "A" && c <= "Z" && p >= "a" && p <= "z";
  }
  function subseq(l, q) {
    var j = 0;
    for (var i = 0; i < l.length && j < q.length; i++) if (l[i] === q[j]) j++;
    return j === q.length;
  }
  function level(k, q) {
    if (k.l === q) return 5;
    if (k.l.lastIndexOf(q, 0) === 0) return 4;
    var i = k.l.indexOf(q);
    if (i >= 0) {
      while (i >= 0) { if (wordStart(k.o, i)) return 3; i = k.l.indexOf(q, i + 1); }
      return 2;
    }
    return q.length > 1 && subseq(k.l, q) ? 1 : 0;
  }
  function query(q) {
    if (!built) build();
    q = q.trim().toLowerCase();
    if (!q) return [];
    var toks = q.split(/\s+/);
    var out = [];
    prepared.forEach(function (ps) {
      ps.items.forEach(function (p) {
        var best = 0, bestKey = 0;
        if (toks.length === 1) {
          for (var i = 0; i < p.keys.length; i++) {
            var lv = level(p.keys[i], q);
            if (lv > best) { best = lv; bestKey = i; }
            if (best === 5) break;
          }
        } else {
          // Several words: each must occur in some key; the whole query
          // also counts as one string (commands have spaces).
          var whole = 0;
          p.keys.forEach(function (k) { whole = Math.max(whole, level(k, q)); });
          var all = toks.every(function (t) { return p.keys.some(function (k) { return k.l.indexOf(t) >= 0; }); });
          best = whole || (all ? 2 : 0);
        }
        if (!best) return;
        var len = p.it.name ? p.it.name.length : 0;
        var score = best * 1000 - bestKey * 60 + (p.it.exported ? 80 : 0) + (p.it.weight || 0) - Math.min(len, 120);
        out.push({ source: ps.source, item: p.it, score: score, level: best });
      });
    });
    out.sort(function (a, b) { return b.score - a.score; });
    return out;
  }

  // ----- the dialog

  function ensure() {
    if (modal) return;
    modal = U.h("div.modal.search");
    modal.innerHTML =
      '<div class="modal-box">' +
      '<div class="sr-top">' + U.icon("search") +
      '<input type="text" class="sr-input" spellcheck="false" autocomplete="off" placeholder="Search names…">' +
      '<div class="seg sr-modes"><button data-m="names" class="on" title="Names (Tab switches)">Names</button><button data-m="text" title="Text in code (Tab switches)">Text in code</button></div>' +
      "</div>" +
      '<div class="sr-opts hidden"><label class="check"><input type="checkbox" data-o="regex"> regular expression</label>' +
      '<label class="check"><input type="checkbox" data-o="case"> match case</label><span class="sr-status muted small"></span></div>' +
      '<div class="sr-list"></div>' +
      '<div class="sr-foot muted small"><span><kbd>↑</kbd><kbd>↓</kbd> choose</span><span><kbd>Enter</kbd> open</span><span><kbd>Shift</kbd>+<kbd>Enter</kbd> inspect</span><span><kbd>Tab</kbd> names / text</span><span><kbd>Esc</kbd> close</span></div>' +
      "</div>";
    document.getElementById("layer-popups").appendChild(modal);
    input = modal.querySelector(".sr-input");
    list = modal.querySelector(".sr-list");
    status = modal.querySelector(".sr-status");
    modal.addEventListener("mousedown", function (ev) { if (ev.target === modal) close(); });
    modal.querySelector(".sr-modes").addEventListener("click", function (ev) {
      var b = ev.target.closest("[data-m]");
      if (b) setMode(b.getAttribute("data-m"));
    });
    modal.querySelector(".sr-opts").addEventListener("change", function () { run(); });
    input.addEventListener("input", function () { expanded = {}; mode === "text" ? runTextSoon() : run(); });
    input.addEventListener("keydown", onKey);
    list.addEventListener("click", function (ev) {
      var more = ev.target.closest("[data-more]");
      if (more) { expanded[more.getAttribute("data-more")] = true; run(); input.focus(); return; }
      var row = ev.target.closest("[data-i]");
      if (!row) return;
      ev.preventDefault();
      choose(+row.getAttribute("data-i"), ev.shiftKey ? "inspect" : "open");
    });
    list.addEventListener("mousemove", function (ev) {
      var row = ev.target.closest("[data-i]");
      if (row && +row.getAttribute("data-i") !== sel) { sel = +row.getAttribute("data-i"); mark(); }
    });
  }

  function setMode(m) {
    mode = m;
    modal.querySelectorAll(".sr-modes [data-m]").forEach(function (b) { b.classList.toggle("on", b.getAttribute("data-m") === m); });
    modal.querySelector(".sr-opts").classList.toggle("hidden", m !== "text");
    input.placeholder = m === "text" ? "Search the text of all code…" : "Search names…";
    modal.classList.toggle("text-mode", m === "text");
    if (m === "text") startText(); else run();
    input.focus();
  }

  function open(q, m) {
    ensure();
    built = false; // settings may have changed
    modal.classList.add("on");
    if (q != null) input.value = q;
    setMode(m || mode);
    input.select();
  }
  function close() { if (modal) modal.classList.remove("on"); }

  function onKey(ev) {
    if (ev.key === "Escape") { ev.preventDefault(); ev.stopPropagation(); close(); return; }
    if (ev.key === "Tab") { ev.preventDefault(); setMode(mode === "names" ? "text" : "names"); return; }
    if (ev.key === "ArrowDown" || ev.key === "ArrowUp") {
      ev.preventDefault();
      if (!flat.length) return;
      sel = (sel + (ev.key === "ArrowDown" ? 1 : -1) + flat.length) % flat.length;
      mark(true);
      return;
    }
    if (ev.key === "Enter") { ev.preventDefault(); if (flat.length) choose(sel, ev.shiftKey ? "inspect" : "open"); }
  }

  function mark(scroll) {
    list.querySelectorAll(".sr-row.on").forEach(function (r) { r.classList.remove("on"); });
    var r = list.querySelector('[data-i="' + sel + '"]');
    if (r) { r.classList.add("on"); if (scroll) r.scrollIntoView({ block: "nearest" }); }
  }

  function choose(i, how) {
    var x = flat[i];
    if (!x) return;
    close();
    if (x.go) { X.router.go(x.go); return; }
    if (how === "inspect") X.inspector.open(x.ref);
    else { var h = R.href(x.ref); if (h) X.router.go(h); else X.inspector.open(x.ref); }
  }

  function run() {
    if (mode === "text") { runTextSoon(); return; }
    var q = input.value;
    flat = []; sel = 0;
    if (!q.trim()) {
      list.innerHTML = '<div class="sr-empty muted">Type a name: a function, type, file, package, command, RPC method, route, configuration key or environment variable.</div>';
      return;
    }
    var res = query(q);
    var groups = new Map();
    res.forEach(function (r) {
      var k = r.source.kind;
      if (!groups.has(k)) groups.set(k, { source: r.source, items: [], best: r.score });
      groups.get(k).items.push(r);
    });
    var ordered = Array.from(groups.values()).sort(function (a, b) {
      var la = Math.floor(a.best / 1000), lb = Math.floor(b.best / 1000);
      return lb - la || (a.source.order || 0) - (b.source.order || 0);
    });
    var html = "";
    ordered.forEach(function (g) {
      var lim = expanded[g.source.kind] ? 200 : 8;
      html += '<div class="sr-group"><div class="sr-gh">' + esc(g.source.title) + ' <span class="count">' + U.fmt(g.items.length) + "</span></div>";
      g.items.slice(0, lim).forEach(function (r) {
        var it = r.item, i = flat.length;
        flat.push({ ref: it.ref });
        var info = R.resolve(it.ref);
        var icon = info.kind === "symbol" ? R.badge(info.obj) : U.icon(R.kinds[info.kind] && R.kinds[info.kind].icon || "file");
        html += '<a class="sr-row" data-i="' + i + '" data-ref="' + esc(it.ref) + '" data-nocard>' + R.zoneMark(info.zones) + icon +
          '<span class="sr-name mono">' + esc(it.name) + "</span>" +
          '<span class="sr-detail">' + esc(it.detail || "") + "</span></a>";
      });
      if (g.items.length > lim) html += '<button class="sr-more" data-more="' + esc(g.source.kind) + '">' + U.fmt(g.items.length - lim) + " more</button>";
      html += "</div>";
    });
    list.innerHTML = html || '<div class="sr-empty muted">Nothing found. <kbd>Tab</kbd> searches the text of the code.</div>';
    mark();
  }

  // ----- text in code

  var textReady = false, textLoading = false, lineStarts = new Map();
  function startText() {
    if (textReady) { runText(); return; }
    list.innerHTML = '<div class="sr-empty muted">Loading the code…</div>';
    if (textLoading) return;
    textLoading = true;
    var t0 = performance.now();
    D.allChunks(function (done, total) {
      status.textContent = "loading the code: " + done + " of " + total;
      if (mode === "text" && !textReady) list.innerHTML = '<div class="sr-empty muted"><div class="progress"><b style="width:' + Math.round(100 * done / total) + '%"></b></div>Loading the code: ' + done + " of " + total + " parts</div>";
    }).then(function () {
      textReady = true;
      status.textContent = "loaded in " + Math.round(performance.now() - t0) + " ms";
      if (mode === "text") runText();
    });
  }
  var runTextSoon = U.debounce(function () { if (textReady) runText(); }, 220);

  function starts(sf) {
    var s = lineStarts.get(sf.path);
    if (s) return s;
    s = [0];
    var src = sf.source;
    for (var i = 0; i < src.length; i++) if (src.charCodeAt(i) === 10) s.push(i + 1);
    lineStarts.set(sf.path, s);
    return s;
  }
  function lineAt(st, off) {
    var lo = 0, hi = st.length - 1;
    while (lo < hi) { var m = (lo + hi + 1) >> 1; if (st[m] <= off) lo = m; else hi = m - 1; }
    return lo;
  }

  function runText() {
    var q = input.value;
    flat = []; sel = 0;
    if (!q) { list.innerHTML = '<div class="sr-empty muted">Type text or a regular expression.</div>'; return; }
    var isRe = modal.querySelector('[data-o="regex"]').checked, cs = modal.querySelector('[data-o="case"]').checked;
    var re;
    try { re = new RegExp(isRe ? q : q.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"), "g" + (cs ? "" : "i") + "m"); }
    catch (e) { list.innerHTML = '<div class="sr-empty muted">Not a valid regular expression: ' + esc(e.message) + "</div>"; return; }
    var showTests = X.settings.get("tests");
    var t0 = performance.now(), max = 1500, count = 0, nFiles = 0, html = "", truncated = false;
    var filesList = D.core.files.filter(function (f) { return f.chunk && (showTests || !f.test); });
    for (var fi = 0; fi < filesList.length && !truncated; fi++) {
      var f = filesList[fi], sf = D.loadedSource(f.path);
      if (!sf) continue;
      re.lastIndex = 0;
      var m, st = null, lastLine = -1, rows = "";
      var n = 0;
      while ((m = re.exec(sf.source))) {
        if (!m[0]) { re.lastIndex++; if (re.lastIndex > sf.source.length) break; continue; }
        st = st || starts(sf);
        var li = lineAt(st, m.index);
        if (li === lastLine) continue;
        lastLine = li;
        var ls = st[li], le = li + 1 < st.length ? st[li + 1] - 1 : sf.source.length;
        var raw = sf.source.slice(ls, le), lead = /^\s*/.exec(raw)[0].length;
        var text = raw.slice(lead), a = Math.max(0, m.index - ls - lead), b = Math.min(text.length, a + m[0].length);
        var shown = text, off = 0;
        if (a > 80) { off = a - 40; shown = "…" + text.slice(off); a = a - off + 1; b = b - off + 1; }
        if (shown.length > 220) shown = shown.slice(0, 220);
        var i = flat.length;
        flat.push({ go: X.router.href("code", f.path, { line: li + 1 }) });
        rows += '<a class="sr-row tl" data-i="' + i + '"><span class="site-ln num">' + (li + 1) + '</span><code>' +
          esc(shown.slice(0, a)) + "<mark>" + esc(shown.slice(a, b)) + "</mark>" + esc(shown.slice(b)) + "</code></a>";
        n++; count++;
        if (count >= max) { truncated = true; break; }
      }
      if (n) {
        nFiles++;
        html += '<div class="sr-group"><div class="sr-gh mono">' + esc(f.path) + ' <span class="count">' + n + "</span></div>" + rows + "</div>";
      }
    }
    var ms = Math.round(performance.now() - t0);
    status.textContent = (truncated ? "first " : "") + U.plural(count, "line") + " in " + U.plural(nFiles, "file") + " · " + ms + " ms";
    list.innerHTML = html || '<div class="sr-empty muted">No line matches.</div>';
    mark();
  }

  document.addEventListener("keydown", function (ev) {
    var t = ev.target;
    var typing = t && (t.tagName === "INPUT" || t.tagName === "TEXTAREA" || t.tagName === "SELECT" || t.isContentEditable);
    if ((ev.key === "k" || ev.key === "K") && (ev.ctrlKey || ev.metaKey)) { ev.preventDefault(); open(null, "names"); return; }
    if (ev.key === "/" && !typing && !ev.ctrlKey && !ev.metaKey && !ev.altKey) { ev.preventDefault(); open(null, "names"); }
  });

  return { open: open, close: close, addSource: addSource, query: query };
})();
