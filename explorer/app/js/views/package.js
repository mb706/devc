// The package page (#/pkg/<path>) and the inspector's package summary:
// role, layer, group, where it runs, the package comment, files,
// neighbours, declarations by file, tests, and what the package
// contributes to the tables of entry points.
(function () {
  "use strict";
  var U = X.util, D = X.data, R = X.refs, esc = U.esc;

  function runsBars(p) {
    var acts = D.actorsOfPackage(p);
    if (!acts.length) return '<p class="muted small">No entry point of the model reaches its code.</p>';
    return '<div class="runs">' + acts.map(function (id) {
      var z = D.zoneOfActor(id), share = p.actors[id];
      return '<div class="run-row' + (z ? " z-" + esc(z.trust) : "") + '">' + R.chip("actor:" + id, { cls: "flat", noZone: true }) +
        '<span class="run-bar"><b style="width:' + Math.max(1.5, share * 100).toFixed(1) + '%"></b></span><span class="num small muted">' + U.pct(share) + "</span></div>";
    }).join("") + '</div><p class="small muted">Share of its functions\' lines reachable from each actor\'s entry points; an over-approximation.</p>';
  }

  function facts(p) {
    var L = D.layer(p.layer), G = D.group(p.group);
    var rows = [];
    if (L) rows.push(["Layer", R.chip("layer:" + L.id)]);
    if (G) rows.push(["Group", R.chip("group:" + G.id)]);
    rows.push(["Code", U.fmt(p.lines.code) + ' lines <span class="muted">+ ' + U.fmt(p.lines.comment) + " comment</span>"]);
    if (p.testLines.code) rows.push(["Tests", U.fmt(p.testLines.code) + " lines (" + U.pct(X.pkgTiles.testShare(p)) + ")"]);
    var goFiles = p.files.filter(function (f) { var x = D.file(f); return x && !x.test; }).length;
    rows.push(["Files", goFiles + (p.files.length > goFiles ? ' <span class="muted">+ ' + (p.files.length - goFiles) + " test</span>" : "")]);
    rows.push(["Import", '<code class="small">' + esc(p.importPath).replace(/\//g, "/<wbr>") + "</code>"]);
    return '<dl class="facts">' + rows.map(function (r) { return "<dt>" + r[0] + "</dt><dd>" + r[1] + "</dd>"; }).join("") + "</dl>";
  }

  // ----- what the package contributes to the entry point tables

  function contributions(p) {
    var inPkg = function (ref) { var s = ref && D.sym(ref); return s && s.package === p.path; };
    var inFile = function (f) { var x = D.file(f); return x && x.package === p.path; };
    var out = [];
    var cmds = D.core.commands.filter(function (c) { return inPkg(c.run); });
    if (cmds.length) out.push(["Commands it runs", cmds.map(function (c) { return R.chip("cmd:" + c.path); })]);
    var defs = D.core.rpc.filter(function (m) { return inPkg(m.const); });
    if (defs.length) out.push(["RPC methods it defines", defs.map(function (m) { return R.chip("rpc:" + m.name); })]);
    var handles = D.core.rpc.filter(function (m) { return (m.handlers || []).some(function (s) { return inPkg(s.handler) || inFile(s.file); }); });
    if (handles.length) out.push(["RPC methods it handles", handles.map(function (m) { return R.chip("rpc:" + m.name); })]);
    var calls = D.core.rpc.filter(function (m) { return (m.callers || []).some(function (s) { return inFile(s.file); }); });
    if (calls.length) out.push(["RPC methods it calls", calls.map(function (m) { return R.chip("rpc:" + m.name); })]);
    var routes = D.core.routes.filter(function (r) { return inPkg(r.handler); });
    if (routes.length) out.push(["Routes it handles", routes.map(function (r) { return R.chip("http:" + r.key); })]);
    var writes = D.core.events.filter(function (e) { return (e.writers || []).some(function (s) { return inFile(s.file); }); });
    if (writes.length) out.push(["Events it writes", writes.map(function (e) { return R.chip("event:" + e.name); })]);
    var reads = D.core.events.filter(function (e) { return (e.readers || []).some(function (s) { return inFile(s.file); }); });
    if (reads.length) out.push(["Events it reads", reads.map(function (e) { return R.chip("event:" + e.name); })]);
    var surfaces = (D.model.surfaces || []).filter(function (s) { return s.kind === "config" && inPkg(s.root); });
    if (surfaces.length) out.push(["Configuration files it defines", surfaces.map(function (s) { return R.chip("surface:" + s.id); })]);
    var env = D.core.env.filter(function (e) { return (e.uses || []).some(function (u) { return inFile(u.file); }); });
    if (env.length) out.push(["Environment variables it names", env.map(function (e) { return R.chip("env:" + e.name); })]);
    return out;
  }

  // ----- declarations

  function declarations(p, showUnexported) {
    var syms = D.symbolsOfPackage(p.path).filter(function (s) { return !s.test && s.kind !== "field" && s.kind !== "imethod"; });
    var byFile = U.groupBy(syms.filter(function (s) { return s.kind !== "method"; }), function (s) { return s.file; });
    var methods = U.groupBy(syms.filter(function (s) { return s.kind === "method"; }), function (s) { return s.parent; });
    var order = { type: 0, func: 1, var: 2, const: 3 };
    var html = "";
    p.files.forEach(function (f) {
      var list = (byFile.get(f) || []).filter(function (s) { return showUnexported || s.exported || (s.kind === "type" && (methods.get(s.ref) || []).some(function (m) { return m.exported; })); });
      if (!list.length) return;
      list = U.sortBy(list, function (s) { return (order[s.kind] || 9) + "\u0000" + (s.exported ? "0" : "1") + s.name.toLowerCase(); });
      var fo = D.file(f);
      html += '<div class="decl-file"><div class="decl-fh">' + R.chip("file:" + f, { act: "full" }) +
        (fo && fo.doc ? '<span class="muted small">' + esc(U.firstSentence(fo.doc)) + "</span>" : "") + "</div><ul class=\"decls\">";
      list.forEach(function (s) {
        html += declItem(s);
        if (s.kind === "type") {
          var ms = (methods.get(s.ref) || []).filter(function (m) { return showUnexported || m.exported; });
          ms = U.sortBy(ms, function (m) { return (m.exported ? "0" : "1") + m.name.toLowerCase(); });
          if (ms.length) html += '<li class="decl-sub"><ul class="decls">' + ms.map(declItem).join("") + "</ul></li>";
        }
      });
      html += "</ul></div>";
    });
    return html || '<p class="muted">No declarations' + (showUnexported ? "" : " that are exported") + ".</p>";
  }
  function declItem(s) {
    return '<li class="decl' + (s.exported ? "" : " unexp") + '">' + R.chip(s.ref, { label: s.name, act: "full" }) +
      '<span class="decl-doc">' + (s.doc ? X.doc.inline(U.firstSentence(s.doc), { pkg: s.package }) : '<span class="muted">—</span>') + "</span></li>";
  }

  function tests(p) {
    var t = D.symbolsOfPackage(p.path).filter(function (s) { return s.test && s.kind === "func" && /^(Test|Fuzz|Benchmark|Example)/.test(s.name); });
    if (!t.length) return "";
    var byFile = U.groupBy(t, function (s) { return s.file; });
    var html = "";
    byFile.forEach(function (list, f) {
      html += '<div class="decl-file"><div class="decl-fh">' + R.chip("file:" + f, { act: "full" }) + ' <span class="count">' + list.length + "</span></div><ul class=\"decls cols\">" +
        list.map(function (s) { return '<li class="decl">' + R.chip(s.ref, { label: s.name, act: "full" }) + "</li>"; }).join("") + "</ul></div>";
    });
    return html;
  }

  function fileRows(p) {
    return p.files.map(function (f) { return D.file(f); }).filter(Boolean);
  }

  // ----- the page

  function render(el, r) {
    var p = D.pkg(r.rest);
    if (!p) { el.innerHTML = '<div class="page narrow"><h1>No such package</h1><p class="muted"><code>' + esc(r.rest) + "</code> is not a package of this build.</p></div>"; return; }
    var showUnexp = U.load("pkgUnexported", true);
    var G = D.group(p.group);
    var contrib = contributions(p);
    el.innerHTML =
      '<div class="pkgpage" style="--gh:' + X.pkgTiles.groupHue(p.group) + '">' +
      '<div class="pp-main">' +
      '<header class="pp-head"><div class="pp-title">' + U.icon("package") + '<h1 class="mono">' + esc(p.path) + "</h1>" +
      (p.testOnly ? '<span class="tag">tests only</span>' : "") + "</div>" +
      (p.role ? '<p class="lead">' + X.doc.inline(p.role, { pkg: p.path }) + "</p>" : "") +
      '<div class="pp-tools"><a class="btn" href="' + esc(X.router.hrefKeep("code", p.path)) + '">' + U.icon("dir") + "Files in the code view</a>" +
      '<a class="btn" href="' + esc(X.router.hrefKeep("packages", "", {})) + '">' + U.icon("layer") + "On the package map</a></div></header>" +
      (p.doc ? '<section class="pp-sec"><h2 class="sec-h">Package comment' + (p.docFile ? ' <span class="muted small">' + R.chip("file:" + p.docFile, { act: "full", cls: "small" }) + "</span>" : "") + '</h2><div class="doc prose">' + X.doc.render(p.doc, { pkg: p.path }) + "</div></section>" :
        '<section class="pp-sec"><p class="muted">No package comment.</p></section>') +
      '<section class="pp-sec"><h2 class="sec-h">Files <span class="count">' + p.files.length + '</span></h2><div class="pp-files"></div></section>' +
      '<section class="pp-sec"><h2 class="sec-h">Neighbours</h2>' + neighbours(p) + "</section>" +
      '<section class="pp-sec"><h2 class="sec-h">Declarations <label class="check"><input type="checkbox" data-a="unexp"' + (showUnexp ? " checked" : "") + '> unexported</label></h2><div class="pp-decls">' + declarations(p, showUnexp) + "</div></section>" +
      (contrib.length ? '<section class="pp-sec"><h2 class="sec-h">Entry points</h2><dl class="contrib">' + contrib.map(function (c) {
        return "<dt>" + esc(c[0]) + ' <span class="count">' + c[1].length + "</span></dt><dd><div class=\"chips\">" + c[1].join("") + "</div></dd>";
      }).join("") + "</dl></section>" : "") +
      (tests(p) ? '<section class="pp-sec"><h2 class="sec-h">Tests</h2>' + tests(p) + "</section>" : "") +
      "</div>" +
      '<aside class="pp-side"><div class="pp-side-in">' +
      '<h3 class="sp-label">Facts</h3>' + facts(p) +
      '<h3 class="sp-label">May run in</h3>' + runsBars(p) +
      "</div></aside></div>";
    var showTests = X.settings.get("tests");
    X.table.create(el.querySelector(".pp-files"), {
      columns: [
        { id: "name", title: "File", html: function (f) { return R.chip("file:" + f.path, { label: U.lastElem(f.path), act: "full" }) + (f.test ? ' <span class="tag">test</span>' : ""); }, sort: function (f) { return f.path; }, width: "220px" },
        { id: "doc", title: "Description", html: function (f) { return f.doc ? X.doc.inline(U.firstSentence(f.doc), { pkg: p.path }) : '<span class="muted">—</span>'; }, sort: function (f) { return f.doc || "~"; } },
        { id: "lines", title: "Lines", cls: "num r", html: function (f) { return U.fmt(f.lines.code); }, sort: function (f) { return -f.lines.code; }, width: "70px" }
      ],
      rows: fileRows(p).filter(function (f) { return showTests || !f.test; }),
      key: function (f) { return f.path; },
      text: function (f) { return f.path + " " + (f.doc || ""); },
      placeholder: "Filter files…"
    });
    el.querySelector('[data-a="unexp"]').addEventListener("change", function (ev) {
      U.save("pkgUnexported", ev.target.checked);
      el.querySelector(".pp-decls").innerHTML = declarations(p, ev.target.checked);
    });
  }

  function neighbours(p) {
    function grid(title, list) {
      if (!list || !list.length) return '<div class="nb"><h3 class="sp-label">' + title + ' <span class="count">0</span></h3><p class="muted small">None.</p></div>';
      return '<div class="nb"><h3 class="sp-label">' + title + ' <span class="count">' + list.length + '</span></h3><div class="tiles-grid">' +
        list.map(function (q) { var x = D.pkg(q); return x ? X.pkgTiles.tile(x, { color: "group" }) : ""; }).join("") + "</div></div>";
    }
    var h = grid("Imports", p.imports) + grid("Imported by", p.importedBy);
    if (p.testImports && p.testImports.length) h += '<div class="nb"><h3 class="sp-label">Imported only by its tests</h3><div class="chips">' + p.testImports.map(function (q) { return R.chip("pkg:" + q); }).join("") + "</div></div>";
    return h;
  }

  // Tiles on the package page behave as on the map: click inspects.
  document.addEventListener("click", function (ev) {
    var t = ev.target.closest && ev.target.closest(".pkgpage .ptile, .insp-body .ptile");
    if (!t || ev.defaultPrevented || ev.target.closest("[data-ref]")) return;
    var path = t.getAttribute("data-pkg");
    if (U.isModifiedClick(ev)) X.router.go(X.router.hrefKeep("pkg", path)); else X.inspector.open("pkg:" + path);
  });

  X.router.register("pkg", {
    nav: "packages",
    title: function (r) { return r.rest; },
    render: render
  });

  // ----- the inspector's summary

  X.inspector.register("package", function (info, body) {
    var p = info.obj;
    var el = U.h("div.insp-pad.pkgsum");
    var showTests = X.settings.get("tests");
    var files = fileRows(p).filter(function (f) { return showTests || !f.test; });
    el.innerHTML =
      (p.role ? '<p class="lead">' + X.doc.inline(p.role, { pkg: p.path }) + "</p>" : "") +
      facts(p) +
      '<h3 class="sp-label">May run in</h3>' + runsBars(p) +
      (p.doc ? '<h3 class="sp-label">Package comment</h3><div class="doc prose clamp">' + X.doc.render(p.doc, { pkg: p.path }) + '</div><button class="btn ghost small" data-a="more">Show all</button>' : "") +
      '<h3 class="sp-label">Files <span class="count">' + files.length + "</span></h3>" +
      '<ul class="flist">' + files.map(function (f) {
        return "<li>" + R.chip("file:" + f.path, { label: U.lastElem(f.path) }) + '<span class="num muted small">' + U.fmt(f.lines.code) + "</span>" +
          (f.doc ? '<span class="muted small fl-doc">' + esc(U.firstSentence(f.doc)) + "</span>" : "") + "</li>";
      }).join("") + "</ul>" +
      '<h3 class="sp-label">Imports <span class="count">' + (p.imports || []).length + '</span></h3><div class="chips">' + (p.imports || []).map(function (q) { return R.chip("pkg:" + q); }).join("") + "</div>" +
      '<h3 class="sp-label">Imported by <span class="count">' + (p.importedBy || []).length + '</span></h3><div class="chips">' + (p.importedBy || []).map(function (q) { return R.chip("pkg:" + q); }).join("") + "</div>" +
      '<p><a class="btn" href="' + esc(X.router.hrefKeep("pkg", p.path)) + '">' + U.icon("full") + "Open the package page</a></p>";
    body.appendChild(el);
    var more = el.querySelector('[data-a="more"]');
    if (more) more.onclick = function () { el.querySelector(".clamp").classList.remove("clamp"); more.remove(); };
  });
})();
