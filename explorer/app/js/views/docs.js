// The Documents view. List (#/docs): the documents grouped as product,
// development, work orders, reviews, the explorer's README and examples.
// A document (#/docs/<path>?h=<anchor>): rendered, with its headings as an
// outline beside it that marks where the reader is. Findings
// (#/docs/findings?sel=<id>&q=): every review finding with its response
// and the places in the code whose comments cite it.
// Also the inspector's views of a document section, a finding and a work
// order, and search over document headings.
(function () {
  "use strict";
  var U = X.util, D = X.data, R = X.refs, esc = U.esc;
  var GROUPS = [
    ["product", "Product", "The README, the spec and the guides."],
    ["development", "Development", "The plan, the status log, what was left over, the development rules."],
    ["workorder", "Work orders", "One per chunk of the implementation."],
    ["review", "Reviews", "Review reports and the responses to them."],
    ["explorer", "The explorer", "This app's design and format reference."],
    ["example", "Examples", ""]
  ];

  // ----- the list

  function list(el) {
    var docs = D.core.documents || [];
    var h = '<div class="page dl"><h1>Documents</h1><p class="lead">The spec, the guides and the development records, with their references to code and to each other as links. ' +
      '<a href="' + esc(X.router.href("docs", "findings")) + '">' + U.plural((D.core.findings || []).length, "review finding") + "</a> are listed with their responses and where the code cites them.</p>";
    GROUPS.forEach(function (g) {
      var ds = docs.filter(function (d) { return d.group === g[0]; });
      if (!ds.length) return;
      h += '<section class="dl-g"><h2 class="sec-h">' + esc(g[1]) + ' <span class="count">' + ds.length + "</span></h2>" + (g[2] ? '<p class="muted small dl-gs">' + esc(g[2]) + "</p>" : "") +
        '<div class="dl-items">' + ds.map(function (d) {
          return '<a class="dl-i" href="' + esc(X.router.href("docs", d.path)) + '" data-ref="doc:' + esc(d.path) + '" data-act="full" data-nocard>' + U.icon(g[0] === "workorder" ? "chunk" : "doc") +
            '<span class="dl-t">' + esc(d.title) + '</span><span class="dl-p mono">' + esc(d.path) + '</span><span class="dl-n muted small num">' + U.plural(d.headings.length, "heading") + " · " + Math.max(1, Math.round(d.bytes / 1024)) + " KB</span></a>";
        }).join("") + "</div></section>";
    });
    var other = docs.filter(function (d) { return !GROUPS.some(function (g) { return g[0] === d.group; }); });
    if (other.length) h += '<section class="dl-g"><h2 class="sec-h">Other</h2><div class="dl-items">' + other.map(function (d) { return '<a class="dl-i" href="' + esc(X.router.href("docs", d.path)) + '">' + U.icon("doc") + '<span class="dl-t">' + esc(d.title) + '</span><span class="dl-p mono">' + esc(d.path) + "</span></a>"; }).join("") + "</div></section>";
    el.innerHTML = h + "</div>";
  }

  // ----- a document

  var S = null;
  function docView(el, r, d) {
    el.innerHTML = '<div class="dv"><nav class="dv-toc"><div class="dv-toc-h"><a href="#/docs">' + U.icon("back") + "Documents</a></div>" +
      '<div class="dv-toc-l">' + d.headings.filter(function (x) { return x.level <= 3; }).map(function (x) {
        return '<a class="dv-ti l' + x.level + '" href="#" data-anchor-link="' + esc(x.anchor) + '">' + esc(X.md.plainText(x.text)) + "</a>";
      }).join("") + "</div></nav>" +
      '<div class="dv-body"><div class="dv-head"><code class="mono small muted">' + esc(d.path) + '</code><a class="btn ghost small" href="' + esc(X.router.href("code", d.path)) + '">' + U.icon("file") + "Source</a></div>" +
      '<article class="md dv-md"><div class="loading"><span class="spinner"></span>Loading ' + esc(d.path) + "…</div></article></div></div>";
    S = { el: el, d: d, ready: false };
    var body = el.querySelector(".dv-body");
    D.doc(d.path).then(function (dt) {
      if (!S || S.d !== d) return;
      var t0 = performance.now();
      el.querySelector(".dv-md").innerHTML = X.md.render(dt.text, { doc: d.path, headings: d.headings, headingTag: 1 });
      document.documentElement.dataset.docRenderMs = Math.round(performance.now() - t0);
      S.ready = true;
      S.heads = Array.from(el.querySelectorAll(".dv-md .md-h"));
      body.addEventListener("scroll", onScroll, { passive: true });
      goTo(X.router.current.params.h);
    }, function (err) { el.querySelector(".dv-md").innerHTML = '<div class="error">' + esc(err.message) + "</div>"; });
    el.addEventListener("click", function (ev) {
      var a = ev.target.closest("[data-anchor-link]");
      if (a) { ev.preventDefault(); X.router.setParams({ h: a.getAttribute("data-anchor-link") }); return; }
      // A link to a heading of this document scrolls instead of inspecting.
      var c = ev.target.closest('[data-ref^="doc:"]');
      if (c && !U.isModifiedClick(ev)) {
        var ref = c.getAttribute("data-ref").slice(4), i = ref.indexOf("#");
        if (i > 0 && ref.slice(0, i) === d.path) {
          ev.preventDefault();
          var h = (R.resolve(c.getAttribute("data-ref")).resolved || {}).anchor || ref.slice(i + 1);
          var hd = d.headings.filter(function (x) { return x.anchor === h || x.number === h; })[0];
          X.router.setParams({ h: hd ? hd.anchor : h });
        }
      }
    });
  }
  function goTo(anchor) {
    if (!S || !S.ready || !anchor) return;
    // A section number ("12", "6.2") stands for its heading.
    var byNum = S.d.headings.filter(function (x) { return x.number === anchor && x.anchor !== anchor; })[0];
    if (byNum && !S.d.headings.some(function (x) { return x.anchor === anchor; })) anchor = byNum.anchor;
    var t = S.el.querySelector('.dv-md [data-anchor="' + (window.CSS && CSS.escape ? CSS.escape(anchor) : anchor) + '"]');
    if (t) { t.scrollIntoView({ block: "start" }); markToc(anchor); }
  }
  var onScroll = U.debounce(function () {
    if (!S || !S.heads) return;
    var body = S.el.querySelector(".dv-body"), top = body.getBoundingClientRect().top + 40, cur = null;
    for (var i = 0; i < S.heads.length; i++) { if (S.heads[i].getBoundingClientRect().top <= top) cur = S.heads[i]; else break; }
    markToc(cur ? cur.getAttribute("data-anchor") : "");
  }, 60);
  function markToc(anchor) {
    if (!S) return;
    var toc = S.el.querySelector(".dv-toc-l");
    toc.querySelectorAll(".dv-ti.on").forEach(function (a) { a.classList.remove("on"); });
    var a = anchor ? toc.querySelector('[data-anchor-link="' + (window.CSS && CSS.escape ? CSS.escape(anchor) : anchor) + '"]') : null;
    if (a) {
      a.classList.add("on");
      var tr = toc.getBoundingClientRect(), ar = a.getBoundingClientRect();
      if (ar.top < tr.top || ar.bottom > tr.bottom) toc.scrollTop += ar.top - tr.top - tr.height / 3;
    }
  }

  // ----- findings

  function citedChips(f, max) {
    var ins = [];
    (f.cited || []).forEach(function (s) { var k = s.in || "file:" + s.file; if (ins.indexOf(k) < 0) ins.push(k); });
    if (!ins.length) return '<span class="muted">—</span>';
    return ins.slice(0, max).map(function (x) { return R.chip(x, { cls: "small" }); }).join("") + (ins.length > max ? '<span class="more muted small">+' + (ins.length - max) + "</span>" : "");
  }
  function findings(el, r) {
    var fs = D.core.findings || [];
    var cited = fs.filter(function (f) { return (f.cited || []).length; }).length;
    el.innerHTML = '<div class="page fd"><div class="fd-crumb"><a href="#/docs">' + U.icon("back") + "Documents</a></div><h1>Review findings</h1>" +
      '<p class="lead">Every finding of the reviews, with the response to it and the places in the code whose comments cite it: how to find out why a piece of hardening exists. ' +
      U.plural(cited, "finding") + " of " + fs.length + " are cited in code.</p><div class=\"fd-table\"></div></div>";
    S = { findings: true, el: el };
    S.table = X.table.create(el.querySelector(".fd-table"), {
      columns: [
        { id: "id", title: "Finding", width: "100px", html: function (f) { return '<a class="mono" data-row data-ref="finding:' + esc(f.id) + '">' + esc(f.id) + "</a>"; }, sort: function (f) { return f.id; } },
        { id: "title", title: "Title", html: function (f) { return '<div class="fd-t">' + X.doc.plain(f.title) + "</div>" + (f.meta ? '<div class="muted small">' + esc(f.meta) + (f.unconfirmed ? " · unconfirmed" : "") + "</div>" : ""); }, sort: function (f) { return f.title.toLowerCase(); } },
        { id: "review", title: "Review", width: "70px", html: function (f) { return esc(f.review); }, sort: function (f) { return f.review; } },
        { id: "resp", title: "Response", width: "28%", cls: "opt", html: function (f) { return f.response ? '<span class="small ink2">' + X.doc.plain(U.firstSentence(X.md.plainText(f.response.replace(/^\*\*[^*]+\*\*\s*/, "")))) + "</span>" : '<span class="muted small">none</span>'; } },
        { id: "cited", title: "Cited in code", width: "24%", html: function (f) { return '<div class="chips">' + citedChips(f, 3) + "</div>"; }, sort: function (f) { return -(f.cited || []).length; } }
      ],
      rows: fs,
      key: function (f) { return f.id; },
      text: function (f) { return [f.id, f.title, f.meta, f.review, f.text, f.response].join(" "); },
      query: r.params.q || "", selected: r.params.sel || null,
      placeholder: "Filter findings…",
      onQuery: function (q) { X.router.setParams({ q: q || null }); },
      onSelect: function (f) { X.router.setParams({ sel: f.id }); X.inspector.open("finding:" + f.id); }
    });
    if (r.params.sel) {
      S.table.select(r.params.sel, true);
      if (!r.params.i && D.finding(r.params.sel)) setTimeout(function () { X.inspector.open("finding:" + r.params.sel); }, 0);
    }
  }

  X.router.register("docs", {
    nav: "docs",
    title: function (r) { var d = D.document(r.rest); return d ? d.title : r.rest === "findings" ? "Review findings" : "Documents"; },
    render: function (el, r) {
      S = null;
      if (!r.rest) { list(el); return; }
      if (r.rest === "findings") { findings(el, r); return; }
      var d = D.document(r.rest);
      if (!d) { el.innerHTML = '<div class="page narrow"><h1>No such document</h1><p class="muted"><code>' + esc(r.rest) + '</code> is not a document of the site. See the <a href="#/docs">documents</a>.</p></div>'; return; }
      docView(el, r, d);
    },
    update: function (r) {
      if (!S) return false;
      if (S.findings && r.rest === "findings") { if (r.params.sel) S.table.select(r.params.sel, false); return true; }
      if (S.d && r.rest === S.d.path) { goTo(r.params.h); return true; }
      return false;
    },
    unmount: function () { S = null; }
  });

  // ----- inspector views

  function sectionOf(text, d, anchor) {
    var h = d.headings.filter(function (x) { return x.anchor === anchor || x.number === anchor; })[0];
    if (!h) return null;
    var next = d.headings.filter(function (x) { return x.line > h.line && x.level <= h.level; })[0];
    var lines = text.split("\n");
    return { h: h, text: lines.slice(h.line - 1, next ? next.line - 1 : lines.length).join("\n") };
  }
  X.inspector.register("doc", function (info, body) {
    var d = info.obj, anchor = (info.resolved && info.resolved.anchor) || info.key.split("#")[1] || "";
    var el = U.h("div.insp-pad");
    el.innerHTML = '<div class="loading"><span class="spinner"></span>Loading…</div>';
    body.appendChild(el);
    var alive = true;
    D.doc(d.path).then(function (dt) {
      if (!alive) return;
      var sec = anchor ? sectionOf(dt.text, d, anchor) : null;
      var text = sec ? sec.text : dt.text, first = sec ? sec.h.line : 1;
      var heads = d.headings.filter(function (x) { return x.line >= first; });
      el.innerHTML = '<p class="muted small">' + esc(d.title) + ' · <code>' + esc(d.path) + "</code></p>" +
        '<div class="md ins-doc">' + X.md.render(text.length > 60000 ? text.slice(0, 60000) : text, { doc: d.path, headings: heads, firstLine: first, headingTag: 2 }) + "</div>" +
        '<p><a class="btn" href="' + esc(R.href(info.ref)) + '">' + U.icon("full") + "Open the document</a></p>";
    }, function (e) { if (alive) el.innerHTML = '<div class="error">' + esc(e.message) + "</div>"; });
    return function () { alive = false; };
  });

  function citedList(sites) {
    if (!sites || !sites.length) return '<p class="muted small">No comment in the code cites it.</p>';
    return '<ul class="sites">' + sites.map(function (s) {
      var f = D.file(s.file);
      return "<li>" + (s.in ? R.chip(s.in) : R.chip("file:" + s.file)) + (f && f.test ? '<span class="tag">test</span>' : "") +
        X.symbolPanel.siteLink(s.file, s.line, null).replace("</a>", '<span class="mono small muted">' + esc(s.file) + "</span></a>") + "</li>";
    }).join("") + "</ul>";
  }
  function headingAt(path, line) {
    var d = D.document(path);
    if (!d) return "";
    var h = d.headings.filter(function (x) { return x.line <= line; }).pop();
    return h ? "doc:" + path + "#" + h.anchor : "doc:" + path;
  }
  function sitePad(body, h) {
    var el = U.h("div.insp-pad.sp", { html: h });
    el.addEventListener("click", function (ev) {
      var a = ev.target.closest("a.site");
      if (!a || U.isModifiedClick(ev) || ev.button !== 0) return;
      ev.preventDefault();
      X.router.go(X.router.hrefKeep("code", a.getAttribute("data-file"), { line: a.getAttribute("data-line") }));
    });
    body.appendChild(el);
  }
  X.inspector.register("finding", function (info, body) {
    var f = info.obj;
    var h = '<h2 class="ins-title">' + X.doc.plain(f.title) + "</h2>" +
      '<div class="chips ins-meta">' + (f.meta ? '<span class="tag">' + esc(f.meta) + "</span>" : "") + (f.unconfirmed ? '<span class="tag">unconfirmed</span>' : "") +
      '<span class="muted small">review ' + esc(f.review) + "</span>" + R.chip(headingAt(f.file, f.line), { cls: "small" }) + "</div>" +
      '<div class="md ins-doc">' + X.md.render(f.text || "", { doc: f.file, headingTag: 3 }) + "</div>" +
      '<section class="sp-sec"><h3 class="sp-h">Response</h3>' + (f.response ? '<div class="md ins-doc">' + X.md.render(f.response, { doc: f.responseFile, headingTag: 3 }) + "</div>" +
        (f.responseFile ? R.chip(headingAt(f.responseFile, f.responseLine), { cls: "small" }) : "") : '<p class="muted small">No response recorded.</p>') + "</section>" +
      '<section class="sp-sec"><h3 class="sp-h">Cited in the code <span class="count">' + (f.cited || []).length + "</span></h3>" + citedList(f.cited) + "</section>";
    sitePad(body, h);
  });
  X.inspector.register("chunk", function (info, body) {
    var w = info.obj;
    var h = '<h2 class="ins-title">' + esc(w.title) + "</h2>" +
      '<p><a class="btn" href="' + esc(X.router.href("docs", w.file)) + '">' + U.icon("full") + "Read the work order</a></p>" +
      '<section class="sp-sec"><h3 class="sp-h">Cited in the code <span class="count">' + (w.cited || []).length + "</span></h3>" + citedList(w.cited) + "</section>";
    sitePad(body, h);
  });

  R.define("finding", {
    card: function (info) {
      var o = info.obj;
      return '<div class="hc-doc"><b>' + X.doc.plain(o.title) + "</b></div>" + (o.meta ? '<div class="hc-row muted">' + esc(o.meta) + "</div>" : "") +
        '<div class="hc-row muted">review ' + esc(o.review) + (o.response ? " · answered" : "") + ((o.cited || []).length ? " · cited in " + U.plural(o.cited.length, "place") + " of the code" : "") + "</div>";
    }
  });

  X.search.addSource({
    kind: "heading", title: "Document headings", order: 95,
    items: function () {
      var out = [];
      (D.core.documents || []).forEach(function (d) {
        d.headings.forEach(function (h) {
          if (h.level > 3) return;
          out.push({ ref: "doc:" + d.path + "#" + h.anchor, name: h.text, keys: [h.text, h.number ? "§" + h.number : ""], detail: d.title, exported: d.group !== "review" && d.group !== "workorder", weight: -10 });
        });
      });
      return out;
    }
  });
})();
