// Placeholder of the explorer app (work order X1): lists the packages with
// their roles and shows one file with highlighting and links. It exists to
// prove that the data loads from file:// and that the annotations work; X2
// replaces it.
(function () {
  "use strict";
  var core = EXPLORER.core;
  var nav = document.getElementById("nav");
  var main = document.getElementById("main");
  if (!core) { nav.textContent = "data/core.js did not load: run ./dev explore"; return; }

  var symbols = {};
  core.symbols.forEach(function (s) { symbols[s.ref] = s; });
  var files = {};
  core.files.forEach(function (f) { files[f.path] = f; });

  function esc(s) {
    return s.replace(/[&<>"]/g, function (c) { return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]; });
  }

  // Where a link target leads: a symbol's declaration or a package.
  function hrefOf(target) {
    if (target.indexOf("pkg:") === 0) return "#pkg=" + encodeURIComponent(target.slice(4));
    var s = symbols[target];
    return s ? "#file=" + encodeURIComponent(s.file) + "&line=" + s.line : "#";
  }

  function renderNav() {
    var b = core.build;
    var html = "<h1>devc explorer</h1><p class=muted>" + esc((b.commit || "").slice(0, 12)) + (b.dirty ? " (dirty)" : "") +
      " · " + esc(b.time) + " · " + core.packages.length + " packages, " + core.symbols.length + " symbols</p>";
    core.packages.forEach(function (p) {
      html += "<div class=pkg><a href='#pkg=" + encodeURIComponent(p.path) + "'><code>" + esc(p.path) + "</code></a>" +
        "<div>" + esc(p.role || "") + "</div></div>";
    });
    nav.innerHTML = html;
  }

  function loadChunk(path, id, done) {
    if (EXPLORER.chunks[id]) { done(EXPLORER.chunks[id]); return; }
    var k = "chunk:" + id;
    (EXPLORER.waiting[k] = EXPLORER.waiting[k] || []).push(done);
    var s = document.createElement("script");
    s.src = path;
    s.onerror = function () { main.textContent = "could not load " + path; };
    document.head.appendChild(s);
  }

  // Merges the token classes and links of a file into HTML, line by line.
  function renderSource(sf, line) {
    var src = sf.source, n = src.length;
    var cls = new Uint8Array(n), link = new Int32Array(n).fill(-1);
    var pos = 0, i, j, t = sf.tokens || [], l = sf.links || [];
    for (i = 0; i < t.length; i += 3) { pos += t[i]; for (j = pos; j < pos + t[i + 1]; j++) cls[j] = t[i + 2]; }
    pos = 0;
    for (i = 0; i < l.length; i += 3) { pos += l[i]; for (j = pos; j < pos + l[i + 1]; j++) link[j] = l[i + 2]; }
    var out = [], lineNo = 1, start = 0;
    function flush(end) {
      var seg = src.slice(start, end), c = cls[start], k = link[start];
      var h = esc(seg);
      if (c) h = "<span class=k" + c + ">" + h + "</span>";
      if (k >= 0) h = "<a class=ref href='" + hrefOf(sf.targets[k]) + "' title='" + esc(sf.targets[k]) + "'>" + h + "</a>";
      out.push(h);
      start = end;
    }
    out.push(lineStart(1, line));
    for (i = 0; i < n; i++) {
      if (src[i] === "\n") {
        if (i > start) flush(i);
        lineNo++;
        out.push("</span>\n" + lineStart(lineNo, line));
        start = i + 1;
      } else if (i > start && (cls[i] !== cls[start] || link[i] !== link[start])) {
        flush(i);
      }
    }
    if (n > start) flush(n);
    out.push("</span>");
    return out.join("");
  }

  function lineStart(no, hl) {
    return "<span id=L" + no + (no === hl ? " class=hl" : "") + "><a class=ln href='#file=" +
      encodeURIComponent(current) + "&line=" + no + "'>" + no + "</a>";
  }

  var current = "";
  function showFile(path, line) {
    var f = files[path];
    if (!f) { main.textContent = "no file " + path; return; }
    if (!f.chunk) { main.textContent = path + " is a binary file"; return; }
    current = path;
    main.innerHTML = "<h1>" + esc(path) + "</h1><p class=muted>" + esc(f.doc || "") + "</p>Loading…";
    var id = f.chunk.replace(/^data\/chunks\//, "").replace(/\.js$/, "");
    loadChunk(f.chunk, id, function (chunk) {
      var sf = chunk.files.filter(function (x) { return x.path === path; })[0];
      main.innerHTML = "<h1>" + esc(path) + "</h1><p class=muted>" + esc(f.doc || "") + "</p><pre id=src>" +
        renderSource(sf, line) + "</pre>";
      var el = line && document.getElementById("L" + line);
      if (el) el.scrollIntoView({ block: "center" });
    });
  }

  function showPackage(path) {
    var p = core.packages.filter(function (x) { return x.path === path; })[0];
    if (!p) { main.textContent = "no package " + path; return; }
    main.innerHTML = "<h1>" + esc(p.path) + "</h1><p>" + esc(p.role || "") + "</p><pre>" + esc(p.doc || "") + "</pre><ul>" +
      p.files.map(function (f) {
        return "<li><a href='#file=" + encodeURIComponent(f) + "'>" + esc(f) + "</a> <span class=muted>" +
          esc((files[f] && files[f].doc) || "") + "</span></li>";
      }).join("") + "</ul>";
  }

  function route() {
    var q = {};
    location.hash.slice(1).split("&").forEach(function (kv) {
      var i = kv.indexOf("=");
      if (i > 0) q[kv.slice(0, i)] = decodeURIComponent(kv.slice(i + 1));
    });
    if (q.file) showFile(q.file, parseInt(q.line || "0", 10));
    else if (q.pkg) showPackage(q.pkg);
    else main.innerHTML = "<p>Choose a package. Example: <a href='#file=internal%2Fdaemon%2Fapi.go&line=36'>internal/daemon/api.go</a></p>";
  }

  renderNav();
  window.addEventListener("hashchange", route);
  route();
})();
