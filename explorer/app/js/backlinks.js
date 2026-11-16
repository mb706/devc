// Backlinks: for every reference (symbols, files, packages, and model
// objects too), the places in the model that mention it: a concern's
// packages, files, key symbols, invariants, tests and text; a flow's steps,
// their checks and text; glossary terms; layout entries and mounts;
// diagrams; actors' entry points; channels' checks; the tables of entry
// points. Computed once from the data.
//
//   X.backlinks.of(ref)           -> [{owner, role, detail, href, rank}]
//                                    owner: the model object's reference
//                                    (or "" with label/href for layout)
//   X.backlinks.inPackage(path)   -> {concerns: [{id, whole}], symbols:
//                                    [{ref, links}], files: [...]}
//   X.backlinks.section(ref)      -> HTML of "In the model" ("" if none)
//   X.backlinks.packageSection(path) -> the same for a package page
//
// It also puts marks in the source view's margin at declarations that are
// a flow's step, a concern's key symbol or where an invariant is enforced.
X.backlinks = (function () {
  "use strict";
  var U = X.util, D = X.data, M = D.model, R = X.refs, esc = U.esc;
  var index = new Map();

  function norm(ref) {
    if (!ref) return "";
    ref = String(ref).trim();
    if (ref.indexOf("file:") === 0) return ref.split("~")[0];
    return ref;
  }
  function add(ref, e) {
    var k = norm(ref);
    if (!k) return;
    if (!index.has(k)) index.set(k, []);
    index.get(k).push(e);
  }
  function addText(text, owner, role, extra) {
    X.model.refsIn(text).forEach(function (r) {
      var e = { owner: owner, role: role, rank: 6 };
      if (extra) Object.keys(extra).forEach(function (k) { e[k] = extra[k]; });
      add(r, e);
    });
  }

  // Ranks order the roles: what defines the place of a symbol first.
  (M.concerns || []).forEach(function (c) {
    var o = "concern:" + c.id;
    (c.packages || []).forEach(function (p) { add("pkg:" + p, { owner: o, role: "belongs to it", rank: 1 }); });
    (c.files || []).forEach(function (f) { add("file:" + f, { owner: o, role: "belongs to it", rank: 1 }); });
    (c.key || []).forEach(function (k) { add(k, { owner: o, role: "key symbol", rank: 2, mark: "key" }); });
    (c.invariants || []).forEach(function (inv) {
      (inv.refs || []).forEach(function (r) { add(r, { owner: o, role: "enforces an invariant", detail: inv.text, rank: 2, mark: "inv" }); });
      (inv.tests || []).forEach(function (r) { add(r, { owner: o, role: "tests an invariant", detail: inv.text, rank: 4 }); });
    });
    (c.tests || []).forEach(function (t) { add(t, { owner: o, role: "test", rank: 4 }); });
    (c.spec || []).forEach(function (s) { add(s, { owner: o, role: "spec section", rank: 5 }); });
    (c.related || []).forEach(function (r) { add("concern:" + r, { owner: o, role: "related", rank: 5 }); });
    (c.diagrams || []).forEach(function (d) {
      (d.nodes || []).forEach(function (n) { if (n.ref) add(n.ref, { owner: o, role: "in a diagram", detail: d.title + ": " + n.label, rank: 3, href: R.href("diagram:" + c.id + "/" + d.id) }); });
      (d.edges || []).forEach(function (e) { if (e.ref) add(e.ref, { owner: o, role: "in a diagram", detail: d.title + ": " + (e.label || e.from + " → " + e.to), rank: 3, href: R.href("diagram:" + c.id + "/" + d.id) }); });
    });
    addText(c.body, o, "mentioned", null);
    addText(c.summary, o, "mentioned", null);
    (c.invariants || []).forEach(function (inv) { addText(inv.text, o, "mentioned in an invariant", { detail: inv.text }); });
  });

  (M.flows || []).forEach(function (f) {
    var o = "flow:" + f.id;
    if (f.concern) add("concern:" + f.concern, { owner: o, role: f.kind === "tour" ? "tour" : "flow", rank: 2 });
    addText(f.summary, o, "mentioned", null);
    X.model.steps(f).forEach(function (st) {
      var s = st.step, href = X.router.href("flows", f.id, { step: st.no });
      var label = "step " + st.no;
      if (s.ref) add(s.ref, { owner: o, role: label, detail: s.text, href: href, rank: 2, mark: "flow", no: st.no });
      (s.checks || []).forEach(function (r) { add(r, { owner: o, role: "checks at " + label, detail: s.text, href: href, rank: 3, mark: "flow", no: st.no }); });
      if (s.via && /^(rpc|http|event|cmd):/.test(s.via)) add(s.via, { owner: o, role: "message at " + label, detail: s.label || s.text, href: href, rank: 3, no: st.no });
      addText(s.text, o, "mentioned at " + label, { href: href, detail: s.text });
      addText(s.detail, o, "mentioned at " + label, { href: href, detail: s.text });
    });
  });

  (M.terms || []).forEach(function (t) {
    var o = "term:" + t.id;
    (t.refs || []).forEach(function (r) { add(r, { owner: o, role: "defines the term", rank: 3 }); });
    (t.see || []).forEach(function (s) { add("term:" + s, { owner: o, role: "see also", rank: 5 }); });
    addText(t.text, o, "mentioned", null);
  });

  (M.trees || []).forEach(function (t) {
    (t.entries || []).forEach(function (e) {
      var where = { owner: "", label: t.root + "/" + e.path, href: X.router.href("state", "", { sel: t.id + ":" + e.path }), rank: 3 };
      if (e.ref) add(e.ref, Object.assign({ role: "knows the path" }, where));
      addText(e.text, "", "mentioned", where);
    });
  });
  (M.mounts || []).forEach(function (m, i) {
    var where = { owner: "", label: m.source + " → " + m.target, href: X.router.href("state", "", { sel: "mount:" + i }), rank: 3 };
    if (m.ref) add(m.ref, Object.assign({ role: "mounts" }, where));
    addText(m.text, "", "mentioned", where);
  });

  (M.actors || []).forEach(function (a) {
    var o = "actor:" + a.id;
    (a.entry || []).forEach(function (r) { add(r, { owner: o, role: "entry point", rank: 2 }); });
    (a.commands || []).forEach(function (c) { add("cmd:" + c, { owner: o, role: "runs as this actor", rank: 2 }); });
    addText(a.summary, o, "mentioned", null);
  });
  (M.channels || []).forEach(function (c) {
    var o = "channel:" + c.id;
    (c.checks || []).forEach(function (r) { add(r, { owner: o, role: "checks what arrives", rank: 2, mark: "inv" }); });
    (c.concerns || []).forEach(function (r) { add("concern:" + r, { owner: o, role: "channel", rank: 4 }); });
    addText(c.carries, o, "mentioned", null);
    addText(c.transport, o, "mentioned", null);
  });
  (M.zones || []).forEach(function (z) { addText(z.summary, "zone:" + z.id, "mentioned", null); });
  (M.surfaces || []).forEach(function (s) {
    var o = "surface:" + s.id;
    (s.handlersIn || []).concat(s.listedIn || []).concat([s.table, s.root, s.consts]).forEach(function (r) {
      if (r) add(r, { owner: o, role: "defines the table", rank: 3 });
    });
    addText(s.summary, o, "mentioned", null);
  });
  addText(M.overview, "", "mentioned in the overview", { label: "Overview", href: X.router.href("overview"), rank: 6 });

  function of(ref) {
    var l = index.get(norm(ref)) || [];
    return l.slice().sort(function (a, b) { return a.rank - b.rank; });
  }

  // inPackage collects what the model says about a package and its parts.
  function inPackage(path) {
    var p = D.pkg(path);
    if (!p) return null;
    var concerns = new Map();
    (M.concerns || []).forEach(function (c) {
      if ((c.packages || []).indexOf(path) >= 0) concerns.set(c.id, { id: c.id, whole: true, files: [] });
      (c.files || []).forEach(function (f) {
        if (D.file(f) && D.file(f).package === path) {
          if (!concerns.has(c.id)) concerns.set(c.id, { id: c.id, whole: false, files: [] });
          concerns.get(c.id).files.push(f);
        }
      });
    });
    var symbols = D.symbolsOfPackage(path).map(function (s) { return { ref: s.ref, links: of(s.ref).filter(function (e) { return e.rank > 0; }) }; })
      .filter(function (x) { return x.links.length; });
    return { concerns: Array.from(concerns.values()), symbols: symbols, direct: of("pkg:" + path).filter(function (e) { return e.role !== "belongs to it"; }) };
  }

  // ----- rendering

  function ownerChip(e) {
    if (e.owner) return R.chip(e.owner, { cls: "flat" });
    return '<a class="chip flat bl-place" href="' + esc(e.href || "") + '"><span class="cl">' + esc(e.label || "") + "</span></a>";
  }
  // list shows one line per owner: its roles, the steps as numbers that
  // lead to them, and the detail when there is only one thing to say.
  // Beyond `limit` owners the rest fold away.
  function list(entries, limit) {
    limit = limit || 12;
    var groups = new Map();
    entries.forEach(function (e) {
      var k = e.owner || e.label || "";
      if (!groups.has(k)) groups.set(k, { e: e, steps: [], roles: [], details: [] });
      var g = groups.get(k);
      if (e.no && /^step /.test(e.role)) { if (!g.steps.some(function (x) { return x.no === e.no; })) g.steps.push(e); return; }
      var role = e.role.replace(/ at step \d+$/, "");
      if (g.roles.indexOf(role) < 0) g.roles.push(role);
      if (e.detail && g.details.indexOf(e.detail) < 0) g.details.push(e.detail);
    });
    var items = Array.from(groups.values()).map(function (g) {
      var h = '<li class="bl">' + ownerChip(g.e);
      if (g.steps.length) h += '<span class="bl-role">' + (g.steps.length > 1 ? "steps" : "step") + "</span>" + g.steps.map(function (x) {
        return '<a class="stepno" href="' + esc(x.href) + '">' + x.no + "</a>";
      }).join("");
      if (g.roles.length) h += '<span class="bl-role">' + esc(g.roles.join("; ")) + "</span>";
      var detail = g.steps.length === 1 && !g.roles.length ? g.steps[0].detail : !g.steps.length && g.details.length === 1 ? g.details[0] : "";
      if (detail) h += '<div class="bl-detail">' + X.md.inline(U.firstSentence(X.md.plainText(detail)), {}) + "</div>";
      return h + "</li>";
    });
    var out = '<ul class="bl-list">' + items.slice(0, limit).join("") + "</ul>";
    if (items.length > limit) out += '<details class="bl-more"><summary>' + U.plural(items.length - limit, "more place") + '</summary><ul class="bl-list">' + items.slice(limit).join("") + "</ul></details>";
    return out;
  }
  function owners(entries) { return new Set(entries.map(function (e) { return e.owner || e.label || ""; })).size; }
  function section(ref) {
    var l = of(ref);
    if (!l.length) return "";
    return '<section class="sp-sec bl-sec"><h3 class="sp-h">In the model <span class="count">' + owners(l) + "</span></h3>" + list(l) + "</section>";
  }
  function packageSection(path) {
    var x = inPackage(path);
    if (!x || (!x.concerns.length && !x.symbols.length && !x.direct.length)) return "";
    var h = "";
    if (x.concerns.length) {
      h += '<div class="bl-group"><h3 class="sp-label">Concerns</h3><ul class="bl-list">' + x.concerns.map(function (c) {
        return '<li class="bl">' + R.chip("concern:" + c.id, { cls: "flat" }) + '<span class="bl-role">' +
          (c.whole ? "the whole package" : U.plural(c.files.length, "file") + " of it") + "</span>" +
          (c.whole ? "" : '<div class="bl-detail chips">' + c.files.map(function (f) { return R.chip("file:" + f, { cls: "small" }); }).join("") + "</div>") + "</li>";
      }).join("") + "</ul></div>";
    }
    if (x.direct.length) h += '<div class="bl-group"><h3 class="sp-label">The package itself</h3>' + list(x.direct) + "</div>";
    if (x.symbols.length) {
      h += '<div class="bl-group"><h3 class="sp-label">Its declarations <span class="count">' + x.symbols.length + "</span></h3><ul class=\"bl-syms\">" + x.symbols.map(function (s) {
        return '<li><div class="bl-sym">' + R.chip(s.ref) + "</div>" + list(s.links, 6) + "</li>";
      }).join("") + "</ul></div>";
    }
    return h;
  }

  // ----- marks in the source

  var MARK_TITLE = { flow: "a step of a flow", key: "a key symbol of a concern", inv: "where an invariant is enforced or what arrives is checked" };
  X.source.addMarks(function (path) {
    var out = [];
    D.symbolsOfFile(path).forEach(function (s) {
      var seen = new Set();
      of(s.ref).forEach(function (e) {
        if (!e.mark || !e.owner) return;
        var k = e.mark + e.owner + (e.no || "");
        if (seen.has(k)) return;
        seen.add(k);
        var o = R.resolve(e.owner);
        out.push({
          line: s.line, cls: e.mark, ref: e.owner, href: e.href || R.href(e.owner),
          title: o.label + ": " + e.role + (e.detail ? " — " + U.firstSentence(X.md.plainText(e.detail)) : "") + " (" + MARK_TITLE[e.mark] + ")"
        });
      });
    });
    return out;
  });

  return { of: of, inPackage: inPackage, section: section, packageSection: packageSection, list: list, norm: norm };
})();
