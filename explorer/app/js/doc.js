// Text rendering shared by every view: Go doc comments, inline text with
// references, and the build's text patterns (finding IDs, work orders,
// plan, rule and spec sections) turned into reference chips.
//
//   X.doc.render(text, ctx)   -> HTML of a Go doc comment: paragraphs,
//                                headings ("# Title"), indented code
//                                blocks, lists. ctx.pkg resolves [Name]
//                                doc links in that package.
//   X.doc.inline(text, ctx)   -> HTML of one paragraph with references
//   X.doc.findRefs(text)      -> [{s, e, ref}] matches of the patterns
//   X.doc.linkify(text, ctx)  -> [{s, e, ref, code}] every reference found
//                                in plain text (patterns, [doc links],
//                                package paths, URLs, `code`)
//   X.doc.highlightGo(src)    -> HTML of a Go snippet (keywords, strings,
//                                comments, numbers); no links
X.doc = (function () {
  "use strict";
  var U = X.util, D = X.data, esc = U.esc;

  // ----- the build's patterns (schema.go, type Pattern)

  var patterns = (D.core.patterns || []).map(function (p) {
    var re = null;
    try { re = new RegExp(p.regex, "gd"); } catch (e) { re = null; }
    return { p: p, re: re, split: p.split ? p.split : "" };
  }).filter(function (x) { return x.re; });

  function lookupOK(p, key, ref) {
    if (p.lookup === "finding") return !!D.finding(key);
    if (p.lookup === "chunk") return !!D.workOrder(key);
    if (p.lookup === "doc") {
      var r = ref.slice(4), i = r.indexOf("#");
      var doc = D.document(i < 0 ? r : r.slice(0, i));
      if (!doc) return false;
      if (i < 0) return true;
      var h = r.slice(i + 1);
      return doc.headings.some(function (x) { return x.number === h || x.anchor === h; }) ||
        doc.headings.filter(function (x) { return x.anchor.indexOf(h + "-") === 0; }).length === 1;
    }
    return true;
  }

  // findRefs applies the patterns in order; a match that overlaps an
  // earlier match that produced a reference is skipped.
  function findRefs(text) {
    var taken = [], out = [];
    patterns.forEach(function (c) {
      var re = c.re, p = c.p, m;
      re.lastIndex = 0;
      while ((m = re.exec(text))) {
        if (!m[0]) { re.lastIndex++; continue; }
        var ms = m.index, me = ms + m[0].length;
        var gi = m.indices && m.indices[p.group], gtext = m[p.group];
        if (!gi || gtext == null) continue;
        var spans = [];
        if (c.split) {
          var sre = new RegExp(c.split, "g"), sm;
          while ((sm = sre.exec(gtext))) {
            if (!sm[0]) { sre.lastIndex++; continue; }
            var ref = p.target.replace("{}", sm[0]);
            if (lookupOK(p, sm[0], ref)) spans.push({ s: gi[0] + sm.index, e: gi[0] + sm.index + sm[0].length, ref: ref });
          }
        } else {
          var r2 = p.target.replace("{}", gtext);
          if (lookupOK(p, gtext, r2)) spans.push({ s: ms, e: me, ref: r2 });
        }
        if (!spans.length) continue;
        if (taken.some(function (t) { return t[0] < me && ms < t[1]; })) continue;
        taken.push([ms, me]);
        out.push.apply(out, spans);
      }
    });
    return out.sort(function (a, b) { return a.s - b.s; });
  }

  // ----- other references in plain text

  var pkgRe = (function () {
    var paths = D.core.packages.filter(function (p) { return !p.xtest; }).map(function (p) { return p.path; })
      .filter(function (p) { return p.indexOf("/") > 0; })
      .sort(function (a, b) { return b.length - a.length; })
      .map(function (p) { return p.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"); });
    if (!paths.length) return null;
    try {
      return new RegExp("(?<![\\w/.-])(" + paths.join("|") + ")((?:\\.[A-Za-z_]\\w*){1,2})?(?![\\w/-])", "g");
    } catch (e) { return null; }
  })();
  // File paths ("internal/box/spec.go") and, in a package's comments,
  // names of its files ("agent.go").
  var pathRe = /(?<![\w\/.-])((?:[\w.-]+\/)*[\w-]+(?:\.[\w-]+)*\.(?:go|js|css|html|toml|json|md|sh|yaml|yml|tmpl))(?![\w\/-])/g;
  var urlRe = /\bhttps?:\/\/[^\s<>()"']+[^\s<>()"'.,;:!?]/g;
  var docLinkRe = /\[(\*?)([A-Za-z_]\w*(?:\.[A-Za-z_]\w*){0,2})\](?![(:])/g;
  var codeRe = /`([^`\n]+)`/g;
  // [[reference]] and [[reference|text]] of the model's Markdown.
  var modelRefRe = /\[\[([^\]|\n]+)(?:\|([^\]\n]+))?\]\]/g;

  function resolveDocLink(name, ctx) {
    if (ctx && ctx.pkg && D.sym(ctx.pkg + "." + name)) return ctx.pkg + "." + name;
    var dot = name.indexOf(".");
    if (dot < 0) return null;
    var first = name.slice(0, dot), rest = name.slice(dot + 1);
    if (ctx && ctx.pkg && D.sym(ctx.pkg + "." + name)) return ctx.pkg + "." + name;
    var cands = D.core.packages.filter(function (p) { return p.name === first || U.lastElem(p.path) === first; });
    for (var i = 0; i < cands.length; i++) if (D.sym(cands[i].path + "." + rest)) return cands[i].path + "." + rest;
    return null;
  }
  function resolveCode(inner, ctx) {
    var t = inner.trim();
    if (D.pkg(t) && t.indexOf("/") > 0) return "pkg:" + t;
    if (D.sym(t)) return t;
    if (/^[\w/.-]+\.[a-z]+$/.test(t) && D.file(t)) return "file:" + t;
    if (ctx && ctx.pkg && /^[A-Za-z_][\w.]*$/.test(t) && D.sym(ctx.pkg + "." + t)) return ctx.pkg + "." + t;
    return null;
  }

  // linkify finds every span to render specially, earliest first,
  // without overlaps (patterns win, then code spans, then the rest).
  function linkify(text, ctx) {
    var spans = [];
    function free(s, e) { return !spans.some(function (x) { return x.s < e && s < x.e; }); }
    function scan(re, fn) {
      if (!re) return;
      re.lastIndex = 0;
      var m;
      while ((m = re.exec(text))) {
        if (!m[0]) { re.lastIndex++; continue; }
        var sp = fn(m);
        if (sp && free(sp.s, sp.e)) spans.push(sp);
      }
    }
    scan(modelRefRe, function (m) { return { s: m.index, e: m.index + m[0].length, ref: m[1].trim(), label: (m[2] || "").trim() || null, model: true }; });
    findRefs(text).forEach(function (sp) { if (free(sp.s, sp.e)) spans.push(sp); });
    scan(codeRe, function (m) { return { s: m.index, e: m.index + m[0].length, code: m[1], ref: resolveCode(m[1], ctx) }; });
    scan(urlRe, function (m) { return { s: m.index, e: m.index + m[0].length, url: m[0] }; });
    scan(docLinkRe, function (m) {
      var r = resolveDocLink(m[2], ctx);
      return r ? { s: m.index, e: m.index + m[0].length, ref: r, label: m[2] } : null;
    });
    scan(pathRe, function (m) {
      var name = m[1];
      if (name.indexOf("/") > 0) return D.file(name) ? { s: m.index, e: m.index + name.length, ref: "file:" + name } : null;
      if (ctx && ctx.pkg && D.file(ctx.pkg + "/" + name)) return { s: m.index, e: m.index + name.length, ref: "file:" + ctx.pkg + "/" + name };
      return null;
    });
    scan(pkgRe, function (m) {
      var ref = "pkg:" + m[1], end = m.index + m[1].length;
      if (m[2]) {
        var full = m[1] + m[2];
        if (D.sym(full)) { ref = full; end = m.index + full.length; }
      }
      return { s: m.index, e: end, ref: ref };
    });
    return spans.sort(function (a, b) { return a.s - b.s; });
  }

  // inline renders one run of text with its references as chips.
  function inline(text, ctx) {
    if (!text) return "";
    var spans = linkify(text, ctx), out = "", pos = 0;
    spans.forEach(function (sp) {
      out += esc(text.slice(pos, sp.s));
      var raw = text.slice(sp.s, sp.e);
      if (sp.url) out += '<a href="' + esc(sp.url) + '" target="_blank" rel="noopener noreferrer">' + esc(raw) + "</a>";
      else if (sp.code != null) out += sp.ref ? X.refs.chip(sp.ref, { cls: "inline code", label: sp.code, plain: true }) : "<code>" + esc(sp.code) + "</code>";
      else if (sp.model) out += X.refs.chip(sp.ref, { cls: "inline", label: sp.label || null });
      else out += X.refs.chip(sp.ref, { cls: "inline", label: sp.label || raw, plain: true });
      pos = sp.e;
    });
    return out + esc(text.slice(pos));
  }

  // ----- Go doc comments

  var LIST_RE = /^[ \t]*([-*+•]|[0-9]+[.)])[ \t]+/;
  function render(text, ctx) {
    if (!text) return "";
    var lines = String(text).replace(/\s+$/, "").split("\n");
    var out = [], i = 0, para = [];
    function flush() {
      if (!para.length) return;
      var t = para.join(" ");
      if (para.length === 1 && /^# \S/.test(para[0])) out.push("<h4>" + inline(para[0].slice(2), ctx) + "</h4>");
      else out.push("<p>" + inline(t, ctx) + "</p>");
      para = [];
    }
    while (i < lines.length) {
      var l = lines[i];
      if (!l.trim()) { flush(); i++; continue; }
      if (/^[ \t]/.test(l)) {
        flush();
        var block = [];
        while (i < lines.length && (/^[ \t]/.test(lines[i]) || (!lines[i].trim() && i + 1 < lines.length && /^[ \t]/.test(lines[i + 1])))) {
          block.push(lines[i]); i++;
        }
        out.push(LIST_RE.test(block[0]) ? renderList(block, ctx) : renderCode(block));
        continue;
      }
      if (LIST_RE.test(l) && !para.length) {
        // An unindented list (gofmt would indent it): items until a blank.
        var items = [];
        while (i < lines.length && lines[i].trim()) { items.push(lines[i]); i++; }
        out.push(renderList(items, ctx));
        continue;
      }
      para.push(l.trim());
      i++;
    }
    flush();
    return out.join("");
  }
  function renderCode(block) {
    var ind = null;
    block.forEach(function (l) {
      if (!l.trim()) return;
      var m = /^[ \t]*/.exec(l)[0];
      if (ind === null || m.length < ind.length) ind = m;
    });
    var body = block.map(function (l) { return l.slice((ind || "").length); }).join("\n");
    return '<pre class="doc-code">' + esc(body) + "</pre>";
  }
  function renderList(block, ctx) {
    var items = [], ordered = /^[ \t]*[0-9]/.test(block[0]);
    block.forEach(function (l) {
      if (LIST_RE.test(l)) items.push(l.replace(LIST_RE, "").trim());
      else if (l.trim() && items.length) items[items.length - 1] += " " + l.trim();
    });
    var tag = ordered ? "ol" : "ul";
    return "<" + tag + ">" + items.map(function (t) { return "<li>" + inline(t, ctx) + "</li>"; }).join("") + "</" + tag + ">";
  }

  // ----- Go highlighting for signatures and snippets

  var GO_KW = new Set(("break case chan const continue default defer else fallthrough for func go goto if import " +
    "interface map package range return select struct switch type var").split(" "));
  var GO_TOKEN = /(\/\/[^\n]*|\/\*[\s\S]*?\*\/)|("(?:[^"\\\n]|\\.)*"|`[^`]*`|'(?:[^'\\\n]|\\.)*')|\b(\d[\w.]*)\b|\b([a-z]+)\b/g;
  function highlightGo(src) {
    if (!src) return "";
    var out = "", pos = 0, m;
    GO_TOKEN.lastIndex = 0;
    while ((m = GO_TOKEN.exec(src))) {
      var cls = m[1] ? 3 : m[2] ? 2 : m[3] ? 4 : (m[4] && GO_KW.has(m[4])) ? 1 : 0;
      if (!cls) continue;
      out += esc(src.slice(pos, m.index)) + '<span class="k' + cls + '">' + esc(m[0]) + "</span>";
      pos = m.index + m[0].length;
    }
    return out + esc(src.slice(pos));
  }

  // plain renders text with `code` spans as code and nothing linked (for
  // places that are links themselves).
  function plain(text) {
    return esc(text || "").replace(/`([^`]+)`/g, "<code>$1</code>");
  }

  return { render: render, inline: inline, plain: plain, findRefs: findRefs, linkify: linkify, highlightGo: highlightGo };
})();
