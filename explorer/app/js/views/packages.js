// The Packages view. Layers: one row per layer of the model, top layer
// first, each package a tile; hovering or selecting a tile draws its
// imports and importers with the connector layer. Size: a treemap of
// packages and their files by lines of code. Both colour packages by
// group, by where the code runs, or by test share.
//
// Route: #/packages[/size]?color=group|zone|tests&all=1
//
//   X.pkgTiles.tile(pkg, opts) -> HTML of a package tile (the package page
//                                 shows neighbours with it)
//   X.pkgTiles.groupHue(groupId), X.pkgTiles.zoneShares(pkg)
(function () {
  "use strict";
  var U = X.util, D = X.data, R = X.refs, esc = U.esc;

  // ----- colours

  var groupIndex = new Map();
  (D.model.groups || []).forEach(function (g, i) { groupIndex.set(g.id, i); });
  // Groups get evenly spaced, muted hues: identity is carried by the
  // group's name on every tile and in the legend; the strong colours stay
  // with the zones.
  function groupHue(id) {
    var n = Math.max(1, groupIndex.size), i = groupIndex.has(id) ? groupIndex.get(id) : -1;
    return i < 0 ? -1 : Math.round((i * 360 / n + 25) % 360);
  }
  // zoneShares: per zone, the largest share of any of its actors.
  function zoneShares(p) {
    var a = p.actors || {}, m = new Map();
    Object.keys(a).forEach(function (id) {
      if (!(a[id] > 0)) return;
      var z = D.zoneOfActor(id);
      if (!z) return;
      m.set(z.id, Math.max(m.get(z.id) || 0, a[id]));
    });
    var order = (D.model.zones || []).map(function (z) { return z.id; });
    return Array.from(m.keys()).sort(function (x, y) { return order.indexOf(x) - order.indexOf(y); })
      .map(function (id) { return { zone: D.zone(id), share: m.get(id) }; });
  }
  function testShare(p) {
    var t = p.testLines.code, c = p.lines.code;
    return t + c ? t / (t + c) : 0;
  }
  var maxLines = Math.max.apply(null, D.core.packages.map(function (p) { return p.lines.code; }).concat([1]));

  function tileStyle(p, color) {
    var st = [];
    var gh = groupHue(p.group);
    if (gh >= 0) st.push("--gh:" + gh);
    if (color === "tests") st.push("--ts:" + testShare(p).toFixed(3));
    if (color === "zone") {
      var zs = zoneShares(p);
      if (zs.length) {
        var top = zs.slice().sort(function (a, b) { return b.share - a.share; })[0];
        st.push("--z:var(--z-" + top.zone.trust + ")");
      }
    }
    return st.join(";");
  }
  function strip(p, color) {
    if (color === "zone") {
      var zs = zoneShares(p);
      if (!zs.length) return '<span class="pt-strip none"></span>';
      return '<span class="pt-strip zones">' + zs.map(function (z) {
        return '<b class="z-' + esc(z.zone.trust) + '" style="flex:' + Math.max(0.15, z.share).toFixed(3) + '" title="' + esc(z.zone.title + ": up to " + U.pct(z.share)) + '"></b>';
      }).join("") + "</span>";
    }
    return '<span class="pt-strip"></span>';
  }

  function tile(p, opts) {
    opts = opts || {};
    var color = opts.color || "group";
    var G = D.group(p.group);
    var dir = U.dirOf(p.path), name = U.lastElem(p.path);
    var zs = zoneShares(p);
    var lines = p.lines.code + (p.testOnly ? p.testLines.code : 0);
    var foot = color === "tests" ? U.pct(testShare(p)) + " tests" : (G ? G.title : "");
    return '<div class="ptile c-' + color + (p.testOnly ? " testonly" : "") + '" data-pkg="' + esc(p.path) + '" style="' + tileStyle(p, color) + '">' +
      strip(p, color) +
      '<div class="pt-head"><a class="pt-name mono" data-ref="pkg:' + esc(p.path) + '" href="' + esc(X.router.hrefKeep("pkg", p.path)) + '">' +
      (dir ? '<span class="pt-dir">' + esc(dir) + "/</span>" : "") + esc(name) + "</a>" +
      '<span class="pt-zones">' + zs.map(function (z) { return R.zoneDot(z.zone, z.zone.title + ": up to " + U.pct(z.share) + " of its code"); }).join("") + "</span></div>" +
      '<div class="pt-role">' + esc(p.role || U.firstSentence(p.doc) || "") + "</div>" +
      '<div class="pt-foot"><span class="pt-bar"><b style="width:' + Math.max(2, 100 * lines / maxLines).toFixed(1) + '%"></b></span>' +
      '<span class="pt-n num">' + U.fmt(lines) + '</span><span class="pt-g">' + esc(foot) + "</span></div></div>";
  }

  X.pkgTiles = { tile: tile, groupHue: groupHue, zoneShares: zoneShares, testShare: testShare };

  // ----- the view

  var S = null;

  function toolbar(r) {
    var color = r.params.color || "group", isSize = r.rest === "size";
    function cl(c, t) { return '<a href="' + esc(X.router.hrefKeep("packages", r.rest, merge(r.params, { color: c === "group" ? null : c }))) + '"' + (color === c ? ' class="on"' : "") + ">" + t + "</a>"; }
    return '<div class="pk-bar">' +
      '<div class="seg">' +
      '<a href="' + esc(X.router.hrefKeep("packages", "", { color: r.params.color })) + '"' + (!isSize ? ' class="on"' : "") + ">Layers</a>" +
      '<a href="' + esc(X.router.hrefKeep("packages", "size", { color: r.params.color })) + '"' + (isSize ? ' class="on"' : "") + ">Size</a></div>" +
      '<span class="pk-l muted small">Colour by</span><div class="seg">' + cl("group", "Group") + cl("zone", "Where it runs") + cl("tests", "Test share") + "</div>" +
      (!isSize ? '<label class="check"><input type="checkbox" data-a="all"' + (r.params.all ? " checked" : "") + "> all imports</label>" : "") +
      '<div class="pk-focus"></div>' +
      '<div class="pk-legend">' + legend(color) + "</div></div>";
  }
  function merge(a, b) { var o = {}; Object.keys(a).forEach(function (k) { if (k !== "i") o[k] = a[k]; }); Object.keys(b).forEach(function (k) { o[k] = b[k]; }); return o; }

  function legend(color) {
    if (color === "group") {
      var used = new Set(D.core.packages.map(function (p) { return p.group; }));
      return (D.model.groups || []).filter(function (g) { return used.has(g.id); }).map(function (g) {
        return '<span class="lg" data-group="' + esc(g.id) + '" style="--gh:' + groupHue(g.id) + '"><i class="lg-sw g"></i>' + esc(g.title) + "</span>";
      }).join("");
    }
    if (color === "zone") {
      var zs = new Map();
      D.core.packages.forEach(function (p) { zoneShares(p).forEach(function (z) { zs.set(z.zone.id, z.zone); }); });
      var order = (D.model.zones || []).map(function (z) { return z.id; });
      return Array.from(zs.values()).sort(function (a, b) { return order.indexOf(a.id) - order.indexOf(b.id); }).map(function (z) {
        return '<span class="lg" data-zone="' + esc(z.id) + '" title="' + esc(z.summary || "") + '">' + R.zoneDot(z) + esc(z.title) + "</span>";
      }).join("") + '<span class="lg muted" title="' + esc((D.core.reach && D.core.reach.note) || "") + '">' + U.icon("info") + "strip width: share of the code that may run there</span>";
    }
    var max = Math.max.apply(null, D.core.packages.map(testShare));
    return '<span class="lg">0%</span><span class="lg-ramp"></span><span class="lg">' + U.pct(max) + " of a package's lines are tests</span>";
  }

  function render(el, r) {
    S = { el: el, r: r, layer: null, hover: null };
    el.innerHTML = '<div class="pk">' + toolbar(r) + '<div class="pk-body"></div></div>';
    var body = el.querySelector(".pk-body");
    el.querySelector(".pk-bar").addEventListener("change", function (ev) {
      if (ev.target.getAttribute("data-a") === "all") X.router.setParams({ all: ev.target.checked ? "1" : null });
    });
    el.querySelector(".pk-legend").addEventListener("mouseover", function (ev) {
      var g = ev.target.closest("[data-group],[data-zone]");
      if (!S.map) return;
      S.map.querySelectorAll(".ptile, .tm-pkg").forEach(function (t) { t.classList.remove("lg-lit"); });
      S.el.querySelector(".pk").classList.toggle("lg-focus", !!g);
      if (!g) return;
      var gid = g.getAttribute("data-group"), zid = g.getAttribute("data-zone");
      S.map.querySelectorAll(".ptile, .tm-pkg").forEach(function (t) {
        var p = D.pkg(t.getAttribute("data-pkg"));
        var hit = gid ? p.group === gid : zoneShares(p).some(function (z) { return z.zone.id === zid; });
        t.classList.toggle("lg-lit", hit);
      });
    });
    el.querySelector(".pk-legend").addEventListener("mouseleave", function () {
      S.el.querySelector(".pk").classList.remove("lg-focus");
    });
    if (r.rest === "size") renderSize(body, r);
    else renderLayers(body, r);
  }

  // ----- layers

  function renderLayers(body, r) {
    var color = r.params.color || "group";
    var layers = (D.model.layers || []).slice().reverse();
    var byLayer = U.groupBy(D.core.packages, function (p) { return p.layer || ""; });
    var html = '<div class="layers">';
    var rows = layers.map(function (L) { return { L: L, pkgs: byLayer.get(L.id) || [] }; });
    if (byLayer.get("")) rows.push({ L: { id: "", title: "No layer", summary: "Packages without an entry in the model." }, pkgs: byLayer.get("") });
    rows.forEach(function (row) {
      var pk = U.sortBy(row.pkgs, function (p) {
        var gi = groupIndex.has(p.group) ? groupIndex.get(p.group) : 99;
        return ("00" + gi).slice(-3) + "\u0000" + p.path;
      });
      html += '<section class="layer' + (r.params.layer === row.L.id ? " lit" : "") + '" data-layer="' + esc(row.L.id) + '">' +
        '<header class="layer-h"><h2 data-ref="layer:' + esc(row.L.id) + '">' + esc(row.L.title) + '</h2><p title="' + esc(row.L.summary || "") + '">' + esc(row.L.summary || "") + "</p>" +
        '<span class="muted small">' + U.plural(pk.length, "package") + "</span></header>" +
        '<div class="layer-tiles">' + pk.map(function (p) { return tile(p, { color: color }); }).join("") + "</div></section>";
    });
    html += "</div>";
    body.innerHTML = html;
    var map = body.querySelector(".layers");
    S.map = map;
    S.layer = X.connectors.create(map);
    S.tiles = new Map();
    map.querySelectorAll(".ptile").forEach(function (t) { S.tiles.set(t.getAttribute("data-pkg"), t); });
    if (r.params.group) map.querySelectorAll(".ptile").forEach(function (t) { t.classList.toggle("lg-lit", D.pkg(t.getAttribute("data-pkg")).group === r.params.group); });
    if (r.params.group) map.classList.add("lg-focus");
    map.addEventListener("mouseover", function (ev) {
      var t = ev.target.closest(".ptile");
      var p = t ? t.getAttribute("data-pkg") : null;
      if (p !== S.hover) { S.hover = p; focus(); }
    });
    map.addEventListener("mouseleave", function () { S.hover = null; focus(); });
    map.addEventListener("click", function (ev) {
      if (ev.defaultPrevented) return;
      var t = ev.target.closest(".ptile");
      if (!t || ev.target.closest("a[data-ref], [data-ref]")) return;
      var p = t.getAttribute("data-pkg");
      if (U.isModifiedClick(ev)) X.router.go(X.router.hrefKeep("pkg", p));
      else X.inspector.open("pkg:" + p);
    });
    focus();
  }

  function selectedPkg() {
    var i = X.router.current && X.router.current.params.i;
    return i && i.indexOf("pkg:") === 0 ? i.slice(4) : null;
  }

  // focus draws the edges of the hovered (else the selected) package, or
  // all edges when asked, and dims the rest.
  function focus() {
    if (!S || !S.layer) return;
    var r = S.r, all = !!r.params.all, p = S.hover || selectedPkg();
    var pk = p ? D.pkg(p) : null;
    var edges = [];
    S.map.classList.toggle("focus", !!pk);
    S.tiles.forEach(function (t) { t.classList.remove("lit", "me"); });
    if (all) {
      D.core.packages.forEach(function (q) {
        (q.imports || []).forEach(function (to) {
          var a = S.tiles.get(q.path), b = S.tiles.get(to);
          if (!a || !b) return;
          var cls = "all";
          if (pk) cls += (q.path === p) ? " imp" : (to === p) ? " by" : " faint";
          edges.push({ from: a, to: b, cls: cls, arrow: !!pk && (q.path === p || to === p), sides: "auto" });
        });
      });
    }
    if (pk) {
      var me = S.tiles.get(p);
      if (me) me.classList.add("me");
      (pk.imports || []).forEach(function (to) {
        var b = S.tiles.get(to);
        if (!b) return;
        b.classList.add("lit");
        if (!all) edges.push({ from: me, to: b, cls: "imp", arrow: true });
      });
      (pk.importedBy || []).forEach(function (from) {
        var a = S.tiles.get(from);
        if (!a) return;
        a.classList.add("lit");
        if (!all) edges.push({ from: a, to: me, cls: "by", arrow: true });
      });
    }
    S.layer.set(edges);
    var info = S.el.querySelector(".pk-focus");
    info.innerHTML = pk ? '<b class="mono">' + esc(pk.path) + '</b><span class="lgx imp"></span>imports ' + (pk.imports || []).length +
      ' <span class="lgx by"></span>imported by ' + (pk.importedBy || []).length : "";
  }

  function update(r, prev) {
    if (!S) return false;
    var same = r.rest === S.r.rest && (r.params.color || "") === (S.r.params.color || "") && (r.params.all || "") === (S.r.params.all || "") &&
      (r.params.group || "") === (S.r.params.group || "") && (r.params.layer || "") === (S.r.params.layer || "");
    if (!same) return false;
    S.r = r;
    focus();
    return true;
  }

  // The selection follows the inspector, which only changes the parameter
  // "i": redraw then.
  X.router.onChange(function (r) { if (S && r.view === "packages" && S.layer) focus(); });

  // ----- size (treemap)

  function renderSize(body, r) {
    var color = r.params.color || "group";
    var showTests = X.settings.get("tests");
    var box = U.h("div.treemap");
    body.appendChild(box);
    S.map = box;
    var note = U.h("div.tm-note.muted.small", showTests ? "Area: lines of code, tests included (hatched). Hide tests in the settings to see the code alone." : "Area: lines of code, tests left out.");
    body.insertBefore(note, box);
    function draw() {
      var W = box.clientWidth, H = box.clientHeight;
      if (W < 50 || H < 50) return;
      var pkgs = D.core.packages.map(function (p) {
        var files = p.files.map(function (f) { return D.file(f); }).filter(function (f) { return f && (showTests || !f.test); })
          .map(function (f) { return { f: f, v: f.lines.code }; }).filter(function (x) { return x.v > 0; });
        return { p: p, files: files, v: files.reduce(function (a, x) { return a + x.v; }, 0) };
      }).filter(function (x) { return x.v > 0; });
      var rects = squarify(pkgs, 0, 0, W, H);
      var html = "";
      rects.forEach(function (pr) {
        var p = pr.item.p, hdr = pr.h > 34 && pr.w > 60 ? 18 : 0;
        html += '<div class="tm-pkg c-' + color + '" data-pkg="' + esc(p.path) + '" style="left:' + pr.x + "px;top:" + pr.y + "px;width:" + pr.w + "px;height:" + pr.h + "px;" + tileStyle(p, color) + '">';
        if (hdr) html += '<a class="tm-h mono" data-ref="pkg:' + esc(p.path) + '" href="' + esc(X.router.hrefKeep("pkg", p.path)) + '">' + esc(p.path) + ' <span class="num">' + U.fmt(pr.item.v) + "</span></a>";
        var inner = squarify(pr.item.files, 2, hdr + 1, pr.w - 4, pr.h - hdr - 3);
        inner.forEach(function (fr) {
          var f = fr.item.f, lab = fr.w > 44 && fr.h > 15;
          html += '<a class="tm-f' + (f.test ? " test" : "") + '" data-ref="file:' + esc(f.path) + '" data-act="full" href="' + esc(X.router.hrefKeep("code", f.path)) + '" style="left:' + fr.x + "px;top:" + fr.y + "px;width:" + fr.w + "px;height:" + fr.h + 'px">' +
            (lab ? '<span class="mono">' + esc(U.lastElem(f.path)) + "</span>" : "") + "</a>";
        });
        html += "</div>";
      });
      box.innerHTML = html;
    }
    draw();
    var ro = window.ResizeObserver ? new ResizeObserver(U.debounce(draw, 60)) : null;
    if (ro) ro.observe(box);
    S.cleanup = function () { if (ro) ro.disconnect(); };
  }

  // squarify lays items ({v}) into a rectangle (Bruls, Huizing, van Wijk).
  function squarify(items, x, y, w, h) {
    var out = [];
    var list = items.filter(function (i) { return i.v > 0; }).sort(function (a, b) { return b.v - a.v; });
    var total = list.reduce(function (a, i) { return a + i.v; }, 0);
    if (!total || w <= 0 || h <= 0) return out;
    var scale = (w * h) / total;
    var rest = list.map(function (i) { return { item: i, a: i.v * scale }; });
    var rx = x, ry = y, rw = w, rh = h;
    while (rest.length) {
      var side = Math.min(rw, rh), row = [], worstPrev = Infinity;
      while (rest.length) {
        var cand = row.concat([rest[0]]);
        var wv = worst(cand, side);
        if (row.length && wv > worstPrev) break;
        row = cand; worstPrev = wv; rest.shift();
      }
      var sum = row.reduce(function (a, r) { return a + r.a; }, 0);
      var thick = sum / side, off = 0;
      row.forEach(function (r2) {
        var len = r2.a / thick;
        if (rw >= rh) out.push({ item: r2.item, x: round(rx), y: round(ry + off), w: round(thick), h: round(len) });
        else out.push({ item: r2.item, x: round(rx + off), y: round(ry), w: round(len), h: round(thick) });
        off += len;
      });
      if (rw >= rh) { rx += thick; rw -= thick; } else { ry += thick; rh -= thick; }
    }
    // Leave a hairline between neighbours.
    out.forEach(function (o) { o.w = Math.max(0, o.w - 2); o.h = Math.max(0, o.h - 2); });
    return out;
  }
  function worst(row, side) {
    var s = 0, mx = 0, mn = Infinity;
    row.forEach(function (r) { s += r.a; mx = Math.max(mx, r.a); mn = Math.min(mn, r.a); });
    var s2 = side * side;
    return Math.max(s2 * mx / (s * s), (s * s) / (s2 * mn));
  }
  function round(v) { return Math.round(v); }

  X.router.register("packages", {
    nav: "packages",
    title: function (r) { return r.rest === "size" ? "Package sizes" : "Packages"; },
    render: render,
    update: update,
    unmount: function () { if (S) { if (S.layer) S.layer.destroy(); if (S.cleanup) S.cleanup(); } S = null; }
  });
})();
