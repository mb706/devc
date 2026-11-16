// References: resolving any reference of explorer/README.md's syntax,
// rendering it as a chip, and the click behaviour every chip shares.
//
//   X.refs.resolve(ref)        -> info {ref, kind, key, obj, label, title,
//                                 zones, file, line, missing}
//   X.refs.chip(ref, opts)     -> HTML string of a chip. opts: label (text
//                                 instead of the short label), cls (extra
//                                 classes), plain (no icon), act ("full":
//                                 a plain click opens in full).
//   X.refs.href(ref)           -> the URL of the reference's full view
//   X.refs.card(ref)           -> HTML of its hover card
//   X.refs.define(kind, spec)  -> add or override a kind's label, icon,
//                                 zones, href or card (X3: model kinds)
//   X.refs.zoneMark(zones), X.refs.actorList(ids), X.refs.badge(sym)
//
// Any element with data-ref="<reference>" behaves as a reference: hover
// shows the card, a click inspects, Ctrl/Cmd+click opens in full, middle
// click follows its href. A container that handles clicks itself calls
// preventDefault() on the event; the shared handler then does nothing.
X.refs = (function () {
  "use strict";
  var U = X.util, D = X.data, esc = U.esc;

  var PREFIX = {
    pkg: "package", file: "file", cmd: "command", rpc: "rpc", http: "route", event: "event",
    config: "config", env: "env", doc: "doc", finding: "finding", chunk: "chunk",
    concern: "concern", flow: "flow", term: "term", actor: "actor", zone: "zone", channel: "channel",
    diagram: "diagram", surface: "surface", layer: "layer", group: "group"
  };
  var KIND_PREFIX = {};
  Object.keys(PREFIX).forEach(function (p) { KIND_PREFIX[PREFIX[p]] = p; });

  function parse(ref) {
    var m = /^([a-z]+):([\s\S]*)$/.exec(ref || "");
    if (m && PREFIX[m[1]]) return { kind: PREFIX[m[1]], key: m[2] };
    return { kind: "symbol", key: ref || "" };
  }
  function make(kind, key) { return kind === "symbol" ? key : KIND_PREFIX[kind] + ":" + key; }

  function surfaceOfKind(kind) {
    var s = (D.model.surfaces || []).filter(function (x) { return x.kind === kind; })[0];
    return s ? s.id : "";
  }
  function headingOf(doc, key) {
    if (!doc || !key) return null;
    return doc.headings.filter(function (h) { return h.number === key || h.anchor === key; })[0] || null;
  }
  function modelTitle(o, key) { return (o && (o.title || o.label)) || key; }

  // Kind definitions. obj(key) finds the object; the rest describe it.
  var kinds = {
    symbol: {
      obj: function (k) { return D.sym(k); },
      label: function (o, k) {
        if (!o) return k;
        return U.lastElem(o.package) + k.slice(o.package.length);
      },
      zones: function (o) { return o && o.actors ? D.zonesOfActors(o.actors) : []; },
      loc: function (o) { return o ? { file: o.file, line: o.line } : null; },
      href: function (o, k) { return o ? X.router.href("code", o.file, { sym: k }) : ""; }
    },
    package: {
      icon: "package",
      obj: function (k) { return D.pkg(k); },
      label: function (o, k) { return k; },
      zones: function (o) { return o ? D.zonesOfActors(D.actorsOfPackage(o)) : []; },
      loc: function (o) { return o && o.docFile ? { file: o.docFile, line: 1 } : null; },
      href: function (o, k) { return X.router.href("pkg", k); }
    },
    file: {
      icon: "file",
      obj: function (k) { return D.file(k.split("~")[0]); },
      label: function (o, k) { return U.lastElem(k.split("~")[0]); },
      loc: function (o, k, r) { return o ? { file: o.path, line: (r && r.line) || 1 } : null; },
      href: function (o, k, r) {
        var p = k.split("~")[0];
        return X.router.href("code", p, r && r.line ? { line: r.line } : null);
      }
    },
    command: {
      icon: "command",
      obj: function (k) { return D.cmd(k); },
      label: function (o, k) { return k; },
      zones: function (o) { return o && o.actor ? D.zonesOfActors([o.actor]) : []; },
      loc: function (o) { return o && o.runFile ? { file: o.runFile, line: o.runLine } : null; },
      href: function (o, k) { return X.router.href("interfaces", surfaceOfKind("commands"), { sel: k }); }
    },
    rpc: {
      icon: "rpc",
      obj: function (k) { return D.rpc(k); },
      label: function (o, k) { return k; },
      loc: function (o) { var s = o && D.sym(o.const); return s ? { file: s.file, line: s.line } : null; },
      href: function (o, k) { return X.router.href("interfaces", (o && o.surfaces && o.surfaces[0]) || surfaceOfKind("rpc"), { sel: k }); }
    },
    route: {
      icon: "route",
      obj: function (k) { return D.route(k); },
      label: function (o, k) { return k; },
      loc: function (o) { return o ? { file: o.file, line: o.line } : null; },
      href: function (o, k) { return X.router.href("interfaces", (o && o.surface) || surfaceOfKind("http"), { sel: k }); }
    },
    event: {
      icon: "event",
      obj: function (k) { return D.event(k); },
      label: function (o, k) { return k; },
      loc: function (o) { var s = o && D.sym(o.const); return s ? { file: s.file, line: s.line } : null; },
      href: function (o, k) { return X.router.href("interfaces", (o && o.surface) || surfaceOfKind("events"), { sel: k }); }
    },
    config: {
      icon: "config",
      obj: function (k) { return D.config(k); },
      label: function (o, k) { return k; },
      loc: function (o) { var s = o && D.sym(o.field); return s ? { file: s.file, line: s.line } : null; },
      href: function (o, k) { return X.router.href("interfaces", (o && o.surface) || surfaceOfKind("config"), { sel: k }); }
    },
    env: {
      icon: "env",
      obj: function (k) { return D.env(k); },
      label: function (o, k) { return k; },
      href: function (o, k) { return X.router.href("interfaces", surfaceOfKind("env"), { sel: k }); }
    },
    doc: {
      icon: "doc",
      obj: function (k) { return D.document(k.split("#")[0]); },
      label: function (o, k) {
        var h = headingOf(o, k.split("#")[1]);
        if (h) return (h.number ? "§" + h.number + " " : "") + h.text.replace(/^[0-9.]+\s+/, "");
        return o ? o.title : k;
      },
      href: function (o, k) {
        var parts = k.split("#");
        var h = headingOf(o, parts[1]);
        return X.router.href("docs", parts[0], h ? { h: h.anchor } : null);
      }
    },
    finding: {
      icon: "finding",
      obj: function (k) { return D.finding(k); },
      label: function (o, k) { return k; },
      loc: function (o) { return o ? { file: o.file, line: o.line } : null; },
      href: function (o, k) { return X.router.href("docs", "findings", { sel: k }); }
    },
    chunk: {
      icon: "chunk",
      obj: function (k) { return D.workOrder(k); },
      label: function (o, k) { return k; },
      loc: function (o) { return o ? { file: o.file, line: 1 } : null; },
      href: function (o, k) { return o ? X.router.href("docs", o.file) : ""; }
    },
    actor: {
      icon: "actor",
      obj: function (k) { return D.actor(k); },
      label: modelTitle,
      zones: function (o) { return o ? D.zonesOfActors([o.id]) : []; },
      href: function (o, k) { return X.router.href("runtime", "", { sel: "actor:" + k }); }
    },
    zone: {
      icon: "zone",
      obj: function (k) { return D.zone(k); },
      label: modelTitle,
      zones: function (o) { return o ? [o] : []; },
      href: function (o, k) { return X.router.href("runtime", "", { sel: "zone:" + k }); }
    },
    channel: {
      icon: "channel",
      obj: function (k) { return D.channel(k); },
      label: modelTitle,
      zones: function (o) { return o ? D.zonesOfActors([o.from, o.to]) : []; },
      href: function (o, k) { return X.router.href("runtime", "", { sel: "channel:" + k }); }
    },
    concern: { icon: "concern", obj: function (k) { return D.concern(k); }, label: modelTitle, href: function (o, k) { return X.router.href("concerns", k); } },
    flow: { icon: "flow", obj: function (k) { return D.flow(k); }, label: modelTitle, href: function (o, k) { return X.router.href("flows", k); } },
    term: { icon: "term", obj: function (k) { return D.term(k); }, label: modelTitle, href: function (o, k) { return X.router.href("glossary", "", { sel: k }); } },
    diagram: {
      icon: "diagram",
      obj: function (k) {
        var r = D.core.refs["diagram:" + k];
        var key = r ? r.key : k, parts = key.split("/");
        var c = D.concern(parts[0]);
        return c && (c.diagrams || []).filter(function (d) { return d.id === parts[1]; })[0];
      },
      label: modelTitle,
      href: function (o, k) {
        var r = D.core.refs["diagram:" + k], key = r ? r.key : k;
        return X.router.href("concerns", key.split("/")[0], { diagram: key.split("/")[1] });
      }
    },
    surface: { icon: "surface", obj: function (k) { return D.surface(k); }, label: modelTitle, href: function (o, k) { return X.router.href("interfaces", k); } },
    layer: { icon: "layer", obj: function (k) { return D.layer(k); }, label: modelTitle, href: function (o, k) { return X.router.href("packages", "", { layer: k }); } },
    group: { icon: "group", obj: function (k) { return D.group(k); }, label: modelTitle, href: function (o, k) { return X.router.href("packages", "", { group: k }); } }
  };

  function define(kind, spec) {
    var k = kinds[kind] = kinds[kind] || {};
    Object.keys(spec).forEach(function (n) { k[n] = spec[n]; });
    cache.clear();
  }

  var cache = new Map();
  function resolve(ref) {
    var hit = cache.get(ref);
    if (hit) return hit;
    var p = parse(ref), k = kinds[p.kind] || {};
    var r = D.core.refs[ref] || null;
    var obj = k.obj ? k.obj(p.key) : null;
    var loc = k.loc ? k.loc(obj, p.key, r) : null;
    if (r && r.file && !loc) loc = { file: r.file, line: r.line || 1 };
    var info = {
      ref: ref, kind: p.kind, key: p.key, obj: obj, resolved: r,
      label: k.label ? k.label(obj, p.key) : p.key,
      title: p.kind === "symbol" ? p.key : ref,
      zones: (k.zones && obj) ? k.zones(obj) : [],
      file: loc ? loc.file : "", line: loc ? loc.line : 0,
      missing: !obj
    };
    cache.set(ref, info);
    return info;
  }

  function href(ref) {
    var info = resolve(ref), k = kinds[info.kind] || {};
    try { return (k.href && k.href(info.obj, info.key, info.resolved)) || ""; } catch (e) { return ""; }
  }

  // ----- small HTML parts

  var LETTER = { func: "ƒ", method: "m", type: "T", var: "v", const: "c", field: "·", imethod: "m" };
  function badge(s) {
    if (!s) return '<i class="kb">?</i>';
    var l = LETTER[s.kind] || "?", cls = s.kind;
    if (s.kind === "type" && s.typeKind === "interface") { l = "I"; cls = "iface"; }
    else if (s.kind === "type" && s.typeKind === "struct") { l = "S"; }
    return '<i class="kb kb-' + cls + '" title="' + esc(kindText(s)) + '">' + l + "</i>";
  }
  function kindText(s) {
    if (!s) return "";
    switch (s.kind) {
      case "func": return "function";
      case "method": return "method";
      case "type": return s.typeKind && s.typeKind !== "named" && s.typeKind !== "basic" ? s.typeKind + " type" : "type";
      case "var": return "variable";
      case "const": return "constant";
      case "field": return "field";
      case "imethod": return "interface method";
    }
    return s.kind;
  }

  // zoneMark is a thin strip in the zone colours, one band per zone.
  function zoneMark(zones) {
    if (!zones || !zones.length) return "";
    return '<i class="zm" title="' + esc("runs in: " + zones.map(function (z) { return z.title; }).join(", ")) + '">' +
      zones.map(function (z) { return '<b class="z-' + esc(z.trust) + '"></b>'; }).join("") + "</i>";
  }
  function zoneDot(z, title) {
    return z ? '<i class="zd z-' + esc(z.trust) + '" title="' + esc(title || z.title) + '"></i>' : "";
  }

  // actorList shows actors grouped by zone: a dot per zone, then the
  // actors' names as chips.
  function actorList(ids, opts) {
    opts = opts || {};
    var byZone = new Map();
    (ids || []).forEach(function (id) {
      var z = D.zoneOfActor(id);
      var key = z ? z.id : "";
      if (!byZone.has(key)) byZone.set(key, { zone: z, ids: [] });
      byZone.get(key).ids.push(id);
    });
    var order = (D.model.zones || []).map(function (z) { return z.id; });
    var groups = Array.from(byZone.values()).sort(function (a, b) {
      return order.indexOf(a.zone ? a.zone.id : "") - order.indexOf(b.zone ? b.zone.id : "");
    });
    return '<span class="actors">' + groups.map(function (g) {
      return '<span class="az' + (g.zone ? " z-" + esc(g.zone.trust) : "") + '">' +
        (g.zone ? '<span class="azt" title="' + esc(g.zone.summary || "") + '">' + esc(g.zone.title) + "</span>" : "") +
        g.ids.map(function (id) { return chip("actor:" + id, { cls: "flat", noZone: true }); }).join("") + "</span>";
    }).join("") + "</span>";
  }

  function chip(ref, opts) {
    opts = opts || {};
    var info = resolve(ref), k = kinds[info.kind] || {};
    var icon = "";
    if (!opts.plain) {
      if (info.kind === "symbol") icon = badge(info.obj);
      else icon = U.icon(k.icon || "file");
    }
    var label = opts.label != null ? opts.label : info.label;
    var cls = "chip k-" + info.kind + (info.missing ? " missing" : "") + (opts.cls ? " " + opts.cls : "");
    var h = href(ref);
    return '<a class="' + cls + '" data-ref="' + esc(ref) + '"' + (h ? ' href="' + esc(h) + '"' : "") +
      (opts.act ? ' data-act="' + esc(opts.act) + '"' : "") + ">" +
      (opts.noZone ? "" : zoneMark(info.zones)) + icon + '<span class="cl">' + esc(label) + "</span></a>";
  }

  // ----- hover cards

  function kindLine(info) {
    var o = info.obj, k = info.kind;
    if (k === "symbol" && o) {
      var parent = o.parent ? D.sym(o.parent) : null;
      return badge(o) + "<span>" + esc(kindText(o)) + (parent ? " of <code>" + esc(parent.name) + "</code>" : "") +
        ' · <code>' + esc(o.package) + "</code>" + (o.test ? ' <span class="tag">test</span>' : "") + "</span>";
    }
    var names = {
      package: "package", file: "file", command: "command", rpc: "RPC method", route: "route", event: "event type",
      config: "configuration key", env: "environment variable", doc: "document", finding: "review finding",
      chunk: "work order", actor: "actor", zone: "zone", channel: "channel", concern: "concern", flow: "flow",
      term: "term", diagram: "diagram", surface: "table of entry points", layer: "layer", group: "group"
    };
    return U.icon((kinds[k] || {}).icon || "file") + "<span>" + esc(names[k] || k) + "</span>";
  }

  function docPara(text, ctx) {
    var p = U.firstParagraph(text);
    if (!p) return "";
    if (p.length > 420) p = p.slice(0, 400).replace(/\s+\S*$/, "") + " …";
    return '<div class="hc-doc">' + X.doc.inline(p, ctx) + "</div>";
  }
  function where(info) {
    if (!info.file) return "";
    return '<div class="hc-where mono">' + esc(info.file) + (info.line ? ":" + info.line : "") + "</div>";
  }

  var cards = {
    symbol: function (info) {
      var o = info.obj;
      var h = '<div class="hc-sig mono">' + X.doc.highlightGo(o.signature) + "</div>";
      if (o.value && o.kind === "const" && o.signature.indexOf(o.value) < 0) h += '<div class="hc-row"><span class="muted">value</span> <code>' + esc(o.value) + "</code></div>";
      h += docPara(o.doc, { pkg: o.package });
      h += where(info);
      var meta = [];
      if (o.uses) meta.push(U.plural(o.uses, "use"));
      if (meta.length) h += '<div class="hc-row muted">' + meta.join(" · ") + "</div>";
      if (o.actors && o.actors.length) h += '<div class="hc-runs"><span class="muted">runs in</span> ' + actorList(o.actors) + "</div>";
      return h;
    },
    package: function (info) {
      var o = info.obj, L = D.layer(o.layer), G = D.group(o.group);
      var h = o.role ? '<div class="hc-doc">' + X.doc.inline(o.role, { pkg: o.path }) + "</div>" : docPara(o.doc, { pkg: o.path });
      var facts = [];
      if (L) facts.push("layer " + esc(L.title));
      if (G) facts.push(esc(G.title));
      facts.push(U.fmt(o.lines.code) + " lines" + (o.testLines.code ? " + " + U.fmt(o.testLines.code) + " in tests" : ""));
      facts.push(U.plural(o.files.length, "file"));
      h += '<div class="hc-row muted">' + facts.join(" · ") + "</div>";
      var acts = D.actorsOfPackage(o);
      if (acts.length) h += '<div class="hc-runs"><span class="muted">runs in</span> ' + actorList(acts) + "</div>";
      return h;
    },
    file: function (info) {
      var o = info.obj;
      var h = '<div class="hc-sig mono">' + esc(o.path) + "</div>";
      if (o.doc) h += docPara(o.doc, { pkg: o.package });
      var facts = [U.fmt(o.lines.code + o.lines.comment + o.lines.blank) + " lines", o.lang];
      if (o.package) facts.push("package " + o.package);
      if (o.test) facts.push("test");
      h += '<div class="hc-row muted">' + esc(facts.join(" · ")) + "</div>";
      return h;
    },
    command: function (info) {
      var o = info.obj;
      var h = '<div class="hc-sig mono">' + esc(o.parent ? o.parent + " " + o.use : o.use) + "</div>";
      if (o.short) h += '<div class="hc-doc">' + esc(o.short) + "</div>";
      var facts = [];
      if (o.hidden) facts.push('<span class="tag">hidden</span>');
      if (o.flags && o.flags.length) facts.push(U.plural(o.flags.length, "flag"));
      if (o.subcommands && o.subcommands.length) facts.push(U.plural(o.subcommands.length, "subcommand"));
      if (facts.length) h += '<div class="hc-row muted">' + facts.join(" · ") + "</div>";
      if (o.run) h += '<div class="hc-row"><span class="muted">runs</span> ' + chip(o.run) + "</div>";
      if (o.actor) h += '<div class="hc-runs"><span class="muted">as</span> ' + actorList([o.actor]) + "</div>";
      return h;
    },
    rpc: function (info) {
      var o = info.obj;
      var h = docPara(o.doc, {});
      h += '<div class="hc-row muted">' + U.plural((o.handlers || []).length, "handler") + " · " +
        U.plural((o.callers || []).length, "caller") + (o.surfaces ? " · " + esc(o.surfaces.map(function (s) { var x = D.surface(s); return x ? x.title : s; }).join(", ")) : "") + "</div>";
      (o.handlers || []).slice(0, 3).forEach(function (s) { if (s.handler) h += '<div class="hc-row"><span class="muted">handled by</span> ' + chip(s.handler) + "</div>"; });
      return h;
    },
    route: function (info) {
      var o = info.obj;
      var h = "";
      var facts = [];
      if (o.access) facts.push("access <code>" + esc(o.access) + "</code>");
      if (o.crossSite) facts.push("cross-site <code>" + esc(o.crossSite) + "</code>");
      if (facts.length) h += '<div class="hc-row">' + facts.join(" · ") + "</div>";
      if (o.handler) h += '<div class="hc-row"><span class="muted">handled by</span> ' + chip(o.handler) + "</div>";
      h += where(info);
      return h;
    },
    event: function (info) {
      var o = info.obj;
      return docPara(o.doc, {}) + '<div class="hc-row muted">' + U.plural((o.writers || []).length, "writer") + " · " +
        U.plural((o.readers || []).length, "reader") + "</div>";
    },
    config: function (info) {
      var o = info.obj, s = D.sym(o.field);
      var h = '<div class="hc-sig mono">' + esc(o.type) + ' <span class="muted">(' + esc(o.kind) + ")</span></div>";
      h += docPara(o.doc, { pkg: s ? s.package : "" });
      if (s && s.uses) h += '<div class="hc-row muted">' + U.plural(s.uses, "use") + " of the field</div>";
      return h;
    },
    env: function (info) {
      var o = info.obj;
      var fs = new Set((o.uses || []).map(function (u) { return u.file; }));
      return '<div class="hc-row muted">' + U.plural((o.uses || []).length, "use") + " in " + U.plural(fs.size, "file") + "</div>";
    },
    doc: function (info) {
      var o = info.obj;
      return '<div class="hc-row">' + esc(o.title) + '</div><div class="hc-where mono">' + esc(o.path) + "</div>";
    },
    finding: function (info) {
      var o = info.obj;
      var h = '<div class="hc-doc"><b>' + esc(o.title) + "</b></div>";
      if (o.meta) h += '<div class="hc-row muted">' + esc(o.meta) + "</div>";
      h += '<div class="hc-row muted">review ' + esc(o.review) + (o.response ? " · answered" : "") + "</div>";
      return h;
    },
    chunk: function (info) { return '<div class="hc-doc">' + esc(info.obj.title) + "</div>" + where(info); },
    actor: function (info) {
      var o = info.obj, z = D.zone(o.zone);
      var h = '<div class="hc-row">' + zoneDot(z) + " " + esc(z ? z.title : o.zone) + ' · <span class="muted">' + esc(o.kind) + "</span></div>";
      if (o.summary) h += '<div class="hc-doc">' + X.doc.inline(U.firstParagraph(o.summary), {}) + "</div>";
      return h;
    },
    zone: function (info) {
      var o = info.obj;
      return '<div class="hc-row">' + zoneDot(o) + ' <span class="muted">trust: ' + esc(o.trust) + "</span></div>" +
        (o.summary ? '<div class="hc-doc">' + X.doc.inline(U.firstParagraph(o.summary), {}) + "</div>" : "");
    }
  };
  function defaultCard(info) {
    var o = info.obj || {};
    var text = o.summary || o.text || o.carries || "";
    return text ? '<div class="hc-doc">' + X.doc.inline(U.firstParagraph(text), {}) + "</div>" : "";
  }

  function card(ref) {
    var info = resolve(ref), k = kinds[info.kind] || {};
    var head = '<div class="hc-kind">' + kindLine(info) + "</div>" +
      '<div class="hc-title mono">' + zoneMark(info.zones) + esc(info.kind === "symbol" ? info.key : (info.obj && info.obj.title && info.kind !== "doc") ? info.obj.title : info.key) + "</div>";
    if (info.missing) return head + '<div class="hc-row muted">Not found in this build.</div>';
    var body;
    try { body = (k.card || cards[info.kind] || defaultCard)(info); } catch (e) { body = ""; }
    return head + body;
  }

  // ----- the shared click behaviour

  function onClick(ev) {
    if (ev.defaultPrevented || ev.button !== 0) return;
    var el = ev.target.closest && ev.target.closest("[data-ref]");
    if (!el) return;
    var ref = el.getAttribute("data-ref");
    if (U.isModifiedClick(ev) || el.getAttribute("data-act") === "full") {
      var h = href(ref);
      if (h) { ev.preventDefault(); X.hovercard.hide(); X.router.go(h); }
      return;
    }
    if (ev.shiftKey) return;
    ev.preventDefault();
    X.hovercard.hide();
    X.inspector.open(ref);
  }
  document.addEventListener("click", onClick);
  // A middle click on a reference without a link (the source view's
  // identifiers) opens its full view in a new tab, like a link would.
  document.addEventListener("auxclick", function (ev) {
    if (ev.button !== 1) return;
    var el = ev.target.closest && ev.target.closest("[data-ref]");
    if (!el || el.hasAttribute("href")) return;
    var h = href(el.getAttribute("data-ref"));
    if (h) { ev.preventDefault(); window.open(location.href.split("#")[0] + h, "_blank", "noopener"); }
  });

  return {
    parse: parse, make: make, resolve: resolve, href: href, chip: chip, card: card, define: define,
    kinds: kinds, badge: badge, kindText: kindText, zoneMark: zoneMark, zoneDot: zoneDot, actorList: actorList
  };
})();
