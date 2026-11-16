// The source view: a file with line numbers, highlighting (the build's
// token classes for Go, a small highlighter for other files), a link on
// every identifier the build resolved, and the text patterns as links in
// comments. Used full-width in the Code view and as an excerpt in the
// inspector. The whole file is rendered (a 2,000-line file takes a few
// tens of milliseconds), so the browser's find-in-page sees all of it;
// line numbers are generated content and are neither found nor copied.
//
//   var v = X.source.view(scroller, {
//     path, from, to,          lines to show (default: the whole file)
//     range: [a, b],           lines to tint (a declaration)
//     line: n | [a, b],        selected line(s)
//     scroll: n | "range",     where to scroll once rendered
//     wrap: bool,              wrap long lines (the inspector does)
//     onIdent(ref, ev, el),    click on an identifier (default: inspect)
//     onLine(n, ev),           click on a line number
//     onReady(v), expand(v)    rendered; "show the whole file" clicked
//   })
//   v.setLine(n | [a, b] | 0), v.setRange([a, b] | null),
//   v.scrollTo(n, "center" | "start" | "nearest"), v.firstVisibleLine(),
//   v.row(n), v.source (SourceFile), v.ready (Promise), v.destroy()
//
//   X.source.addMarks(fn)      fn(path) -> [{line, ref, cls, title}]:
//                              marks in the margin (X3: flow steps, key
//                              symbols, invariants)
//   X.source.header(path, opts) -> element: breadcrumbs, package, file
//                              comment, copy and editor links
//   X.source.declStart(sf, sym) -> first line of the symbol's doc comment
X.source = (function () {
  "use strict";
  var U = X.util, D = X.data, esc = U.esc;
  var markProviders = [];
  var cache = new Map();

  // ----- highlighting of files that are not Go

  var JS_KW = "break case catch class const continue debugger default delete do else export extends finally for function if import in instanceof let new return super switch this throw try typeof var void while with yield async await of null true false undefined";
  var LANG = {
    javascript: { line: ["//"], block: [["/*", "*/"]], str: ["\"", "'", "`"], kw: JS_KW },
    css: { block: [["/*", "*/"]], str: ["\"", "'"] },
    html: { block: [["<!--", "-->"]], str: ["\""] },
    template: { block: [["{{/*", "*/}}"], ["<!--", "-->"]], str: ["\""], tmpl: true },
    shell: { line: ["#"], str: ["\"", "'"], kw: "if then else elif fi for while do done case esac function in return exit local export set" },
    toml: { line: ["#"], str: ["\"", "'"], kw: "true false" },
    yaml: { line: ["#"], str: ["\"", "'"], kw: "true false null" },
    dockerfile: { line: ["#"], str: ["\"", "'"], kw: "FROM RUN COPY ADD ENV ARG WORKDIR USER ENTRYPOINT CMD LABEL EXPOSE VOLUME SHELL HEALTHCHECK" },
    json: { str: ["\""], kw: "true false null" },
    markdown: { md: true },
    text: {}
  };
  Object.keys(LANG).forEach(function (k) { if (LANG[k].kw) LANG[k].kwSet = new Set(LANG[k].kw.split(" ")); });

  // tokenize returns absolute (start, length, class) triples.
  function tokenize(src, lang) {
    var c = LANG[lang] || LANG.text, out = [], i = 0, n = src.length;
    if (c.md) return tokenizeMarkdown(src);
    function at(s) { return src.substr(i, s.length) === s; }
    while (i < n) {
      var ch = src[i], j, matched = false, k;
      for (k = 0; c.block && k < c.block.length; k++) {
        if (at(c.block[k][0])) {
          j = src.indexOf(c.block[k][1], i + c.block[k][0].length);
          j = j < 0 ? n : j + c.block[k][1].length;
          out.push(i, j - i, 3); i = j; matched = true; break;
        }
      }
      if (matched) continue;
      for (k = 0; c.line && k < c.line.length; k++) {
        if (at(c.line[k]) && (c.line[k] !== "#" || i === 0 || /\s/.test(src[i - 1]))) {
          j = src.indexOf("\n", i); if (j < 0) j = n;
          out.push(i, j - i, 3); i = j; matched = true; break;
        }
      }
      if (matched) continue;
      if (c.str && c.str.indexOf(ch) >= 0) {
        j = i + 1;
        while (j < n && src[j] !== ch && (ch === "`" || src[j] !== "\n")) { if (src[j] === "\\") j++; j++; }
        j = Math.min(n, j + 1);
        out.push(i, j - i, 2); i = j; continue;
      }
      if (c.tmpl && at("{{")) {
        j = src.indexOf("}}", i); j = j < 0 ? n : j + 2;
        out.push(i, j - i, 1); i = j; continue;
      }
      if (/[A-Za-z_]/.test(ch)) {
        j = i + 1;
        while (j < n && /[\w-]/.test(src[j])) j++;
        if (c.kwSet && c.kwSet.has(src.slice(i, j)) && (i === 0 || !/[\w.$-]/.test(src[i - 1]))) out.push(i, j - i, 1);
        i = j; continue;
      }
      if (/[0-9]/.test(ch) && (i === 0 || !/[\w.]/.test(src[i - 1]))) {
        j = i + 1;
        while (j < n && /[\w.]/.test(src[j])) j++;
        out.push(i, j - i, 4); i = j; continue;
      }
      i++;
    }
    return out;
  }
  function tokenizeMarkdown(src) {
    var out = [], re = /^(#{1,6} .*)$|^(```[\s\S]*?^```)$|(`[^`\n]+`)|^(\s*<!--[\s\S]*?-->)/gm, m;
    while ((m = re.exec(src))) {
      if (!m[0]) { re.lastIndex++; continue; }
      out.push(m.index, m[0].length, m[1] ? 5 : m[4] ? 3 : 2);
    }
    return out;
  }

  // ----- building the lines of a file

  // lines returns the HTML of every line of a file, computed once.
  function lines(sf) {
    var hit = cache.get(sf.path);
    if (hit) return hit;
    var src = sf.source, n = src.length;
    var cls = new Uint8Array(n), link = new Int32Array(n).fill(-1);
    var i, j, pos = 0, t, file = D.file(sf.path) || {};
    if (sf.tokens) {
      t = sf.tokens;
      for (i = 0; i < t.length; i += 3) { pos += t[i]; for (j = pos; j < pos + t[i + 1] && j < n; j++) cls[j] = t[i + 2]; }
    } else if (file.lang !== "go") {
      t = tokenize(src, file.lang);
      for (i = 0; i < t.length; i += 3) for (j = t[i]; j < t[i] + t[i + 1] && j < n; j++) cls[j] = t[i + 2];
    }
    if (sf.links) {
      pos = 0; t = sf.links;
      for (i = 0; i < t.length; i += 3) { pos += t[i]; for (j = pos; j < pos + t[i + 1] && j < n; j++) link[j] = t[i + 2]; }
    }
    var targets = sf.targets || [];
    var out = [], cur = "", lineNo = 1, start = 0;
    function seg(end) {
      if (end <= start) return;
      var text = src.slice(start, end), c = cls[start], k = link[start], h;
      if (c === 3 && !sf.links && file.lang !== "go") h = esc(text);
      else if (c === 3) h = commentHTML(text);
      else h = esc(text);
      if (c) h = '<span class="k' + c + '">' + h + "</span>";
      if (k >= 0) {
        var ref = targets[k], s = D.sym(ref);
        var def = s && s.file === sf.path && s.line === lineNo && s.name === text;
        h = '<a class="sl' + (def ? " def" : "") + '" data-ref="' + esc(ref) + '">' + h + "</a>";
      }
      cur += h;
    }
    for (i = 0; i < n; i++) {
      if (src.charCodeAt(i) === 10) {
        seg(i);
        out.push(cur); cur = ""; lineNo++;
        start = i + 1;
      } else if (i > start && (cls[i] !== cls[start] || link[i] !== link[start])) {
        seg(i);
        start = i;
      }
    }
    seg(n);
    if (cur || !out.length || src.charCodeAt(n - 1) !== 10) out.push(cur);
    cache.set(sf.path, out);
    return out;
  }

  // commentHTML links the text patterns inside a comment.
  function commentHTML(text) {
    var spans = X.doc.findRefs(text);
    if (!spans.length) return esc(text);
    var out = "", p = 0;
    spans.forEach(function (sp) {
      out += esc(text.slice(p, sp.s)) + '<a class="pl" data-ref="' + esc(sp.ref) + '">' + esc(text.slice(sp.s, sp.e)) + "</a>";
      p = sp.e;
    });
    return out + esc(text.slice(p));
  }

  function marksOf(path) {
    var m = new Map();
    markProviders.forEach(function (fn) {
      var list;
      try { list = fn(path) || []; } catch (e) { list = []; }
      list.forEach(function (x) {
        if (!m.has(x.line)) m.set(x.line, []);
        m.get(x.line).push(x);
      });
    });
    return m;
  }

  // declStart is the first line of the doc comment above a declaration.
  function declStart(sf, sym) {
    var ls = sf.source.split("\n", sym.line), a = sym.line;
    while (a > 1 && /^\s*\/\//.test(ls[a - 2] || "")) a--;
    return a;
  }

  // ----- the view

  function view(scroller, o) {
    o = o || {};
    var v = { source: null, path: o.path };
    var host = U.h("div.src" + (o.wrap ? ".wrap" : ""));
    scroller.classList.add("src-scroll");
    scroller.innerHTML = "";
    scroller.appendChild(U.h("div.loading", U.h("span.spinner"), "Loading " + o.path + "…"));
    var from = 1, to = 0, total = 0, sel = null, range = null, destroyed = false;

    v.ready = D.source(o.path).then(function (sf) {
      if (destroyed) return v;
      v.source = sf;
      var ls = lines(sf);
      total = ls.length;
      from = Math.max(1, o.from || 1);
      to = Math.min(total, o.to || total);
      var t0 = performance.now();
      render(ls);
      v.renderMs = performance.now() - t0;
      scroller.innerHTML = "";
      if (from > 1) scroller.appendChild(moreBar("above"));
      scroller.appendChild(host);
      if (to < total) scroller.appendChild(moreBar("below"));
      if (o.range) v.setRange(o.range);
      if (o.line) v.setLine(o.line);
      if (o.scroll === "range" && o.range) v.scrollTo(o.range[0], "start");
      else if (o.scroll) v.scrollTo(o.scroll, "center");
      if (o.onReady) o.onReady(v);
      return v;
    }, function (err) {
      scroller.innerHTML = "";
      scroller.appendChild(U.h("div.error", err.message));
      throw err;
    });
    v.ready.catch(function () {});

    function render(ls) {
      var marks = marksOf(o.path);
      var gw = String(total).length;
      host.style.setProperty("--gw", (gw + 1) + "ch");
      var hasMarks = marks.size > 0;
      host.classList.toggle("has-marks", hasMarks);
      var out = new Array(to - from + 1);
      for (var n = from; n <= to; n++) {
        var mk = "";
        if (hasMarks) {
          var ms = marks.get(n);
          mk = '<i class="mk">' + (ms ? ms.map(function (m) {
            return '<a class="mkx ' + esc(m.cls || "") + '"' + (m.ref ? ' data-ref="' + esc(m.ref) + '" data-nocard' : "") +
              (m.href ? ' href="' + esc(m.href) + '" data-act="full"' : "") + ' title="' + esc(m.title || "") + '"></a>';
          }).join("") : "") + "</i>";
        }
        out[n - from] = '<div class="r" data-n="' + n + '"><i class="n" data-n="' + n + '"></i>' + mk + '<span class="c">' + (ls[n - 1] || "") + "</span></div>";
      }
      host.innerHTML = out.join("");
    }

    function moreBar(where) {
      var b = U.h("button.src-more", { type: "button" }, where === "above" ? "Show lines 1–" + (from - 1) : "Show lines " + (to + 1) + "–" + total);
      b.onclick = function () {
        if (o.expand) o.expand(v);
        else v.expandAll();
      };
      return b;
    }
    v.expandAll = function () {
      if (!v.source) return;
      var keep = range ? range[0] : from;
      from = 1; to = total;
      render(lines(v.source));
      scroller.innerHTML = "";
      scroller.appendChild(host);
      if (range) v.setRange(range);
      if (sel) v.setLine(sel);
      v.scrollTo(keep, "start");
    };

    v.row = function (n) {
      if (n < from || n > to) return null;
      return host.children[n - from] || null;
    };
    function eachRow(a, b, fn) { for (var n = Math.max(a, from); n <= Math.min(b, to); n++) { var r = v.row(n); if (r) fn(r); } }
    v.setRange = function (r) {
      if (range) eachRow(range[0], range[1], function (el) { el.classList.remove("in"); });
      range = r;
      if (range) eachRow(range[0], range[1], function (el) { el.classList.add("in"); });
    };
    v.setLine = function (l) {
      if (sel) eachRow(sel[0], sel[1], function (el) { el.classList.remove("sel"); });
      sel = !l ? null : Array.isArray(l) ? [Math.min(l[0], l[1]), Math.max(l[0], l[1])] : [l, l];
      if (sel) eachRow(sel[0], sel[1], function (el) { el.classList.add("sel"); });
    };
    v.scrollTo = function (n, block) {
      var r = v.row(Math.max(from, Math.min(to, n)));
      if (!r) return;
      var top = r.offsetTop;
      var h = scroller.clientHeight;
      if (block === "center") scroller.scrollTop = Math.max(0, top - h / 2 + r.offsetHeight * 2);
      else if (block === "nearest") {
        if (top < scroller.scrollTop || top > scroller.scrollTop + h - r.offsetHeight) scroller.scrollTop = Math.max(0, top - h / 3);
      } else scroller.scrollTop = Math.max(0, top - Math.min(80, h / 6));
    };
    // firstVisibleLine finds the first line at the top of the scroller.
    v.firstVisibleLine = function () {
      var rows = host.children, lo = 0, hi = rows.length - 1, top = scroller.scrollTop + 4;
      if (!rows.length) return 0;
      while (lo < hi) {
        var mid = (lo + hi + 1) >> 1;
        if (rows[mid].offsetTop <= top) lo = mid; else hi = mid - 1;
      }
      return from + lo;
    };
    v.destroy = function () { destroyed = true; };

    host.addEventListener("click", function (ev) {
      var a = ev.target.closest("a.sl");
      if (a && o.onIdent && !U.isModifiedClick(ev) && ev.button === 0) {
        ev.preventDefault();
        X.hovercard.hide();
        o.onIdent(a.getAttribute("data-ref"), ev, a);
        return;
      }
      var num = ev.target.closest("i.n");
      if (num && o.onLine) { ev.preventDefault(); o.onLine(+num.getAttribute("data-n"), ev); }
    });
    return v;
  }

  // ----- the file header

  function header(path, opts) {
    opts = opts || {};
    var f = D.file(path) || { path: path, lines: { code: 0, comment: 0, blank: 0 } };
    var el = U.h("div.src-head");
    var parts = path.split("/"), crumbs = [], acc = "";
    parts.forEach(function (p, i) {
      acc = acc ? acc + "/" + p : p;
      var last = i === parts.length - 1;
      var isPkg = !last && D.pkg(acc);
      if (last) crumbs.push('<span class="crumb last">' + esc(p) + "</span>");
      else if (isPkg) crumbs.push('<a class="crumb" data-ref="pkg:' + esc(acc) + '" href="' + esc(X.router.href("pkg", acc)) + '">' + esc(p) + "</a>");
      else crumbs.push('<a class="crumb" href="' + esc(X.router.href("code", acc)) + '" data-nocard>' + esc(p) + "</a>");
    });
    var nLines = f.lines.code + f.lines.comment + f.lines.blank;
    var meta = [U.fmt(nLines) + " lines"];
    if (f.test) meta.push("test");
    if (f.ignored) meta.push("excluded by build constraints");
    var row = '<div class="src-path mono">' + crumbs.join('<span class="sep">/</span>') + "</div>" +
      '<div class="src-tools">' +
      (f.package ? X.refs.chip("pkg:" + f.package, { cls: "small" }) : '<span class="tag">' + esc(f.lang || "") + "</span>") +
      '<span class="muted small">' + esc(meta.join(" · ")) + "</span>" +
      '<button class="btn ghost icon" data-a="copy" title="Copy path:line">' + U.icon("copy") + "</button>" +
      '<a class="btn ghost icon" data-a="editor" title="Open in editor (set the editor in the settings)">' + U.icon("editor") + "</a>" +
      (opts.tools || "") + "</div>";
    el.innerHTML = '<div class="src-row">' + row + "</div>" +
      (f.doc ? '<div class="src-doc">' + X.doc.render(f.doc, { pkg: f.package }) + "</div>" : "");
    var line = function () { return (opts.line && opts.line()) || 1; };
    el.querySelector('[data-a="copy"]').onclick = function () { U.copy(path + ":" + line()); };
    var ed = el.querySelector('[data-a="editor"]');
    function updEditor() {
      var u = X.settings.editorURL(path, line());
      if (u) { ed.setAttribute("href", u); ed.classList.remove("off"); } else { ed.removeAttribute("href"); ed.classList.add("off"); }
    }
    ed.addEventListener("mouseenter", updEditor);
    ed.addEventListener("focus", updEditor);
    ed.addEventListener("click", function (ev) { updEditor(); if (!ed.getAttribute("href")) { ev.preventDefault(); X.shell && X.shell.openSettings(); } });
    updEditor();
    var doc = el.querySelector(".src-doc");
    if (doc) {
      doc.addEventListener("click", function (ev) { if (!ev.target.closest("a")) doc.classList.toggle("open"); });
    }
    return el;
  }

  return {
    view: view, lines: lines, header: header, declStart: declStart, tokenize: tokenize,
    addMarks: function (fn) { markProviders.push(fn); }
  };
})();
