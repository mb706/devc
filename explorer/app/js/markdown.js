// Markdown for the model's text and the documents: headings with anchors,
// paragraphs, emphasis, inline code, fenced and indented code blocks,
// nested lists, tables, block quotes, links, rules, and the model's
// [[reference]] and [[reference|text]] forms. Runs of plain text go
// through X.doc.inline, so the build's patterns (findings, work orders,
// plan and spec sections), package and file paths and backticked names
// that resolve become references there. Everything else is escaped: raw
// HTML is shown as text, HTML comments are dropped, nothing executes.
//
//   X.md.render(text, ctx) -> HTML
//     ctx.pkg        package for Go [Name] links
//     ctx.doc        path of the document being shown (relative links,
//                    "#anchor" links); ctx.headings its Heading list
//                    (anchors by line, as the build computed them)
//     ctx.firstLine  line number of text's first line in its file
//     ctx.headingTag shift heading levels: 1 renders "#" as <h1> (default
//                    2: a page title is the view's own <h1>)
//   X.md.inline(text, ctx) -> HTML of one paragraph's inline Markdown
//   X.md.slug(text)        -> the anchor of a heading (GitHub style, as
//                    the tool's slug)
//   X.md.headings(text)    -> [{level, text, anchor, line}] of Markdown
//   X.md.plainText(md)     -> the text without markup (for summaries)
//   X.md.cardText(md)      -> HTML of the first paragraph for a hover card,
//                    cut at 420 characters
//
// Headings carry data-anchor (not id: several renderings share a page).
X.md = (function () {
  "use strict";
  var U = X.util, D = X.data, esc = U.esc;

  // ----- anchors

  function slug(s) {
    var out = "";
    var t = String(s).toLowerCase();
    for (var i = 0; i < t.length; i++) {
      var c = t[i];
      if (/[\p{L}\p{N}_-]/u.test(c)) out += c;
      else if (c === " ") out += "-";
    }
    return out;
  }
  function headings(text) {
    var out = [], used = {}, fence = null;
    String(text || "").split("\n").forEach(function (l, i) {
      var fm = /^\s{0,3}(`{3,}|~{3,})/.exec(l);
      if (fm) { if (!fence) fence = fm[1][0]; else if (fm[1][0] === fence) fence = null; return; }
      if (fence) return;
      var m = /^\s{0,3}(#{1,6})\s+(.*?)\s*#*\s*$/.exec(l);
      if (!m) return;
      var a = slug(m[2]);
      if (used[a]) { var k = used[a]; used[a] = k + 1; a = a + "-" + k; } else used[a] = 1;
      out.push({ level: m[1].length, text: m[2], anchor: a, line: i + 1 });
    });
    return out;
  }

  // ----- inline

  // Placeholders: <n>. Kind "raw" (code spans, [[refs]]) is
  // put back into the text that X.doc.inline renders; kind "html" (links,
  // escapes, breaks) splits it.
  var PH = /(\d+)/g;

  function protect(src, ctx) {
    var store = [];
    function ph(kind, value) { store.push({ kind: kind, v: value }); return "" + (store.length - 1) + ""; }
    var out = "", i = 0, n = src.length;
    while (i < n) {
      var c = src[i], m;
      var rest = src.slice(i);
      if (c === "\\" && i + 1 < n && /[\\`*_{}\[\]()#+\-.!|<>~]/.test(src[i + 1])) {
        out += ph("html", esc(src[i + 1])); i += 2; continue;
      }
      if (c === "`") {
        var run = /^`+/.exec(rest)[0];
        var close = src.indexOf(run, i + run.length);
        while (close >= 0 && src[close + run.length] === "`") close = src.indexOf(run, close + run.length + 1);
        if (close > 0) {
          var inner = src.slice(i + run.length, close);
          if (run.length > 1) out += ph("html", "<code>" + esc(inner.trim()) + "</code>");
          else out += ph("raw", src.slice(i, close + 1));
          i = close + run.length; continue;
        }
        out += ph("html", esc(run)); i += run.length; continue;
      }
      if (c === "[" && src[i + 1] === "[" && (m = /^\[\[([^\]|\n]+)(?:\|([^\]\n]+))?\]\]/.exec(rest))) {
        out += ph("raw", m[0]); i += m[0].length; continue;
      }
      if ((c === "!" && src[i + 1] === "[") || c === "[") {
        var lk = link(src, c === "!" ? i + 1 : i);
        if (lk) {
          if (c === "!") out += ph("html", '<span class="md-img" title="' + esc(lk.url) + '">[' + esc(lk.text || "image") + "]</span>");
          else out += ph("html", renderLink(lk, ctx));
          i = lk.end; continue;
        }
      }
      if (c === "<") {
        if ((m = /^<(https?:\/\/[^\s>]+)>/.exec(rest))) { out += ph("html", '<a href="' + esc(m[1]) + '" target="_blank" rel="noopener noreferrer">' + esc(m[1]) + "</a>"); i += m[0].length; continue; }
        if ((m = /^<br\s*\/?>/i.exec(rest))) { out += ph("html", "<br>"); i += m[0].length; continue; }
        if ((m = /^<!--[\s\S]*?-->/.exec(rest))) { i += m[0].length; continue; }
      }
      out += c; i++;
    }
    return { text: out, store: store };
  }

  // link parses [text](url "title") at i; text may hold code and nested
  // brackets.
  function link(src, i) {
    var depth = 0, j = i;
    for (; j < src.length; j++) {
      var c = src[j];
      if (c === "\\") { j++; continue; }
      if (c === "`") { var e = src.indexOf("`", j + 1); if (e > 0) { j = e; continue; } }
      if (c === "[") depth++;
      else if (c === "]") { depth--; if (depth === 0) break; }
      else if (c === "\n" && src[j + 1] === "\n") return null;
    }
    if (depth !== 0 || src[j + 1] !== "(") return null;
    var m = /^\(\s*<?([^\s()<>]*(?:\([^\s()]*\)[^\s()<>]*)*)>?(?:\s+"([^"]*)")?\s*\)/.exec(src.slice(j + 1));
    if (!m) return null;
    return { text: src.slice(i + 1, j), url: m[1], title: m[2] || "", end: j + 1 + m[0].length };
  }

  // renderLink: external links open elsewhere; a link to a document or a
  // file of the site becomes a reference to it.
  function renderLink(lk, ctx) {
    var label = inline(lk.text, ctx, true);
    var url = lk.url;
    if (/^(https?:|mailto:)/i.test(url)) return '<a href="' + esc(url) + '" target="_blank" rel="noopener noreferrer"' + (lk.title ? ' title="' + esc(lk.title) + '"' : "") + ">" + label + "</a>";
    var ref = localRef(url, ctx);
    if (ref) {
      var h = X.refs.href(ref);
      return '<a class="chip inline md-link" data-ref="' + esc(ref) + '"' + (h ? ' href="' + esc(h) + '"' : "") + ">" + label + "</a>";
    }
    return '<span class="md-deadlink" title="' + esc(url) + '">' + label + "</span>";
  }
  function localRef(url, ctx) {
    var parts = url.split("#"), p = parts[0], anchor = parts[1] || "";
    var base = ctx && ctx.doc ? U.dirOf(ctx.doc) : "";
    if (!p) return ctx && ctx.doc && anchor ? "doc:" + ctx.doc + "#" + anchor : null;
    var path = normalize(p.charAt(0) === "/" ? p.slice(1) : (base ? base + "/" + p : p));
    if (D.document(path)) return "doc:" + path + (anchor ? "#" + anchor : "");
    if (D.file(path)) return "file:" + path;
    if (D.pkg(path.replace(/\/$/, ""))) return "pkg:" + path.replace(/\/$/, "");
    return null;
  }
  function normalize(path) {
    var out = [];
    path.split("/").forEach(function (s) {
      if (s === "..") out.pop();
      else if (s && s !== ".") out.push(s);
    });
    return out.join("/");
  }

  // Emphasis on the protected text: strong, then em, then strike.
  var EMPH = [
    [/\*\*([^\s*](?:[\s\S]*?[^\s])?)\*\*/g, "strong"],
    [/(^|[^\w])__([^\s_](?:[\s\S]*?[^\s])?)__(?!\w)/g, "strong", true],
    [/(^|[^*\w])\*([^\s*](?:[\s\S]*?[^\s*])?)\*(?!\*)/g, "em", true],
    [/(^|[^\w])_([^\s_](?:[\s\S]*?[^\s_])?)_(?!\w)/g, "em", true],
    [/~~([^\s~](?:[\s\S]*?[^\s])?)~~/g, "del"]
  ];
  function emphasis(t) {
    EMPH.forEach(function (e) {
      t = t.replace(e[0], function () {
        var a = arguments;
        return e[2] ? a[1] + "" + e[1] + "" + a[2] + "/" + e[1] + "" : "" + e[1] + "" + a[1] + "/" + e[1] + "";
      });
    });
    return t;
  }

  function inline(src, ctx, inLink) {
    if (!src) return "";
    var pr = protect(String(src), ctx);
    var t = emphasis(pr.text);
    var out = "";
    t.split(/(\/?(?:strong|em|del))/).forEach(function (part) {
      var m = /^(\/?)(strong|em|del)$/.exec(part);
      if (m) { out += "<" + m[1] + m[2] + ">"; return; }
      out += runs(part, pr.store, ctx, inLink);
    });
    return out;
  }
  // runs renders text with placeholders: raw ones go back into the text for
  // X.doc.inline, html ones are emitted as they are.
  function runs(part, store, ctx, inLink) {
    var out = "", buf = "", last = 0, m;
    PH.lastIndex = 0;
    function flush() {
      if (!buf) return;
      out += inLink ? esc(buf).replace(/`([^`]+)`/g, "<code>$1</code>") : X.doc.inline(buf, ctx);
      buf = "";
    }
    while ((m = PH.exec(part))) {
      buf += part.slice(last, m.index);
      var s = store[+m[1]];
      if (s.kind === "raw") buf += s.v;
      else { flush(); out += s.v; }
      last = m.index + m[0].length;
    }
    buf += part.slice(last);
    flush();
    return out;
  }

  // ----- blocks

  var RE = {
    fence: /^(\s{0,3})(`{3,}|~{3,})\s*([\w+-]*)/,
    heading: /^\s{0,3}(#{1,6})\s+(.*?)\s*#*\s*$/,
    hr: /^\s{0,3}([-*_])(?:\s*\1){2,}\s*$/,
    quote: /^\s{0,3}>\s?/,
    item: /^(\s*)([-*+]|\d{1,9}[.)])(\s+|$)/,
    tableSep: /^\s*\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)*\|?\s*$/,
    blank: /^\s*$/,
    comment: /^\s{0,3}<!--/
  };

  function render(text, ctx) {
    ctx = ctx || {};
    var lines = String(text || "").replace(/\r\n?/g, "\n").replace(/\t/g, "    ").split("\n");
    var st = { ctx: ctx, used: {}, byLine: null, shift: (ctx.headingTag || 2) - 1 };
    if (ctx.headings) {
      st.byLine = new Map();
      ctx.headings.forEach(function (h) { st.byLine.set(h.line, h); });
    }
    return blocks(lines, 0, lines.length, st, (ctx.firstLine || 1));
  }

  // blocks renders lines[a..b) ; line0 is the file line of lines[0].
  function blocks(lines, a, b, st, line0) {
    var out = [], i = a;
    while (i < b) {
      var l = lines[i], m;
      if (RE.blank.test(l)) { i++; continue; }
      if (RE.comment.test(l)) {
        var j = i;
        while (j < b && lines[j].indexOf("-->") < 0) j++;
        i = j + 1; continue;
      }
      if ((m = RE.fence.exec(l))) {
        var ch = m[2][0], len = m[2].length, indent = m[1].length, body = [];
        i++;
        while (i < b && !new RegExp("^\\s{0,3}" + (ch === "`" ? "`" : "~") + "{" + len + ",}\\s*$").test(lines[i])) {
          body.push(lines[i].slice(Math.min(indent, /^\s*/.exec(lines[i])[0].length)));
          i++;
        }
        i++;
        out.push(codeBlock(body.join("\n"), m[3]));
        continue;
      }
      if ((m = RE.heading.exec(l))) {
        out.push(heading(m[1].length, m[2], i + line0, st));
        i++; continue;
      }
      if (RE.hr.test(l)) { out.push("<hr>"); i++; continue; }
      if (RE.quote.test(l)) {
        var q = [];
        while (i < b && !RE.blank.test(lines[i]) && (RE.quote.test(lines[i]) || q.length)) {
          if (!RE.quote.test(lines[i]) && startsBlock(lines[i])) break;
          q.push(lines[i].replace(RE.quote, "")); i++;
        }
        out.push("<blockquote>" + blocks(q, 0, q.length, st, i - q.length + line0) + "</blockquote>");
        continue;
      }
      if (RE.item.test(l) && /^\s{0,3}\S/.test(l)) {
        var r = list(lines, i, b, st, line0);
        out.push(r.html); i = r.end; continue;
      }
      if (/^ {4}/.test(l)) {
        var code = [];
        while (i < b && (/^ {4}/.test(lines[i]) || (RE.blank.test(lines[i]) && i + 1 < b && /^ {4}/.test(lines[i + 1])))) { code.push(lines[i].slice(4)); i++; }
        out.push(codeBlock(code.join("\n"), ""));
        continue;
      }
      if (l.indexOf("|") >= 0 && i + 1 < b && RE.tableSep.test(lines[i + 1]) && lines[i + 1].indexOf("-") >= 0) {
        var t = table(lines, i, b, st);
        out.push(t.html); i = t.end; continue;
      }
      // A paragraph: until a blank line or the start of another block.
      var para = [l.trim()];
      i++;
      while (i < b && !RE.blank.test(lines[i]) && !startsBlock(lines[i])) { para.push(lines[i]); i++; }
      out.push("<p>" + paragraph(para, st) + "</p>");
    }
    return out.join("");
  }
  function startsBlock(l) {
    return RE.fence.test(l) || RE.heading.test(l) || RE.quote.test(l) || RE.hr.test(l) || (RE.item.test(l) && /^\s{0,3}\S/.test(l) && !/^\s*\d{1,9}[.)]\s*$/.test(l));
  }
  function paragraph(lines, st) {
    // Hard breaks: two trailing spaces or a backslash.
    var text = lines.map(function (x, k) {
      var t = x.replace(/^\s+/, "");
      if (k < lines.length - 1 && (/ {2,}$/.test(x) || /\\$/.test(x))) return t.replace(/\\$/, "").replace(/\s+$/, "") + "<br>";
      return t.replace(/\s+$/, "");
    }).join("\n");
    return inline(text, st.ctx);
  }

  function heading(level, text, line, st) {
    var h = st.byLine && st.byLine.get(line), a;
    if (h) a = h.anchor;
    else {
      a = slug(text);
      if (st.used[a]) { var k = st.used[a]; st.used[a] = k + 1; a = a + "-" + k; } else st.used[a] = 1;
    }
    var tag = "h" + Math.min(6, level + st.shift);
    return "<" + tag + ' class="md-h" data-anchor="' + esc(a) + '" data-line="' + line + '">' + inline(text, st.ctx) +
      '<a class="md-anchor" href="#" data-anchor-link="' + esc(a) + '" title="Link to this heading" aria-hidden="true">#</a></' + tag + ">";
  }

  function codeBlock(body, lang) {
    lang = (lang || "").toLowerCase();
    var map = { sh: "shell", bash: "shell", zsh: "shell", console: "shell", js: "javascript", json: "json", toml: "toml", yaml: "yaml", yml: "yaml", css: "css", html: "html", javascript: "javascript", shell: "shell", dockerfile: "dockerfile" };
    var html;
    if (lang === "go") html = X.doc.highlightGo(body);
    else if (map[lang] && X.source && X.source.tokenize) html = tokens(body, X.source.tokenize(body, map[lang]));
    else html = esc(body);
    return '<pre class="doc-code md-code"' + (lang ? ' data-lang="' + esc(lang) + '"' : "") + ">" + html + "</pre>";
  }
  function tokens(src, t) {
    var out = "", pos = 0;
    for (var i = 0; i < t.length; i += 3) {
      if (t[i] < pos) continue;
      out += esc(src.slice(pos, t[i])) + '<span class="k' + t[i + 2] + '">' + esc(src.substr(t[i], t[i + 1])) + "</span>";
      pos = t[i] + t[i + 1];
    }
    return out + esc(src.slice(pos));
  }

  // list renders a list starting at line i: items of the same marker type
  // at the same indent; an item's content is everything indented at least
  // to its text, rendered as blocks (nested lists, code, paragraphs).
  function list(lines, i, b, st, line0) {
    var first = RE.item.exec(lines[i]);
    var ordered = /\d/.test(first[2]), base = first[1].length;
    var start = ordered ? parseInt(first[2], 10) : 1;
    var items = [], loose = false, cur = null, blankBefore = false;
    while (i < b) {
      var l = lines[i], m = RE.item.exec(l);
      if (RE.blank.test(l)) {
        if (cur) cur.lines.push("");
        blankBefore = true; i++; continue;
      }
      var ind = /^\s*/.exec(l)[0].length;
      if (m && m[1].length < base + 2 && m[1].length >= base - 1 && (/\d/.test(m[2])) === ordered) {
        if (cur && blankBefore) loose = true;
        var content = m[1].length + m[2].length + Math.max(1, Math.min(4, m[3].length || 1));
        cur = { lines: [l.slice(Math.min(l.length, content))], indent: content, line: i + line0 };
        items.push(cur);
        blankBefore = false; i++; continue;
      }
      if (!cur) break;
      if (ind >= cur.indent) {
        if (blankBefore && hasText(cur.lines)) loose = true;
        cur.lines.push(l.slice(cur.indent)); blankBefore = false; i++; continue;
      }
      if (!blankBefore && !startsBlock(l) && ind < cur.indent) { cur.lines.push(l.trim()); i++; continue; } // lazy continuation
      break;
    }
    while (items.length && cur && cur.lines.length && cur.lines[cur.lines.length - 1] === "") cur.lines.pop();
    var tag = ordered ? "ol" : "ul";
    var html = "<" + tag + (ordered && start !== 1 ? ' start="' + start + '"' : "") + (loose ? "" : ' class="tight"') + ">" + items.map(function (it) {
      while (it.lines.length && it.lines[it.lines.length - 1] === "") it.lines.pop();
      var inner = blocks(it.lines, 0, it.lines.length, st, it.line);
      if (!loose) inner = inner.replace(/^<p>([\s\S]*?)<\/p>/, "$1");
      var task = /^\[([ xX])\]\s/.exec(it.lines[0] || "");
      if (task) inner = '<input type="checkbox" disabled' + (task[1] !== " " ? " checked" : "") + "> " + inner.replace(/^\[[ xX]\]\s/, "");
      return "<li>" + inner + "</li>";
    }).join("") + "</" + tag + ">";
    return { html: html, end: i };
  }
  function hasText(ls) { return ls.some(function (x) { return x.trim(); }); }

  function cells(row) {
    var r = row.trim();
    if (r[0] === "|") r = r.slice(1);
    if (r[r.length - 1] === "|" && r[r.length - 2] !== "\\") r = r.slice(0, -1);
    var out = [], cur = "", inCode = false;
    for (var i = 0; i < r.length; i++) {
      var c = r[i];
      if (c === "\\" && r[i + 1] === "|") { cur += "|"; i++; continue; }
      if (c === "`") inCode = !inCode;
      if (c === "|" && !inCode) { out.push(cur.trim()); cur = ""; continue; }
      cur += c;
    }
    out.push(cur.trim());
    return out;
  }
  function table(lines, i, b, st) {
    var head = cells(lines[i]);
    var align = cells(lines[i + 1]).map(function (c) {
      return /^:-+:$/.test(c) ? "center" : /-+:$/.test(c) ? "right" : "";
    });
    var rows = [];
    i += 2;
    while (i < b && !RE.blank.test(lines[i]) && lines[i].indexOf("|") >= 0) { rows.push(cells(lines[i])); i++; }
    function td(tag, c, k) { return "<" + tag + (align[k] ? ' style="text-align:' + align[k] + '"' : "") + ">" + inline(c, st.ctx) + "</" + tag + ">"; }
    var html = '<div class="md-tw"><div class="md-table"><table><thead><tr>' + head.map(function (c, k) { return td("th", c, k); }).join("") + "</tr></thead><tbody>" +
      rows.map(function (r) { return "<tr>" + head.map(function (_, k) { return td("td", r[k] || "", k); }).join("") + "</tr>"; }).join("") +
      "</tbody></table></div></div>";
    return { html: html, end: i };
  }

  // ----- tables wider than their column scroll sideways, with a cue: a
  // line above saying so and a fade on each side that has more. Every
  // table that appears anywhere is watched, and checked again on resize.
  var tableRO = window.ResizeObserver ? new ResizeObserver(function (entries) {
    entries.forEach(function (en) { fitTable(en.target); });
  }) : null;
  function fitTable(sc) {
    var tw = sc.parentNode;
    if (!tw || !tw.classList) return;
    // First let it take the room its page allows beyond the text's measure
    // (where the page's styles allow that), once, so it does not flicker.
    if (!tw.classList.contains("grown") && sc.scrollWidth > sc.clientWidth + 1) tw.classList.add("grown");
    var wide = sc.scrollWidth > sc.clientWidth + 1;
    tw.classList.toggle("wide", wide);
    tw.classList.toggle("at-start", !wide || sc.scrollLeft <= 1);
    tw.classList.toggle("at-end", !wide || sc.scrollLeft + sc.clientWidth >= sc.scrollWidth - 1);
  }
  function watchTables(root) {
    (root.querySelectorAll ? root.querySelectorAll(".md-table") : []).forEach(function (sc) {
      if (sc._watched) return;
      sc._watched = true;
      sc.addEventListener("scroll", function () { fitTable(sc); }, { passive: true });
      if (tableRO) tableRO.observe(sc); else fitTable(sc);
    });
  }
  if (window.MutationObserver) {
    new MutationObserver(function (muts) {
      muts.forEach(function (m) {
        m.addedNodes.forEach(function (n) { if (n.nodeType === 1 && (n.classList.contains("md-tw") || n.querySelector(".md-table"))) watchTables(n); });
      });
    }).observe(document.documentElement, { childList: true, subtree: true });
  }

  // plainText strips markup: for summaries in places that are links.
  function plainText(md) {
    return String(md || "")
      .replace(/\[\[([^\]|\n]+)\|([^\]\n]+)\]\]/g, "$2")
      .replace(/\[\[([^\]|\n]+)\]\]/g, function (_, r) { try { return X.refs.resolve(r.trim()).label; } catch (e) { return r; } })
      .replace(/!?\[([^\]]*)\]\([^)]*\)/g, "$1")
      .replace(/(\*\*|__|~~)(.*?)\1/g, "$2")
      .replace(/(^|\W)[*_](\S.*?)[*_](?=\W|$)/g, "$1$2")
      .replace(/^#+\s+/gm, "");
  }

  // cardText: the first paragraph of a text for a hover card, with its
  // links; past 420 characters, cut at a word as plain text.
  function cardText(text) {
    var p = U.firstParagraph(text || "");
    if (p.length <= 420) return inline(p, {});
    var t = plainText(p);
    return X.doc.plain(t.length > 420 ? t.slice(0, 400).replace(/\s+\S*$/, "") + " …" : t);
  }

  return { render: render, inline: function (t, ctx) { return inline(t, ctx || {}); }, slug: slug, headings: headings, plainText: plainText, cardText: cardText };
})();
