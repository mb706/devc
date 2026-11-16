// The Code view: the file tree (left), the source (middle) and, on the
// right, the symbol panel of the selected declaration above the file's
// outline. Route: #/code/<path>?sym=<ref>&line=<n or a-b>; a directory
// path lists the directory. Clicking an identifier goes to its
// definition here; the inspector is for looking from elsewhere.
(function () {
  "use strict";
  var U = X.util, D = X.data, R = X.refs, esc = U.esc;

  // ----- the tree of directories and files (built once)

  var treeRoot = null;
  function buildTree() {
    if (treeRoot) return treeRoot;
    treeRoot = { name: "", path: "", dirs: new Map(), files: [] };
    D.core.files.forEach(function (f) {
      var parts = f.path.split("/"), node = treeRoot, acc = "";
      for (var i = 0; i < parts.length - 1; i++) {
        acc = acc ? acc + "/" + parts[i] : parts[i];
        if (!node.dirs.has(parts[i])) node.dirs.set(parts[i], { name: parts[i], path: acc, dirs: new Map(), files: [] });
        node = node.dirs.get(parts[i]);
      }
      node.files.push(f);
    });
    return treeRoot;
  }
  function dirNode(path) {
    var node = buildTree();
    if (!path) return node;
    var parts = path.split("/");
    for (var i = 0; i < parts.length; i++) { node = node.dirs.get(parts[i]); if (!node) return null; }
    return node;
  }
  function hasVisible(node, showTests) {
    if (node.files.some(function (f) { return showTests || !f.test; })) return true;
    var any = false;
    node.dirs.forEach(function (d) { if (!any && hasVisible(d, showTests)) any = true; });
    return any;
  }
  function countFiles(node, showTests) {
    var n = node.files.filter(function (f) { return showTests || !f.test; }).length;
    node.dirs.forEach(function (d) { n += countFiles(d, showTests); });
    return n;
  }

  var expanded = new Set(U.load("codeTreeOpen", null) || []);
  if (!U.load("codeTreeOpen", null)) {
    // Open the first level that holds most of the code.
    var counts = [];
    buildTree().dirs.forEach(function (d) { counts.push([countFiles(d, true), d.path]); });
    counts.sort(function (a, b) { return b[0] - a[0]; });
    if (counts[0]) expanded.add(counts[0][1]);
  }
  function saveExpanded() { U.save("codeTreeOpen", Array.from(expanded)); }

  function treeHTML(node, depth, cur, filter, showTests) {
    var html = "";
    var dirs = Array.from(node.dirs.values()).sort(function (a, b) { return a.name < b.name ? -1 : 1; });
    dirs.forEach(function (d) {
      if (!hasVisible(d, showTests)) return;
      var inner = "";
      var open = filter ? null : expanded.has(d.path);
      if (filter) {
        inner = treeHTML(d, depth + 1, cur, filter, showTests);
        if (!inner && !filter.dir(d)) return;
        open = !!inner;
      } else if (open) inner = treeHTML(d, depth + 1, cur, filter, showTests);
      var pk = D.pkg(d.path);
      var role = pk ? U.firstSentence(pk.role || pk.doc) : "";
      html += '<div class="tn dir' + (open ? " open" : "") + (cur === d.path ? " cur" : "") + '" data-dir="' + esc(d.path) + '" style="--d:' + depth + '">' +
        '<span class="tw">' + U.icon("fwd", "caret") + "</span>" +
        (pk ? '<span class="ti" data-ref="pkg:' + esc(d.path) + '">' + U.icon("package") + "</span>" : '<span class="ti">' + U.icon("dir") + "</span>") +
        '<span class="tname">' + esc(d.name) + "</span>" +
        (role ? '<span class="trole">' + esc(role.replace(/`/g, "")) + "</span>" : "") + "</div>";
      if (open) html += inner;
    });
    node.files.slice().sort(function (a, b) { return a.path < b.path ? -1 : 1; }).forEach(function (f) {
      if (!showTests && f.test) return;
      if (filter && !filter.file(f)) return;
      var n = f.lines.code + f.lines.comment + f.lines.blank;
      var first = U.firstSentence(f.doc);
      html += '<a class="tn file' + (f.test ? " test" : "") + (cur === f.path ? " cur" : "") + '" data-file="' + esc(f.path) +
        '" href="' + esc(X.router.hrefKeep("code", f.path)) + '" style="--d:' + depth + '"' + (first ? ' title="' + esc(first) + '"' : "") + ">" +
        '<span class="tw"></span><span class="ti">' + U.icon("file") + "</span>" +
        '<span class="tname">' + esc(U.lastElem(f.path)) + "</span>" +
        '<span class="tlines num">' + U.fmt(n) + "</span>" +
        (first ? '<span class="tdoc">' + esc(first.replace(/`/g, "")) + "</span>" : "") + "</a>";
    });
    return html;
  }

  function makeFilter(q) {
    var toks = q.toLowerCase().split(/\s+/).filter(Boolean);
    if (!toks.length) return null;
    function m(t) { t = t.toLowerCase(); return toks.every(function (k) { return t.indexOf(k) >= 0; }); }
    return {
      file: function (f) { return m(f.path + " " + (f.doc ? U.firstSentence(f.doc) : "")); },
      dir: function (d) { var p = D.pkg(d.path); return m(d.path + " " + (p ? p.role || "" : "")); }
    };
  }

  // ----- the view

  var S = null; // the mounted view's state
  // Where the source was scrolled, per URL, so that back and forward
  // return the reader to the place they left.
  var scrollMemo = new Map();
  function memoKey() { return location.hash.replace(/([?&])i=[^&]*&?/, "$1").replace(/[?&]$/, ""); }

  function render(el, r) {
    var target = resolveTarget(r);
    if (target.redirect) { X.router.replace(target.redirect); return; }
    el.innerHTML =
      '<div class="cv">' +
      '<div class="cv-tree"><div class="cv-tree-bar"><input type="search" placeholder="Filter files…" spellcheck="false">' +
      '<label class="check" title="Show test files"><input type="checkbox" data-a="tests"' + (X.settings.get("tests") ? " checked" : "") + "> tests</label></div>" +
      '<div class="tree"></div></div>' +
      '<div class="cv-center"></div>' +
      '<div class="cv-side"><div class="cv-sym"></div><div class="cv-outline"></div></div>' +
      "</div>";
    S = {
      el: el, path: null, src: null, sym: "", cleanup: null, filter: null,
      tree: el.querySelector(".tree"), center: el.querySelector(".cv-center"),
      side: el.querySelector(".cv-side"), symEl: el.querySelector(".cv-sym"), outline: el.querySelector(".cv-outline")
    };
    var input = el.querySelector(".cv-tree-bar input");
    input.addEventListener("input", U.debounce(function () { S.filter = makeFilter(input.value); drawTree(); }, 120));
    el.querySelector('[data-a="tests"]').addEventListener("change", function (ev) { X.settings.set("tests", ev.target.checked); drawTree(); if (S.path) showOutline(); });
    S.tree.addEventListener("click", onTreeClick);
    show(target, r);
  }

  // resolveTarget turns the route into a file, a directory or a symbol.
  function resolveTarget(r) {
    var sym = r.params.sym ? D.sym(r.params.sym) : null;
    var path = r.rest;
    if (sym && sym.file !== path) return { redirect: X.router.href("code", sym.file, copyParams(r.params)) };
    if (!path) return { dir: "" };
    if (D.file(path)) return { file: path, sym: sym, line: parseLine(r.params.line) };
    if (dirNode(path)) return { dir: path };
    return { missing: path };
  }
  function copyParams(p) { var o = {}; Object.keys(p).forEach(function (k) { o[k] = p[k]; }); return o; }
  function parseLine(s) {
    if (!s) return 0;
    var m = /^(\d+)(?:-(\d+))?$/.exec(s);
    if (!m) return 0;
    return m[2] ? [+m[1], +m[2]] : +m[1];
  }

  function show(t, r) {
    if (t.missing) {
      S.path = null;
      S.center.innerHTML = '<div class="page narrow"><h1>Not in this build</h1><p class="muted"><code>' + esc(t.missing) + "</code> is neither a file nor a directory of the site.</p></div>";
      S.el.querySelector(".cv").classList.add("no-side");
      drawTree();
      return;
    }
    if (t.dir != null) { showDir(t.dir); return; }
    if (t.file !== S.path) openFile(t, r);
    else select(t, true);
  }

  function update(r) {
    if (!S) return false;
    var t = resolveTarget(r);
    if (t.redirect) { X.router.replace(t.redirect); return true; }
    show(t, r);
    return true;
  }

  function drawTree() {
    var cur = S.path || S.dir || "";
    S.tree.innerHTML = treeHTML(buildTree(), 0, cur, S.filter, X.settings.get("tests")) ||
      '<div class="muted small pad">No file matches.</div>';
    var c = S.tree.querySelector(".tn.cur");
    if (c) {
      var tr = S.tree.getBoundingClientRect(), cr = c.getBoundingClientRect();
      if (cr.top < tr.top || cr.bottom > tr.bottom) c.scrollIntoView({ block: "center" });
    }
  }
  function onTreeClick(ev) {
    if (ev.target.closest("[data-ref]")) return; // the package icon inspects
    var d = ev.target.closest(".tn.dir");
    if (d) {
      var p = d.getAttribute("data-dir");
      if (ev.target.closest(".tname") && !d.classList.contains("open")) { expanded.add(p); saveExpanded(); X.router.go(X.router.hrefKeep("code", p)); return; }
      if (expanded.has(p)) expanded.delete(p); else expanded.add(p);
      saveExpanded();
      drawTreeKeep();
      return;
    }
  }
  function drawTreeKeep() {
    var top = S.tree.scrollTop;
    S.tree.innerHTML = treeHTML(buildTree(), 0, S.path || S.dir || "", S.filter, X.settings.get("tests"));
    S.tree.scrollTop = top;
  }
  // openAncestors opens the directories above a path in the tree.
  function openAncestors(path) {
    var parts = path.split("/"), acc = "";
    for (var i = 0; i < parts.length - 1; i++) { acc = acc ? acc + "/" + parts[i] : parts[i]; expanded.add(acc); }
    saveExpanded();
  }

  // ----- a directory

  function showDir(path) {
    cleanupFile();
    S.path = null; S.dir = path;
    S.el.querySelector(".cv").classList.add("no-side");
    if (path) { openAncestors(path + "/x"); }
    drawTree();
    var node = dirNode(path), showTests = X.settings.get("tests");
    var pk = D.pkg(path);
    var html = '<div class="cv-dir"><div class="page">';
    html += '<div class="dir-head"><h1 class="mono">' + esc(path || "/") + "</h1>" +
      (pk ? R.chip("pkg:" + path, { label: "package page", act: "full" }) : "") + "</div>";
    if (pk && (pk.role || pk.doc)) html += '<p class="lead">' + X.doc.inline(pk.role || U.firstParagraph(pk.doc), { pkg: path }) + "</p>";
    var dirs = Array.from(node.dirs.values()).filter(function (d) { return hasVisible(d, showTests); }).sort(function (a, b) { return a.name < b.name ? -1 : 1; });
    if (dirs.length) {
      html += '<h3 class="sec-h">Directories</h3><div class="dir-list">' + dirs.map(function (d) {
        var p = D.pkg(d.path);
        return '<a class="dir-item" href="' + esc(X.router.hrefKeep("code", d.path)) + '">' + U.icon(p ? "package" : "dir") +
          '<span class="mono">' + esc(d.name) + "/</span>" +
          '<span class="muted small">' + (p ? X.doc.plain(U.firstSentence(p.role || p.doc)) : esc(U.plural(countFiles(d, showTests), "file"))) + "</span></a>";
      }).join("") + "</div>";
    }
    var files = node.files.filter(function (f) { return showTests || !f.test; });
    if (files.length) {
      html += '<h3 class="sec-h">Files</h3><div class="dir-list">' + files.map(function (f) {
        var n = f.lines.code + f.lines.comment + f.lines.blank;
        return '<a class="dir-item" href="' + esc(X.router.hrefKeep("code", f.path)) + '" data-ref="file:' + esc(f.path) + '" data-act="full">' + U.icon("file") +
          '<span class="mono">' + esc(U.lastElem(f.path)) + '</span><span class="num muted small">' + U.fmt(n) + " lines</span>" +
          '<span class="muted small">' + X.doc.plain(U.firstSentence(f.doc)) + "</span></a>";
      }).join("") + "</div>";
    }
    html += "</div></div>";
    S.center.innerHTML = html;
  }

  // ----- a file

  function cleanupFile() {
    if (S.src) S.src.destroy();
    if (S.cleanup) S.cleanup();
    if (S.onScroll && S.scroller) S.scroller.removeEventListener("scroll", S.onScroll);
    S.src = null; S.cleanup = null; S.onScroll = null;
  }

  function openFile(t, r) {
    cleanupFile();
    S.path = t.file; S.dir = null; S.shownSym = null; S.shownLine = null;
    var fo = D.file(t.file);
    S.el.querySelector(".cv").classList.toggle("no-side", !(fo && fo.lang === "go" && !fo.ignored));
    openAncestors(t.file);
    drawTree();
    var f = D.file(t.file);
    S.center.innerHTML = "";
    S.line = t.line;
    var head = X.source.header(t.file, { line: function () { var l = S.line || (S.symObj && S.symObj.line); return Array.isArray(l) ? l[0] : l; } });
    S.center.appendChild(head);
    var scroller = U.h("div.cv-src");
    S.center.appendChild(scroller);
    S.scroller = scroller;
    showOutline();
    if (!f.chunk) {
      scroller.innerHTML = '<div class="page narrow muted">A binary file (' + U.fmt(f.bytes) + " bytes); there is nothing to show.</div>";
      return;
    }
    var t0 = performance.now();
    S.src = X.source.view(scroller, {
      path: t.file,
      onIdent: onIdent,
      onLine: onLine,
      onReady: function (v) {
        S.openMs = performance.now() - t0;
        document.documentElement.dataset.fileOpenMs = Math.round(S.openMs);
        select(t, false);
      }
    });
    S.onScroll = onScrollFactory();
    scroller.addEventListener("scroll", S.onScroll, { passive: true });
  }

  function onIdent(ref) {
    if (ref.indexOf("pkg:") === 0) { X.router.go(X.router.hrefKeep("pkg", ref.slice(4))); return; }
    var s = D.sym(ref);
    if (!s) return;
    X.router.go(X.router.hrefKeep("code", s.file, { sym: ref }));
  }
  function onLine(n, ev) {
    var cur = S.line, val = String(n);
    if (ev.shiftKey && cur) {
      var a = Array.isArray(cur) ? cur[0] : cur;
      val = Math.min(a, n) + "-" + Math.max(a, n);
    }
    X.router.setParams({ line: val }, true);
  }

  // select applies the route's symbol and line to the open file.
  function select(t, routeChange) {
    if (!S.src || !S.src.source) {
      showSymbol(t.sym);
      return;
    }
    var sym = t.sym, v = S.src;
    var symChanged = (sym ? sym.ref : "") !== S.shownSym, lineKey = String(t.line || "");
    var lineChanged = lineKey !== S.shownLine;
    S.shownSym = sym ? sym.ref : ""; S.shownLine = lineKey;
    S.line = t.line;
    v.setLine(t.line || 0);
    var first = Array.isArray(t.line) ? t.line[0] : t.line;
    var memo = scrollMemo.get(memoKey());
    if (memo != null && (symChanged || lineChanged || !routeChange)) {
      v.setRange(sym ? [X.source.declStart(v.source, sym), sym.endLine] : null);
      S.scroller.scrollTop = memo;
    } else if (sym) {
      var start = X.source.declStart(v.source, sym);
      v.setRange([start, sym.endLine]);
      if (symChanged || !routeChange) v.scrollTo(start, "start");
      else if (lineChanged && first) v.scrollTo(first, "nearest");
    } else {
      v.setRange(null);
      if (first && (lineChanged || !routeChange)) v.scrollTo(first, routeChange ? "nearest" : "center");
      else if (!routeChange) v.scrollTo(1, "start");
    }
    showSymbol(sym);
    markOutline();
  }

  function showSymbol(sym) {
    var ref = sym ? sym.ref : "";
    S.symObj = sym;
    if (ref === S.sym) return;
    S.sym = ref;
    if (S.cleanup) { S.cleanup(); S.cleanup = null; }
    S.symEl.innerHTML = "";
    S.side.classList.toggle("has-sym", !!sym);
    if (!sym) return;
    var close = U.h("button.btn.ghost.icon.sp-close", { type: "button", title: "Close the symbol panel", html: U.icon("close") });
    close.onclick = function () { X.router.setParams({ sym: null }, true); };
    S.symEl.appendChild(close);
    var body = U.h("div");
    S.symEl.appendChild(body);
    S.cleanup = X.symbolPanel.render(body, ref, {});
    S.symEl.scrollTop = 0;
  }

  // ----- the outline

  function outlineItems(path) {
    var showTests = X.settings.get("tests");
    var syms = D.symbolsOfFile(path).filter(function (s) { return (s.kind !== "field" && s.kind !== "imethod") && (showTests || !s.test); });
    var types = new Map(), items = [];
    syms.forEach(function (s) { if (s.kind === "type") types.set(s.ref, { s: s, methods: [] }); });
    var foreign = new Map();
    syms.forEach(function (s) {
      if (s.kind === "method") {
        var t = types.get(s.parent);
        if (t) { t.methods.push(s); return; }
        if (!foreign.has(s.parent)) { var g = { foreign: s.parent, methods: [], line: s.line }; foreign.set(s.parent, g); items.push(g); }
        foreign.get(s.parent).methods.push(s);
        return;
      }
      if (s.kind === "type") items.push(types.get(s.ref));
      else items.push({ s: s });
    });
    return items;
  }

  function showOutline() {
    var items = outlineItems(S.path);
    var flat = [];
    var html = '<div class="ol-bar"><span class="sp-label">Outline</span><input type="search" placeholder="Filter…" spellcheck="false"></div><div class="ol-list">';
    if (!items.length) html += '<div class="muted small pad">No declarations.</div>';
    items.forEach(function (it) {
      if (it.foreign) {
        var p = D.sym(it.foreign);
        html += '<div class="ol-g muted small">methods of ' + (p ? R.chip(p.ref, { cls: "small" }) : esc(it.foreign)) + "</div>";
      } else {
        html += olItem(it.s, 0);
        flat.push(it.s);
      }
      (it.methods || []).forEach(function (m) { html += olItem(m, 1); flat.push(m); });
    });
    S.outline.innerHTML = html + "</div>";
    S.outlineFlat = flat.sort(function (a, b) { return a.line - b.line; });
    S.outlineStarts = null;
    var input = S.outline.querySelector("input");
    input.addEventListener("input", function () {
      var q = input.value.toLowerCase();
      S.outline.querySelectorAll(".ol-i").forEach(function (e) { e.classList.toggle("hidden", !!q && e.getAttribute("data-name").indexOf(q) < 0); });
    });
    S.outline.querySelector(".ol-list").addEventListener("click", function (ev) {
      var a = ev.target.closest("a.ol-i");
      if (!a || U.isModifiedClick(ev)) return;
      ev.preventDefault();
      X.router.go(X.router.hrefKeep("code", S.path, { sym: a.getAttribute("data-sym") }));
    });
    markOutline();
  }
  function olItem(s, depth) {
    return '<a class="ol-i' + (depth ? " sub" : "") + (s.exported ? "" : " unexp") + '" data-sym="' + esc(s.ref) + '" data-name="' + esc(s.name.toLowerCase()) +
      '" href="' + esc(X.router.hrefKeep("code", s.file, { sym: s.ref })) + '">' + R.badge(s) + '<span class="mono">' + esc(s.name) + "</span>" +
      '<span class="ol-ln num">' + s.line + "</span></a>";
  }

  // markOutline marks the declaration at the top of the source.
  function markOutline() {
    if (!S.outlineFlat || !S.src || !S.src.source) return;
    // A declaration counts from the first line of its doc comment.
    // The reader's eye is near the top of the source, a little below it.
    var line = S.src.firstVisibleLine() + Math.max(2, Math.round(S.scroller.clientHeight / 20 / 5)), cur = null, sf = S.src.source;
    if (!S.outlineStarts || S.outlineStarts.path !== S.path) {
      S.outlineStarts = { path: S.path, m: new Map() };
      S.outlineFlat.forEach(function (x) { S.outlineStarts.m.set(x.ref, X.source.declStart(sf, x)); });
    }
    for (var i = 0; i < S.outlineFlat.length; i++) {
      var s = S.outlineFlat[i], st = S.outlineStarts.m.get(s.ref) || s.line;
      if (st <= line) { if (!cur || s.line >= cur.line) cur = s; } else break;
    }
    var prev = S.outline.querySelector(".ol-i.cur");
    var next = cur ? S.outline.querySelector('.ol-i[data-sym="' + cssEsc(cur.ref) + '"]') : null;
    if (prev === next) return;
    if (prev) prev.classList.remove("cur");
    if (next) {
      next.classList.add("cur");
      var list = S.outline.querySelector(".ol-list"), lr = list.getBoundingClientRect(), nr = next.getBoundingClientRect();
      if (nr.top < lr.top || nr.bottom > lr.bottom) list.scrollTop += nr.top - lr.top - lr.height / 3;
    }
    var sl = S.outline.querySelector(".ol-i.sel"), want = S.sym ? S.outline.querySelector('.ol-i[data-sym="' + cssEsc(S.sym) + '"]') : null;
    if (sl !== want) { if (sl) sl.classList.remove("sel"); if (want) want.classList.add("sel"); }
  }
  function onScrollFactory() {
    var raf = 0;
    return function () {
      if (raf) return;
      raf = requestAnimationFrame(function () {
        raf = 0;
        markOutline();
        if (S && S.scroller && S.src && S.src.source) scrollMemo.set(memoKey(), S.scroller.scrollTop);
      });
    };
  }
  function cssEsc(s) { return window.CSS && CSS.escape ? CSS.escape(s) : s; }

  X.router.register("code", {
    nav: "code",
    title: function (r) { return r.rest ? U.lastElem(r.rest) : "Code"; },
    render: render,
    update: update,
    unmount: function () { if (S) cleanupFile(); S = null; }
  });

  // The inspector's view of a file: its header and the whole source.
  X.inspector.register("file", function (info, body) {
    var path = info.key.split("~")[0], line = info.line && info.key.indexOf("~") > 0 ? info.line : 0;
    body.classList.add("fill");
    var head = X.source.header(path, { line: function () { return line || 1; } });
    var scroller = U.h("div.insp-src");
    body.appendChild(head);
    body.appendChild(scroller);
    var f = D.file(path);
    var v = null;
    if (f && f.chunk) v = X.source.view(scroller, { path: path, wrap: true, line: line || 0, scroll: line || 0 });
    else scroller.innerHTML = '<div class="pad muted">A binary file.</div>';
    return function () { body.classList.remove("fill"); if (v) v.destroy(); };
  });
})();
