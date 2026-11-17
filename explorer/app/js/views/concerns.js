// The Concerns view. Index (#/concerns): the concerns in the model's order
// as cards. Page (#/concerns/<id>?diagram=<id>&h=<anchor>): the text in
// the main column, diagrams where the text names them ([[diagram:id]]) and
// the others after the first section, the invariants with the code that
// enforces them; beside it, in view while reading, the packages and files
// that belong to the concern (a small layer picture with them lit), key
// symbols, flows, related concerns, spec sections and tests.
// Also the inspector's view of a concern.
(function () {
  "use strict";
  var U = X.util, D = X.data, M = D.model, R = X.refs, esc = U.esc;

  // packagesOf: the packages a concern has, whole or by some files.
  function packagesOf(c) {
    var m = new Map();
    (c.packages || []).forEach(function (p) { m.set(p, { path: p, whole: true, files: [] }); });
    (c.files || []).forEach(function (f) {
      var fo = D.file(f), p = fo && fo.package ? fo.package : U.dirOf(f);
      if (!m.has(p)) m.set(p, { path: p, whole: false, files: [] });
      m.get(p).files.push(f);
    });
    return Array.from(m.values());
  }

  // layerPicture: every package as a small square, layer by layer (top
  // first), the concern's lit (half lit when only some files belong).
  function layerPicture(c) {
    var mine = new Map();
    packagesOf(c).forEach(function (p) { mine.set(p.path, p); });
    var layers = (M.layers || []).slice().reverse();
    var by = U.groupBy(D.core.packages.filter(function (p) { return !p.xtest; }), function (p) { return p.layer || ""; });
    var h = '<div class="lp">';
    layers.forEach(function (L) {
      var list = U.sortBy(by.get(L.id) || [], function (p) { return p.group + "\u0000" + p.path; });
      if (!list.length) return;
      h += '<div class="lp-row"><span class="lp-l" title="' + esc(L.title) + '">' + esc(L.title) + '</span><span class="lp-sq">' + list.map(function (p) {
        var m = mine.get(p.path), gh = X.pkgTiles ? X.pkgTiles.groupHue(p.group) : -1;
        return '<a class="lp-p' + (m ? (m.whole ? " on" : " part") : "") + '" data-ref="pkg:' + esc(p.path) + '" href="' + esc(X.router.hrefKeep("pkg", p.path)) + '"' + (gh >= 0 ? ' style="--gh:' + gh + '"' : "") + "></a>";
      }).join("") + "</span></div>";
    });
    return h + "</div>";
  }

  function firstDoc(ref) {
    var s = D.sym(ref);
    return s && s.doc ? '<div class="sp-first">' + X.doc.inline(U.firstSentence(s.doc), { pkg: s.package }) + "</div>" : "";
  }
  function side(title, n, body, cls) {
    return '<section class="cp-s' + (cls ? " " + cls : "") + '"><h3 class="sp-label">' + esc(title) + (n != null ? ' <span class="count">' + n + "</span>" : "") + "</h3>" + body + "</section>";
  }

  // ----- the page

  var S = null;

  function page(el, r, c) {
    var flows = X.model.flowsOf("concern", c.id);
    var pk = packagesOf(c);
    var inv = c.invariants || [];
    var bodyHTML = X.md.render(c.body || "", { headingTag: 1 });
    el.innerHTML = '<div class="cp">' +
      '<article class="cp-main">' +
      '<header class="cp-head"><a class="cp-crumb" href="#/concerns">' + U.icon("concern") + "Concerns</a>" +
      "<h1>" + esc(c.title) + "</h1>" +
      (c.summary ? '<p class="lead">' + X.md.inline(c.summary, {}) + "</p>" : "") +
      '<div class="cp-meta muted small">' + [U.plural(pk.length, "package"), U.plural(flows.length, "flow"), U.plural(inv.length, "invariant"), U.plural((c.diagrams || []).length, "diagram")].join(" · ") +
      ' · <a href="' + esc(X.router.hrefKeep("runtime", "", { concern: c.id })) + '">on the runtime map</a></div></header>' +
      '<div class="md cp-body">' + bodyHTML + "</div>" +
      (inv.length ? '<section class="cp-inv md"><h2 class="md-h" data-anchor="invariants">Invariants</h2><ol class="inv-list">' + inv.map(function (x) {
        return '<li class="inv"><div class="inv-t">' + X.md.inline(x.text, {}) + "</div>" +
          ((x.refs || []).length ? '<div class="inv-r"><span class="muted small">enforced in</span>' + x.refs.map(function (r2) { return R.chip(r2, { cls: "small" }); }).join("") + "</div>" : "") +
          ((x.tests || []).length ? '<div class="inv-r"><span class="muted small">shown by</span>' + x.tests.map(function (r2) { return R.chip(r2, { cls: "small" }); }).join("") + "</div>" : "") + "</li>";
      }).join("") + "</ol></section>" : "") +
      "</article>" +
      '<aside class="cp-side"><div class="cp-side-in">' + sideHTML(c, pk, flows) + "</div></aside></div>";
    S = { el: el, c: c, graphs: [] };
    S.spy = U.debounce(spy, 40);
    el.addEventListener("scroll", S.spy, { passive: true });
    placeDiagrams(el.querySelector(".cp-body"), c);
    el.querySelector(".cp-body").addEventListener("click", onBodyClick);
    el.querySelector(".cp-side").addEventListener("click", onBodyClick);
    focus(r);
    spy();
  }

  // sideHTML: the side column, in the order a reader needs it while
  // reading: where they are in the page, the flows to play, the code.
  function sideHTML(c, pk, flows) {
    var h = "";
    var heads = X.md.headings(c.body || "").filter(function (x) { return x.level <= 2; });
    if (heads.length > 2) h += side("On this page", null, '<ul class="cp-toc">' + heads.map(function (x) {
      return '<li><a href="#" data-goto="' + esc(x.anchor) + '">' + esc(X.md.plainText(x.text)) + "</a></li>";
    }).join("") + ((c.invariants || []).length ? '<li><a href="#" data-goto="invariants">Invariants <span class="count">' + c.invariants.length + "</span></a></li>" : "") + "</ul>", "toc");
    if (flows.length) h += side("Flows", flows.length, '<div class="chips">' + flows.map(function (f) { return R.chip("flow:" + f.id, { act: "full" }); }).join("") + "</div>");
    h += side("Code", pk.length, layerPicture(c) + '<ul class="cp-pk">' + pk.map(function (p) {
      return "<li>" + R.chip("pkg:" + p.path, { cls: "small" }) + (p.whole ? "" : '<div class="cp-files">' + p.files.map(function (f) { return R.chip("file:" + f, { cls: "small flat" }); }).join("") + "</div>") + "</li>";
    }).join("") + "</ul>");
    if ((c.key || []).length) h += side("Key symbols", c.key.length, '<ul class="cp-key">' + c.key.map(function (k) { return "<li>" + R.chip(k, { cls: "small" }) + firstDoc(k) + "</li>"; }).join("") + "</ul>");
    if ((c.invariants || []).length && heads.length <= 2) h += side("Invariants", c.invariants.length, '<a class="small" href="#" data-goto="invariants">' + U.plural(c.invariants.length, "statement") + " with the code that enforces it</a>");
    var related = (c.related || []).slice();
    (M.concerns || []).forEach(function (o) { if (o.id !== c.id && (o.related || []).indexOf(c.id) >= 0 && related.indexOf(o.id) < 0) related.push(o.id); });
    if (related.length) h += side("Related", related.length, '<div class="chips">' + related.map(function (x) { return R.chip("concern:" + x, { act: "full" }); }).join("") + "</div>");
    if ((c.spec || []).length) h += side("Spec", c.spec.length, '<div class="chips">' + c.spec.map(function (x) { return R.chip(x, { cls: "small" }); }).join("") + "</div>");
    if ((c.tests || []).length) h += side("Tests", c.tests.length, '<div class="chips">' + c.tests.map(function (x) { return R.chip(x, { cls: "small" }); }).join("") + "</div>");
    return h;
  }

  // spy marks, in "On this page", the section at the top of the view.
  function spy() {
    if (!S) return;
    var toc = S.el.querySelector(".cp-toc");
    if (!toc) return;
    var top = S.el.getBoundingClientRect().top + 90, cur = null;
    S.el.querySelectorAll(".cp-body > h2[data-anchor], .cp-inv > h2[data-anchor]").forEach(function (h) {
      if (h.getBoundingClientRect().top <= top) cur = h.getAttribute("data-anchor");
    });
    toc.querySelectorAll("a[data-goto]").forEach(function (a, i) {
      a.classList.toggle("cur", cur ? a.getAttribute("data-goto") === cur : i === 0);
    });
  }

  // placeDiagrams puts each of the concern's diagrams after the block of
  // the text that first names it; the others after the first section.
  function placeDiagrams(body, c) {
    var placed = new Set();
    var ds = c.diagrams || [];
    function topBlock(node) { while (node && node.parentNode !== body) node = node.parentNode; return node; }
    body.querySelectorAll('[data-ref^="diagram:"]').forEach(function (chip) {
      var info = R.resolve(chip.getAttribute("data-ref"));
      var key = info.resolved ? info.resolved.key : "";
      var parts = key.split("/");
      if (parts[0] !== c.id) return;
      var d = ds.filter(function (x) { return x.id === parts[1]; })[0];
      if (!d || placed.has(d.id)) return;
      var blk = topBlock(chip);
      if (!blk) return;
      var holder = U.h("div.cp-dg");
      blk.parentNode.insertBefore(holder, blk.nextSibling);
      S.graphs.push(X.graph.render(holder, d, { concern: c.id }));
      placed.add(d.id);
    });
    var rest = ds.filter(function (d) { return !placed.has(d.id); });
    if (!rest.length) return;
    // After the first section: before the second "##", else after the
    // first two blocks, else at the end.
    var h2s = body.querySelectorAll(":scope > h2");
    var before = h2s.length >= 2 ? h2s[1] : body.children[2] || null;
    rest.forEach(function (d) {
      var holder = U.h("div.cp-dg");
      body.insertBefore(holder, before);
      S.graphs.push(X.graph.render(holder, d, { concern: c.id }));
    });
  }

  function onBodyClick(ev) {
    var g = ev.target.closest("[data-goto]");
    if (g) { ev.preventDefault(); X.router.setParams({ h: g.getAttribute("data-goto"), diagram: null }); return; }
    var a = ev.target.closest(".md-anchor");
    if (a) { ev.preventDefault(); X.router.setParams({ h: a.getAttribute("data-anchor-link"), diagram: null }); return; }
    var chip = ev.target.closest('[data-ref^="diagram:"]');
    if (chip && !U.isModifiedClick(ev)) {
      var info = R.resolve(chip.getAttribute("data-ref")), key = info.resolved ? info.resolved.key : "";
      if (key.split("/")[0] === S.c.id) { ev.preventDefault(); X.router.setParams({ diagram: key.split("/")[1], h: null }); }
    }
  }

  function focus(r) {
    var target = null;
    if (r.params.diagram) {
      target = S.el.querySelector('.gd-fig[data-diagram="' + cssEsc(S.c.id + "/" + r.params.diagram) + '"]');
      if (target) { target.classList.add("flash"); setTimeout(function () { target.classList.remove("flash"); }, 1200); }
    } else if (r.params.h) target = S.el.querySelector('[data-anchor="' + cssEsc(r.params.h) + '"]');
    if (target) target.scrollIntoView({ block: "start" });
  }
  function cssEsc(s) { return window.CSS && CSS.escape ? CSS.escape(s) : s; }

  // ----- the index

  function index(el) {
    var list = M.concerns || [];
    var h = '<div class="page ci"><h1>Concerns</h1><p class="lead">What the system does, concern by concern: an explanation, its diagrams, the invariants with the code that enforces them, the key symbols, the flows and the tests.</p>';
    if (!list.length) h += '<p class="muted">The model has no concerns yet.</p>';
    h += '<div class="ci-grid">' + list.map(function (c, i) {
      var pk = packagesOf(c), flows = X.model.flowsOf("concern", c.id);
      return '<a class="ci-card" href="' + esc(X.router.href("concerns", c.id)) + '" data-ref="concern:' + esc(c.id) + '" data-act="full" data-nocard>' +
        '<div class="ci-top"><span class="ci-n num">' + (i + 1) + '</span><span class="ci-title">' + esc(c.title) + "</span></div>" +
        (c.summary ? '<p class="ci-sum">' + X.doc.plain(X.md.plainText(c.summary)) + "</p>" : "") +
        '<div class="ci-pk">' + pk.slice(0, 10).map(function (p) {
          var pp = D.pkg(p.path), gh = pp && X.pkgTiles ? X.pkgTiles.groupHue(pp.group) : -1;
          return '<span class="ci-tile' + (p.whole ? "" : " part") + '"' + (gh >= 0 ? ' style="--gh:' + gh + '"' : "") + ' title="' + esc(p.path + (p.whole ? "" : ": " + U.plural(p.files.length, "file"))) + '">' + esc(U.lastElem(p.path)) + "</span>";
        }).join("") + (pk.length > 10 ? '<span class="muted small">+' + (pk.length - 10) + "</span>" : "") + "</div>" +
        '<div class="ci-foot muted small">' + U.plural(flows.length, "flow") + " · " + U.plural((c.invariants || []).length, "invariant") + ((c.diagrams || []).length ? " · " + U.plural(c.diagrams.length, "diagram") : "") + "</div></a>";
    }).join("") + "</div></div>";
    el.innerHTML = h;
  }

  function cleanup() {
    if (S) { S.graphs.forEach(function (g) { g.destroy(); }); S.el.removeEventListener("scroll", S.spy); }
    S = null;
  }

  X.router.register("concerns", {
    nav: "concerns",
    title: function (r) { var c = D.concern(r.rest); return c ? c.title : "Concerns"; },
    render: function (el, r) {
      cleanup();
      if (!r.rest) { index(el); return; }
      var c = D.concern(r.rest);
      if (!c) { el.innerHTML = '<div class="page narrow"><h1>No such concern</h1><p class="muted"><code>' + esc(r.rest) + '</code> is not a concern of the model. See the <a href="#/concerns">concerns</a>.</p></div>'; return; }
      page(el, r, c);
    },
    update: function (r) {
      if (!S || r.rest !== S.c.id) return false;
      focus(r);
      return true;
    },
    unmount: cleanup
  });

  // ----- references, the inspector, search

  R.define("concern", {
    card: function (info) {
      var c = info.obj;
      return (c.summary ? '<div class="hc-doc">' + X.md.cardText(c.summary) + "</div>" : "") +
        '<div class="hc-row muted">' + U.plural(packagesOf(c).length, "package") + " · " + U.plural(X.model.flowsOf("concern", c.id).length, "flow") + " · " + U.plural((c.invariants || []).length, "invariant") + "</div>";
    }
  });
  R.define("diagram", {
    card: function (info) {
      var d = info.obj, key = info.resolved ? info.resolved.key : "";
      var c = D.concern(key.split("/")[0]);
      return '<div class="hc-row muted">' + U.plural((d.nodes || []).length, "node") + " · " + U.plural((d.edges || []).length, "edge") + (c ? " · in " + esc(c.title) : "") + "</div>";
    }
  });
  X.inspector.register("concern", function (info, body) {
    var c = info.obj, pk = packagesOf(c), flows = X.model.flowsOf("concern", c.id);
    var h = (c.summary ? '<div class="md ins-lead">' + X.md.render(c.summary, {}) + "</div>" : "") +
      '<p><a class="btn" href="' + esc(X.router.href("concerns", c.id)) + '">' + U.icon("full") + "Read the concern</a></p>" +
      side("Code", pk.length, layerPicture(c) + '<div class="chips">' + pk.map(function (p) { return R.chip("pkg:" + p.path, { cls: "small" }); }).join("") + "</div>") +
      ((c.key || []).length ? side("Key symbols", c.key.length, '<ul class="cp-key">' + c.key.map(function (k) { return "<li>" + R.chip(k, { cls: "small" }) + firstDoc(k) + "</li>"; }).join("") + "</ul>") : "") +
      ((c.invariants || []).length ? side("Invariants", c.invariants.length, '<ol class="inv-list small">' + c.invariants.map(function (x) { return '<li class="inv"><div class="inv-t">' + X.md.inline(x.text, {}) + '</div><div class="inv-r">' + (x.refs || []).map(function (r2) { return R.chip(r2, { cls: "small" }); }).join("") + "</div></li>"; }).join("") + "</ol>") : "") +
      (flows.length ? side("Flows", flows.length, '<div class="chips">' + flows.map(function (f) { return R.chip("flow:" + f.id); }).join("") + "</div>") : "");
    body.appendChild(U.h("div.insp-pad.sp.cp-insp", { html: h }));
  });
  X.inspector.register("diagram", function (info, body) {
    var key = info.resolved ? info.resolved.key : "", c = D.concern(key.split("/")[0]);
    var el = U.h("div.insp-pad");
    body.appendChild(el);
    if (c) el.insertAdjacentHTML("beforeend", '<p class="muted small">In ' + R.chip("concern:" + c.id) + "</p>");
    var g = X.graph.render(el, info.obj, { concern: c ? c.id : "" });
    return function () { g.destroy(); };
  });
  X.search.addSource({
    kind: "concern", title: "Concerns", order: 2,
    items: function () {
      return (M.concerns || []).map(function (c) { return { ref: "concern:" + c.id, name: c.title, keys: [c.title, c.id], detail: X.md.plainText(U.firstSentence(c.summary || "")), exported: true, weight: 80 }; });
    }
  });
})();
