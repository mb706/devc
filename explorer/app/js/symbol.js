// The symbol panel: everything known about one declaration. Used beside
// the source in the Code view and as the inspector's view of a symbol.
//
//   X.symbolPanel.render(el, ref, {excerpt: bool, goto(file, line, ev)})
//     excerpt: show the declaration's code (the inspector does)
//     goto:    how a click on a use or call site navigates (default: the
//              Code view at that line, keeping the inspector)
//   Returns a cleanup function.
X.symbolPanel = (function () {
  "use strict";
  var U = X.util, D = X.data, R = X.refs, esc = U.esc;

  function siteLink(file, line, text, opts) {
    var h = X.router.hrefKeep("code", file, { line: line });
    return '<a class="site" href="' + esc(h) + '" data-file="' + esc(file) + '" data-line="' + line + '">' +
      '<span class="site-ln num">' + line + "</span>" +
      (text != null ? '<code class="site-code">' + X.doc.highlightGo(text) + "</code>" : "") + "</a>";
  }

  function section(title, count, cls) {
    var s = U.h("section.sp-sec" + (cls ? "." + cls : ""));
    s.innerHTML = '<h3 class="sp-h">' + esc(title) + (count != null ? ' <span class="count">' + U.fmt(count) + "</span>" : "") + "</h3>";
    return s;
  }

  function render(el, ref, opts) {
    opts = opts || {};
    var s = D.sym(ref);
    el.innerHTML = "";
    if (!s) { el.innerHTML = '<div class="error">No symbol ' + esc(ref) + " in this build.</div>"; return null; }
    var wrap = U.h("div.sp");
    el.appendChild(wrap);
    var parent = s.parent ? D.sym(s.parent) : null;

    // Header
    var head = U.h("div.sp-head");
    head.innerHTML =
      '<div class="sp-name">' + R.badge(s) + '<h2 class="mono">' + esc(parent ? parent.name + "." + s.name : s.name) + "</h2>" +
      (s.test ? '<span class="tag">test</span>' : "") + (!s.exported ? '<span class="tag">unexported</span>' : "") + "</div>" +
      '<div class="sp-meta">' + esc(R.kindText(s)) + (parent ? " of " + R.chip(parent.ref) : "") + " in " + R.chip("pkg:" + s.package) + "</div>" +
      '<div class="sp-loc">' + siteLink(s.file, s.line, null).replace('<a class="site"', '<a class="site loc"').replace("</a>", '<span class="mono">' + esc(s.file) + "</span></a>") +
      '<button class="btn ghost icon" data-a="copy" title="Copy path:line">' + U.icon("copy") + "</button>" +
      '<a class="btn ghost icon" data-a="editor" title="Open in editor">' + U.icon("editor") + "</a></div>";
    head.querySelector('[data-a="copy"]').onclick = function () { U.copy(s.file + ":" + s.line); };
    var ed = head.querySelector('[data-a="editor"]'), eu = X.settings.editorURL(s.file, s.line);
    if (eu) ed.href = eu; else { ed.classList.add("off"); ed.onclick = function (e) { e.preventDefault(); X.shell.openSettings(); }; }
    wrap.appendChild(head);

    var sig = U.h("pre.sp-sig");
    sig.innerHTML = X.doc.highlightGo(s.signature);
    wrap.appendChild(sig);
    if (s.kind === "const" && s.value && s.signature.indexOf(s.value) < 0) {
      wrap.appendChild(U.h("div.sp-value", { html: '<span class="muted">value</span> <code>' + esc(s.value) + "</code>" }));
    }
    if (s.doc) wrap.appendChild(U.h("div.doc.sp-doc", { html: X.doc.render(s.doc, { pkg: s.package }) }));
    else wrap.appendChild(U.h("div.sp-doc.muted.small", "No doc comment."));

    runsIn(wrap, s);
    var bl = X.backlinks ? X.backlinks.section(s.ref) : "";
    if (bl) wrap.insertAdjacentHTML("beforeend", bl);

    if (opts.excerpt) excerpt(wrap, s);

    members(wrap, s);
    implementations(wrap, s);

    // The rest needs the package's chunk.
    var dyn = U.h("div.sp-dyn");
    dyn.innerHTML = '<div class="loading"><span class="spinner"></span>Loading uses…</div>';
    wrap.appendChild(dyn);
    var alive = true;
    D.chunkOfPackage(s.package).then(function (c) {
      if (!alive) return;
      dyn.innerHTML = "";
      usedBy(dyn, s, (c.usedBy || {})[s.ref] || [], (c.dynamicCallers || {})[s.ref] || []);
      calls(dyn, s, (c.calls || {})[s.ref] || []);
    }, function (err) { if (alive) dyn.innerHTML = '<div class="error">' + esc(err.message) + "</div>"; });

    wrap.addEventListener("click", function (ev) {
      var a = ev.target.closest("a.site");
      if (!a || U.isModifiedClick(ev) || ev.button !== 0) return;
      ev.preventDefault();
      var f = a.getAttribute("data-file"), l = +a.getAttribute("data-line");
      if (opts.goto) opts.goto(f, l, ev); else X.router.go(X.router.hrefKeep("code", f, { line: l }));
    });
    return function () { alive = false; };
  }

  function runsIn(wrap, s) {
    var ids = s.actors || [], note = "";
    if (s.kind === "type") {
      var set = new Set();
      D.children(s.ref).forEach(function (m) { (m.actors || []).forEach(function (a) { set.add(a); }); });
      ids = Array.from(set);
      note = "its methods";
    }
    if (s.kind !== "func" && s.kind !== "method" && !ids.length) return;
    var sec = U.h("div.sp-runs");
    var reach = (D.core.reach && D.core.reach.note) || "";
    sec.innerHTML = '<div class="sp-runs-h"><span class="sp-label">' + (note ? "May run in (" + note + ")" : "May run in") + "</span>" +
      '<button class="btn ghost icon" data-a="note" title="How this is computed">' + U.icon("info") + "</button></div>" +
      (ids.length ? R.actorList(ids) : '<span class="muted small">No entry point reaches it (test code, or reached only through calls the graph cannot see).</span>') +
      '<p class="sp-note small muted hidden">' + esc(reach) + "</p>";
    sec.querySelector('[data-a="note"]').onclick = function () { sec.querySelector(".sp-note").classList.toggle("hidden"); };
    wrap.appendChild(sec);
  }

  function excerpt(wrap, s) {
    var box = U.h("div.sp-code");
    wrap.appendChild(box);
    var scroller = U.h("div.sp-code-scroll");
    box.appendChild(scroller);
    D.source(s.file).then(function (sf) {
      var start = X.source.declStart(sf, s);
      var end = Math.min(s.endLine, start + 79);
      X.source.view(scroller, {
        path: s.file, from: start, to: end, range: [s.line, s.endLine], wrap: true,
        expand: function (v) { v.expandAll(); }
      });
    }, function (err) { scroller.textContent = err.message; });
  }

  function members(wrap, s) {
    var kids = D.children(s.ref).filter(function (k) { return X.settings.get("tests") || !k.test; });
    if (!kids.length) return;
    var fields = kids.filter(function (k) { return k.kind === "field"; });
    var methods = kids.filter(function (k) { return k.kind !== "field"; })
      .sort(function (a, b) { return (b.exported ? 1 : 0) - (a.exported ? 1 : 0) || (a.name < b.name ? -1 : 1); });
    function list(title, items, showType) {
      if (!items.length) return;
      var sec = section(title, items.length);
      sec.insertAdjacentHTML("beforeend", '<ul class="sp-members">' + items.map(function (k) {
        var type = showType ? k.signature.replace(/^\S+\s*/, "").replace(/`[^`]*`\s*$/, "").trim() : "";
        return "<li>" + R.chip(k.ref, { label: k.name }) + (type ? ' <code class="sp-type">' + esc(type) + "</code>" : "") +
          (k.doc ? '<div class="sp-first">' + X.doc.inline(U.firstSentence(k.doc), { pkg: k.package }) + "</div>" : "") + "</li>";
      }).join("") + "</ul>");
      wrap.appendChild(sec);
    }
    list("Fields", fields, true);
    list(s.typeKind === "interface" ? "Methods of the interface" : "Methods", methods, false);
  }

  function implementations(wrap, s) {
    if (s.kind !== "type") return;
    var impls = D.implementationsOf(s.ref), ifs = D.interfacesOf(s.ref);
    if (impls.length) {
      var a = section("Implemented by", impls.length);
      a.insertAdjacentHTML("beforeend", '<div class="chips">' + impls.map(function (im) {
        return R.chip(im.type, im.pointer ? { label: "*" + R.resolve(im.type).label } : null);
      }).join("") + "</div>");
      wrap.appendChild(a);
    }
    if (ifs.length) {
      var b = section("Implements", ifs.length);
      b.insertAdjacentHTML("beforeend", '<div class="chips">' + ifs.map(function (im) {
        return R.chip(im.interface) + (im.pointer ? '<span class="muted small"> (as pointer)</span>' : "");
      }).join("") + "</div>");
      wrap.appendChild(b);
    }
  }

  var USE_KIND = { call: "called", type: "as a type", field: "in a literal", ref: "mentioned" };

  // usedBy groups the uses by package and by the enclosing declaration.
  function usedBy(dyn, s, uses, dynCallers) {
    var code = uses.filter(function (u) { return !u.test; }), tests = uses.filter(function (u) { return u.test; });
    var sec = section("Used by", code.length);
    if (!uses.length && !dynCallers.length) {
      sec.insertAdjacentHTML("beforeend", '<p class="muted small">No uses in the module.</p>');
      dyn.appendChild(sec);
      return;
    }
    if (!code.length) sec.insertAdjacentHTML("beforeend", '<p class="muted small">Used only in tests.</p>');
    else sec.appendChild(useGroups(code, 160));
    dyn.appendChild(sec);
    if (dynCallers.length) {
      var d = section("Called through an interface", dynCallers.length);
      d.insertAdjacentHTML("beforeend", '<ul class="sp-calls">' + dynCallers.map(function (c) {
        return "<li>" + R.chip(c.ref) + (c.via ? ' <span class="muted small">via</span> ' + R.chip(c.via) : "") + "</li>";
      }).join("") + "</ul>");
      dyn.appendChild(d);
    }
    if (tests.length) {
      var t = section("Used in tests", tests.length, "tests");
      var body = U.h("div.sp-collapsed");
      var btn = U.h("button.btn.ghost.small", { type: "button" }, "Show " + U.plural(tests.length, "use") + " in tests");
      btn.onclick = function () { btn.remove(); body.appendChild(useGroups(tests, 400)); };
      body.appendChild(btn);
      t.appendChild(body);
      if (X.settings.get("tests") && tests.length <= 12) { btn.remove(); body.appendChild(useGroups(tests, 400)); }
      dyn.appendChild(t);
    }
  }

  function useGroups(uses, limit) {
    var box = U.h("div.uses");
    var byPkg = U.groupBy(uses, function (u) { var f = D.file(u.file); return f && f.package ? f.package : U.dirOf(u.file); });
    var shown = 0, html = "";
    var pkgs = Array.from(byPkg.keys()).sort(function (a, b) { return byPkg.get(b).length - byPkg.get(a).length || (a < b ? -1 : 1); });
    var rest = [];
    pkgs.forEach(function (p) {
      var list = byPkg.get(p);
      if (shown >= limit) { rest.push(p); return; }
      html += '<div class="ug"><div class="ug-h">' + (D.pkg(p) ? R.chip("pkg:" + p) : '<code>' + esc(p) + "</code>") +
        ' <span class="count">' + list.length + "</span></div>";
      var byIn = U.groupBy(list, function (u) { return u.in || ""; });
      byIn.forEach(function (us, fn) {
        html += '<div class="ui"><div class="ui-h">' + (fn ? R.chip(fn) : '<span class="muted small">package level</span>') +
          (us.length > 1 ? ' <span class="count">' + us.length + "</span>" : "") +
          ' <span class="muted small">' + esc(kindsOf(us)) + "</span></div>" +
          us.map(function (u) { return siteLink(u.file, u.line, u.text); }).join("") + "</div>";
        shown += us.length;
      });
      html += "</div>";
    });
    box.innerHTML = html;
    if (rest.length) {
      var n = rest.reduce(function (a, p) { return a + byPkg.get(p).length; }, 0);
      var more = U.h("button.btn.small", { type: "button" }, "Show " + U.plural(n, "more use") + " in " + U.plural(rest.length, "package"));
      more.onclick = function () { box.replaceWith(useGroups(uses, Infinity)); };
      box.appendChild(more);
    }
    return box;
  }
  function kindsOf(us) {
    var set = new Set(us.map(function (u) { return USE_KIND[u.kind] || u.kind; }));
    return Array.from(set).join(", ");
  }

  function calls(dyn, s, list) {
    if (!list.length) return;
    var stat = list.filter(function (c) { return !c.dynamic; });
    var viaIface = U.groupBy(list.filter(function (c) { return c.dynamic; }), function (c) { return c.via || ""; });
    var sec = section(s.kind === "func" || s.kind === "method" ? "Calls" : "Mentions", stat.length + viaIface.size);
    var html = '<ul class="sp-calls">';
    U.sortBy(stat, function (c) { return c.line; }).forEach(function (c) {
      html += "<li>" + siteLink(s.file, c.line, null) + R.chip(c.ref) + "</li>";
    });
    viaIface.forEach(function (cs, via) {
      html += '<li class="dyn">' + siteLink(s.file, cs[0].line, null) + (via ? R.chip(via) : "") +
        '<span class="muted small"> through the interface, may reach</span><div class="chips">' +
        cs.map(function (c) { return R.chip(c.ref); }).join("") + "</div></li>";
    });
    sec.insertAdjacentHTML("beforeend", html + "</ul>");
    dyn.appendChild(sec);
  }

  X.inspector.register("symbol", function (info, body) {
    var el = U.h("div.insp-pad");
    body.appendChild(el);
    return render(el, info.key, { excerpt: true });
  });

  return { render: render, siteLink: siteLink, useGroups: useGroups };
})();
