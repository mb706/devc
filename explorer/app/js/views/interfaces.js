// The Interfaces view: one tab per table of entry points of the model
// (commands, RPC methods by surface, routes, events, configuration files,
// environment variables), each a filterable table whose rows lead to code.
// Route: #/interfaces/<surface id>?q=<filter>&sel=<row>. Also the
// inspector's views of commands, RPC methods, routes, event types,
// configuration keys and environment variables.
(function () {
  "use strict";
  var U = X.util, D = X.data, R = X.refs, esc = U.esc;
  var KIND_ORDER = ["commands", "rpc", "http", "events", "config", "env"];
  var KIND_TITLE = { commands: "Commands", rpc: "RPC methods", http: "Routes", events: "Events", config: "Configuration", env: "Environment" };

  function surfaces() {
    return (D.model.surfaces || []).slice().sort(function (a, b) { return KIND_ORDER.indexOf(a.kind) - KIND_ORDER.indexOf(b.kind); });
  }
  function uniq(list) { return Array.from(new Set(list.filter(Boolean))); }
  function chips(refs, max) {
    refs = uniq(refs);
    max = max || 4;
    var h = refs.slice(0, max).map(function (r) { return R.chip(r); }).join("");
    if (refs.length > max) h += '<span class="more muted small">+' + (refs.length - max) + "</span>";
    return h || '<span class="muted">—</span>';
  }

  // ----- tables per kind

  var TABLES = {
    commands: function (s) {
      var parentActor = function (c) { var p = c.parent ? D.cmd(c.parent) : null; return p ? p.actor : ""; };
      return {
        rows: D.core.commands,
        key: function (c) { return c.path; },
        ref: function (c) { return "cmd:" + c.path; },
        tree: { depth: function (c) { return c.path.split(" ").length - 1; }, parent: function (c) { return c.parent || ""; } },
        text: function (c) { return c.path + " " + (c.short || "") + " " + (c.run || "") + " " + (c.hidden ? "hidden" : "") + " " + (c.flags || []).map(function (f) { return f.name; }).join(" "); },
        columns: [
          { id: "cmd", title: "Command", width: "21%", html: function (c) {
            var name = c.path.split(" ").pop();
            return '<a class="mono cmd-name" data-row data-ref="cmd:' + esc(c.path) + '">' + esc(name) + "</a>" +
              (c.hidden ? ' <span class="tag">hidden</span>' : "") + (c.deprecated ? ' <span class="tag warn">deprecated</span>' : "") +
              (c.aliases && c.aliases.length ? ' <span class="muted small">(' + esc(c.aliases.join(", ")) + ")</span>" : "");
          }, sort: function (c) { return c.path; } },
          { id: "short", title: "Description", html: function (c) { return esc(c.short || ""); } },
          { id: "flags", title: "Flags", width: "54px", cls: "num r opt", html: function (c) { var n = (c.flags || []).length; return n ? '<span title="' + esc((c.flags || []).map(function (f) { return "--" + f.name; }).join(" ")) + '">' + n + "</span>" : '<span class="muted">—</span>'; }, sort: function (c) { return -(c.flags || []).length; } },
          { id: "run", title: "Runs", width: "22%", html: function (c) { return c.run ? R.chip(c.run) : '<span class="muted">—</span>'; }, sort: function (c) { return c.run || "~"; } },
          { id: "actor", title: "As", width: "15%", html: function (c) {
            if (!c.actor) return '<span class="muted">—</span>';
            return c.actor === parentActor(c) ? '<span class="muted small" title="as its parent">〃</span>' : R.chip("actor:" + c.actor);
          }, sort: function (c) { return c.actor || "~"; } }
        ]
      };
    },
    rpc: function (s) {
      return {
        rows: D.core.rpc.filter(function (m) { return (m.surfaces || []).indexOf(s.id) >= 0; }),
        key: function (m) { return m.name; },
        ref: function (m) { return "rpc:" + m.name; },
        text: function (m) { return m.name + " " + m.const + " " + (m.doc || ""); },
        columns: [
          { id: "name", title: "Method", width: "20%", html: function (m) { return '<a class="mono" data-row data-ref="rpc:' + esc(m.name) + '">' + esc(m.name) + "</a>"; }, sort: function (m) { return m.name; } },
          { id: "doc", title: "Description", html: function (m) { return m.doc ? X.doc.inline(U.firstSentence(m.doc), {}) : '<span class="muted">—</span>'; } },
          { id: "handlers", title: "Handled by", width: "24%", html: function (m) { return chips((m.handlers || []).map(function (x) { return x.handler || x.in; }), 2); }, sort: function (m) { return -(m.handlers || []).length; } },
          { id: "callers", title: "Called from", width: "22%", html: function (m) { return chips((m.callers || []).map(function (x) { return x.in; }), 2); }, sort: function (m) { return -(m.callers || []).length; } },
          { id: "also", title: "Also in", width: "10%", cls: "opt", html: function (m) {
            return (m.surfaces || []).filter(function (x) { return x !== s.id; }).map(function (x) { return R.chip("surface:" + x, { plain: true }); }).join("") || '<span class="muted">—</span>';
          } }
        ]
      };
    },
    http: function (s) {
      return {
        rows: D.core.routes.filter(function (r) { return r.surface === s.id; }),
        key: function (r) { return r.key; },
        ref: function (r) { return "http:" + r.key; },
        text: function (r) { return r.key + " " + (r.access || "") + " " + (r.crossSite || "") + " " + (r.handler || ""); },
        columns: [
          { id: "method", title: "Method", width: "80px", html: function (r) { return '<span class="http-m m-' + esc(r.method.toLowerCase()) + '">' + esc(r.method) + "</span>"; }, sort: function (r) { return r.method; } },
          { id: "path", title: "Path", html: function (r) { return '<a class="mono" data-row data-ref="http:' + esc(r.key) + '">' + esc(r.path) + "</a>"; }, sort: function (r) { return r.path; } },
          { id: "access", title: "Access", width: "16%", html: function (r) { return r.accessRef ? R.chip(r.accessRef, { label: r.access }) : esc(r.access || ""); }, sort: function (r) { return r.access || ""; } },
          { id: "xsite", title: "Cross-site", width: "16%", html: function (r) { return r.crossSiteRef ? R.chip(r.crossSiteRef, { label: r.crossSite }) : esc(r.crossSite || ""); }, sort: function (r) { return r.crossSite || ""; } },
          { id: "handler", title: "Handler", width: "26%", html: function (r) { return r.handler ? R.chip(r.handler) : '<span class="muted">—</span>'; }, sort: function (r) { return r.handler || ""; } }
        ]
      };
    },
    events: function (s) {
      return {
        rows: D.core.events.filter(function (e) { return e.surface === s.id; }),
        key: function (e) { return e.name; },
        ref: function (e) { return "event:" + e.name; },
        text: function (e) { return e.name + " " + e.const + " " + (e.doc || ""); },
        columns: [
          { id: "name", title: "Type", width: "16%", html: function (e) { return '<a class="mono" data-row data-ref="event:' + esc(e.name) + '">' + esc(e.name) + "</a>"; }, sort: function (e) { return e.name; } },
          { id: "doc", title: "Description", html: function (e) { return e.doc ? X.doc.inline(U.firstSentence(e.doc), {}) : '<span class="muted">—</span>'; } },
          { id: "w", title: "Written by", width: "24%", html: function (e) { return chips((e.writers || []).map(function (x) { return x.in; }), 3); }, sort: function (e) { return -(e.writers || []).length; } },
          { id: "r", title: "Read by", width: "24%", html: function (e) { return chips((e.readers || []).map(function (x) { return x.in; }), 3); }, sort: function (e) { return -(e.readers || []).length; } }
        ]
      };
    },
    config: function (s) {
      var rows = D.core.config.filter(function (c) { return c.surface === s.id; });
      var base = (s.key || "").split(".").length;
      return {
        rows: rows,
        key: function (c) { return c.key; },
        ref: function (c) { return "config:" + c.key; },
        tree: { depth: function (c) { return Math.max(0, c.key.split(".").length - base - 1); }, parent: function (c) { return c.parent || ""; } },
        text: function (c) { return c.key + " " + c.type + " " + (c.doc || ""); },
        columns: [
          { id: "key", title: "Key", width: "21%", html: function (c) {
            var last = c.key.split(".").pop();
            return '<a class="mono" data-row data-ref="config:' + esc(c.key) + '" title="' + esc(c.key) + '">' + esc(c.kind === "tables" ? "[[" + last + "]]" : c.kind === "table" || c.kind === "map" ? "[" + last + "]" : last) + "</a>";
          }, sort: function (c) { return c.key; } },
          { id: "type", title: "Go type", width: "17%", html: function (c) { return '<code class="small">' + esc(c.type) + "</code>"; }, sort: function (c) { return c.type; } },
          { id: "doc", title: "Description", html: function (c) { var sy = D.sym(c.field); return c.doc ? X.doc.inline(U.firstSentence(c.doc), { pkg: sy ? sy.package : "" }) : '<span class="muted">—</span>'; } },
          { id: "uses", title: "Uses", width: "60px", cls: "num r", html: function (c) { var sy = D.sym(c.field); return sy && sy.uses ? U.fmt(sy.uses) : '<span class="muted">0</span>'; }, sort: function (c) { var sy = D.sym(c.field); return -(sy && sy.uses || 0); } }
        ]
      };
    },
    env: function (s) {
      return {
        rows: D.core.env,
        key: function (e) { return e.name; },
        ref: function (e) { return "env:" + e.name; },
        text: function (e) { return e.name + " " + (e.consts || []).join(" ") + " " + (e.uses || []).map(function (u) { return u.file; }).join(" "); },
        columns: [
          { id: "name", title: "Variable", width: "26%", html: function (e) { return '<a class="mono" data-row data-ref="env:' + esc(e.name) + '">' + esc(e.name) + "</a>"; }, sort: function (e) { return e.name; } },
          { id: "consts", title: "Constant", width: "22%", html: function (e) { return chips(e.consts || [], 2); } },
          { id: "uses", title: "Uses", width: "60px", cls: "num r", html: function (e) { return U.fmt((e.uses || []).length); }, sort: function (e) { return -(e.uses || []).length; } },
          { id: "where", title: "Named in", html: function (e) {
            var fs = uniq((e.uses || []).map(function (u) { return "file:" + u.file; }));
            return chips(fs, 3);
          } }
        ]
      };
    }
  };

  // ----- the view

  var S = null;
  function render(el, r) {
    var all = surfaces();
    if (!all.length) { el.innerHTML = '<div class="page narrow"><h1>Interfaces</h1><p class="muted">The model names no tables of entry points.</p></div>'; return; }
    var s = D.surface(r.rest) || all[0];
    if (!r.rest || !D.surface(r.rest)) { X.router.replace(X.router.hrefKeep("interfaces", s.id, { q: r.params.q, sel: r.params.sel })); return; }
    S = { el: el, s: s };
    var kinds = KIND_ORDER.filter(function (k) { return all.some(function (x) { return x.kind === k; }); });
    var sameKind = all.filter(function (x) { return x.kind === s.kind; });
    el.innerHTML = '<div class="if">' +
      '<div class="if-tabs">' + kinds.map(function (k) {
        var first = all.filter(function (x) { return x.kind === k; })[0];
        return '<a href="' + esc(X.router.hrefKeep("interfaces", first.id)) + '"' + (k === s.kind ? ' class="on"' : "") + ">" + U.icon(iconOf(k)) + esc(KIND_TITLE[k] || k) + "</a>";
      }).join("") + "</div>" +
      (sameKind.length > 1 ? '<div class="if-sub">' + sameKind.map(function (x) {
        return '<a href="' + esc(X.router.hrefKeep("interfaces", x.id)) + '"' + (x.id === s.id ? ' class="on"' : "") + ">" + esc(x.title) + "</a>";
      }).join("") + "</div>" : "") +
      '<div class="if-head"><h1>' + esc(s.title) + "</h1>" + (s.summary ? '<p class="lead">' + X.doc.inline(s.summary, {}) + "</p>" : "") + sourceNote(s) + "</div>" +
      '<div class="if-table"></div></div>';
    var spec = (TABLES[s.kind] || TABLES.env)(s);
    S.spec = spec;
    S.table = X.table.create(el.querySelector(".if-table"), {
      columns: spec.columns, rows: spec.rows, key: spec.key, text: spec.text, tree: spec.tree,
      query: r.params.q || "", selected: r.params.sel || null,
      placeholder: "Filter " + (KIND_TITLE[s.kind] || "rows").toLowerCase() + "…",
      onQuery: function (q) { X.router.setParams({ q: q || null }); },
      onSelect: function (row) {
        X.router.setParams({ sel: spec.key(row) });
        X.inspector.open(spec.ref(row));
      }
    });
    if (r.params.sel) S.table.select(r.params.sel, true);
  }
  function iconOf(k) { return { commands: "command", rpc: "rpc", http: "route", events: "event", config: "config", env: "env" }[k] || "surface"; }
  function sourceNote(s) {
    var where = [];
    if (s.consts) where.push("constants of " + R.chip(s.consts) + (s.prefix ? " named <code>" + esc(s.prefix) + "…</code>" : ""));
    if (s.handlersIn && s.handlersIn.length) where.push("handlers registered in " + s.handlersIn.map(function (x) { return R.chip(x); }).join(" "));
    if (s.listedIn && s.listedIn.length) where.push("listed in " + s.listedIn.map(function (x) { return R.chip(x); }).join(" "));
    if (s.table) where.push("the table of " + R.chip(s.table));
    if (s.root) where.push("the fields of " + R.chip(s.root));
    if (s.pattern) where.push("names matching <code>" + esc(s.pattern) + "</code>");
    return where.length ? '<p class="small muted src-note">From ' + where.join("; ") + ".</p>" : "";
  }
  function update(r) {
    if (!S || r.rest !== S.s.id) return false;
    if (r.params.sel) S.table.select(r.params.sel, false);
    return true;
  }

  X.router.register("interfaces", { nav: "interfaces", title: function (r) { var s = D.surface(r.rest); return s ? s.title : "Interfaces"; }, render: render, update: update, unmount: function () { S = null; } });

  // ----- inspector views

  function siteList(sites, opts) {
    opts = opts || {};
    if (!sites || !sites.length) return '<p class="muted small">None.</p>';
    return '<ul class="sites">' + sites.map(function (s) {
      var lead = opts.handler && s.handler ? R.chip(s.handler) : s.in ? R.chip(s.in) : '<span class="muted small">package level</span>';
      return "<li>" + lead + X.symbolPanel.siteLink(s.file, s.line, null).replace("</a>", '<span class="mono small muted">' + esc(s.file) + "</span></a>") + "</li>";
    }).join("") + "</ul>";
  }
  function sec(title, n, body) {
    return '<section class="sp-sec"><h3 class="sp-h">' + esc(title) + (n != null ? ' <span class="count">' + n + "</span>" : "") + "</h3>" + body + "</section>";
  }
  function pad(body, html) {
    var el = U.h("div.insp-pad.sp", { html: html });
    body.appendChild(el);
    el.addEventListener("click", function (ev) {
      var a = ev.target.closest("a.site");
      if (!a || U.isModifiedClick(ev) || ev.button !== 0) return;
      ev.preventDefault();
      X.router.go(X.router.hrefKeep("code", a.getAttribute("data-file"), { line: a.getAttribute("data-line") }));
    });
    return el;
  }

  X.inspector.register("command", function (info, body) {
    var c = info.obj;
    var inherited = [], p = c.parent ? D.cmd(c.parent) : null;
    while (p) { (p.flags || []).forEach(function (f) { if (f.persistent) inherited.push({ f: f, from: p.path }); }); p = p.parent ? D.cmd(p.parent) : null; }
    var flagRow = function (f, from) {
      return "<tr><td><code>--" + esc(f.name) + "</code>" + (f.shorthand ? " <code>-" + esc(f.shorthand) + "</code>" : "") + (f.hidden ? ' <span class="tag">hidden</span>' : "") +
        '</td><td><code class="small muted">' + esc(f.type) + "</code></td><td>" + esc(f.usage || "") +
        (f.default && f.default !== "false" && f.default !== "[]" ? ' <span class="muted small">(default ' + esc(f.default) + ")</span>" : "") +
        (from ? ' <span class="muted small">from ' + esc(from) + "</span>" : "") + "</td></tr>";
    };
    var h = '<pre class="sp-sig">' + esc((c.parent ? c.parent + " " : "") + c.use) + "</pre>";
    if (c.hidden || c.deprecated) h += "<p>" + (c.hidden ? '<span class="tag">hidden</span> ' : "") + (c.deprecated ? '<span class="tag warn">deprecated</span> ' + esc(c.deprecated) : "") + "</p>";
    if (c.short) h += '<p class="lead">' + esc(c.short) + "</p>";
    if (c.long && c.long.trim() !== (c.short || "").trim()) h += '<div class="cmd-long">' + esc(c.long) + "</div>";
    if (c.aliases && c.aliases.length) h += '<p class="small"><span class="muted">aliases</span> ' + c.aliases.map(function (a) { return "<code>" + esc(a) + "</code>"; }).join(" ") + "</p>";
    if (c.actor) h += '<div class="sp-runs"><span class="sp-label">Runs as</span>' + R.actorList([c.actor]) + "</div>";
    if (c.run) h += sec("Run function", null, '<div class="chips">' + R.chip(c.run) + X.symbolPanel.siteLink(c.runFile, c.runLine, null) + (c.runLiteral ? '<span class="muted small">a function literal in it</span>' : "") + "</div>");
    if ((c.flags || []).length || inherited.length) {
      h += sec("Flags", (c.flags || []).length, '<table class="mini">' + (c.flags || []).map(function (f) { return flagRow(f); }).join("") +
        inherited.map(function (x) { return flagRow(x.f, x.from); }).join("") + "</table>");
    }
    if (c.example) h += sec("Example", null, '<pre class="doc-code">' + esc(c.example) + "</pre>");
    if (c.subcommands && c.subcommands.length) h += sec("Subcommands", c.subcommands.length, '<div class="chips">' + c.subcommands.map(function (x) { return R.chip("cmd:" + x, { label: x.split(" ").pop() }); }).join("") + "</div>");
    if (c.parent) h += sec("Parent", null, R.chip("cmd:" + c.parent));
    pad(body, h);
  });

  X.inspector.register("rpc", function (info, body) {
    var m = info.obj;
    var h = '<div class="chips">' + R.chip(m.const) + (m.surfaces || []).map(function (s) { return R.chip("surface:" + s); }).join("") + "</div>";
    if (m.doc) h += '<div class="doc sp-doc">' + X.doc.render(m.doc, {}) + "</div>";
    h += sec("Handled by", (m.handlers || []).length, siteList(m.handlers, { handler: true }));
    h += sec("Called from", (m.callers || []).length, siteList(m.callers));
    if ((m.listed || []).length) h += sec("Listed in", m.listed.length, siteList(m.listed));
    if ((m.tests || []).length) h += sec("In tests", m.tests.length, siteList(m.tests));
    pad(body, h);
  });

  X.inspector.register("route", function (info, body) {
    var r = info.obj;
    var h = '<pre class="sp-sig">' + esc(r.method + " " + r.path) + "</pre>" +
      '<dl class="facts">' +
      (r.access ? "<dt>Access</dt><dd>" + (r.accessRef ? R.chip(r.accessRef, { label: r.access }) : esc(r.access)) + "</dd>" : "") +
      (r.crossSite ? "<dt>Cross-site</dt><dd>" + (r.crossSiteRef ? R.chip(r.crossSiteRef, { label: r.crossSite }) : esc(r.crossSite)) + "</dd>" : "") +
      (r.handler ? "<dt>Handler</dt><dd>" + R.chip(r.handler) + "</dd>" : "") +
      "<dt>Table</dt><dd>" + X.symbolPanel.siteLink(r.file, r.line, null).replace("</a>", '<span class="mono small muted">' + esc(r.file) + "</span></a>") + "</dd></dl>";
    var hs = r.handler && D.sym(r.handler);
    if (hs && hs.doc) h += sec("The handler", null, '<div class="doc">' + X.doc.render(hs.doc, { pkg: hs.package }) + "</div>");
    pad(body, h);
  });

  X.inspector.register("event", function (info, body) {
    var e = info.obj;
    var h = '<div class="chips">' + R.chip(e.const) + "</div>";
    if (e.doc) h += '<div class="doc sp-doc">' + X.doc.render(e.doc, {}) + "</div>";
    h += sec("Written by", (e.writers || []).length, siteList(e.writers));
    h += sec("Read by", (e.readers || []).length, siteList(e.readers));
    if ((e.other || []).length) h += sec("Other uses", e.other.length, siteList(e.other));
    if ((e.tests || []).length) h += sec("In tests", e.tests.length, siteList(e.tests));
    pad(body, h);
  });

  X.inspector.register("config", function (info, body) {
    var c = info.obj, s = D.sym(c.field);
    var kids = D.configChildren(c.key);
    var h = '<pre class="sp-sig">' + esc(c.key) + "  " + esc(c.type) + "</pre>" +
      '<div class="chips"><span class="tag">' + esc(c.kind) + "</span>" + R.chip(c.field) + (c.parent ? R.chip("config:" + c.parent) : "") + R.chip("surface:" + c.surface) + "</div>";
    if (c.doc) h += '<div class="doc sp-doc">' + X.doc.render(c.doc, { pkg: s ? s.package : "" }) + "</div>";
    if (kids.length) h += sec("Keys", kids.length, '<div class="chips">' + kids.map(function (k) { return R.chip("config:" + k.key, { label: k.key.split(".").pop() }); }).join("") + "</div>");
    h += '<section class="sp-sec"><h3 class="sp-h">Where it takes effect' + (s ? ' <span class="count">' + (s.uses || 0) + "</span>" : "") + '</h3><div class="cfg-uses"><div class="loading"><span class="spinner"></span>Loading…</div></div></section>';
    var el = pad(body, h);
    if (!s) { el.querySelector(".cfg-uses").textContent = ""; return; }
    D.chunkOfPackage(s.package).then(function (ch) {
      var uses = ((ch.usedBy || {})[s.ref] || []).filter(function (u) { return X.settings.get("tests") || !u.test; });
      var box = el.querySelector(".cfg-uses");
      box.innerHTML = "";
      if (!uses.length) { box.innerHTML = '<p class="muted small">The field is not used outside its declaration.</p>'; return; }
      box.appendChild(X.symbolPanel.useGroups(uses, 200));
    }, function (e) { el.querySelector(".cfg-uses").textContent = e.message; });
  });

  X.inspector.register("env", function (info, body) {
    var e = info.obj;
    var h = (e.consts && e.consts.length ? '<div class="chips"><span class="muted small">constants</span>' + e.consts.map(function (c) { return R.chip(c); }).join("") + "</div>" : "") +
      sec("Named in", (e.uses || []).length, siteList(e.uses));
    if (e.consts && e.consts.length) h += sec("Uses of the constants", null, '<div class="env-uses"><div class="loading"><span class="spinner"></span>Loading…</div></div>');
    var el = pad(body, h);
    if (!e.consts || !e.consts.length) return;
    Promise.all(e.consts.map(function (c) { var s = D.sym(c); return s ? D.chunkOfPackage(s.package).then(function (ch) { return ((ch.usedBy || {})[c] || []); }) : Promise.resolve([]); }))
      .then(function (lists) {
        var uses = [].concat.apply([], lists).filter(function (u) { return X.settings.get("tests") || !u.test; });
        var box = el.querySelector(".env-uses");
        box.innerHTML = "";
        if (!uses.length) box.innerHTML = '<p class="muted small">None.</p>'; else box.appendChild(X.symbolPanel.useGroups(uses, 200));
      });
  });
})();
