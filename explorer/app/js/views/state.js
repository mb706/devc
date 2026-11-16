// The State view (#/state?sel=<tree>:<path> | mount:<n>): the trees of
// layout.toml as outlines that fold, each path with who writes it (in the
// zone colours), what it is and the code that knows the path; and the
// mounts of a box as two sides, the host's paths and the box's, joined by
// an arrow that says whether the box can write there.
(function () {
  "use strict";
  var U = X.util, D = X.data, M = D.model, R = X.refs, esc = U.esc;
  var WRITER = { host: "host writes", box: "the box writes", both: "host and box write", user: "you write" };

  function writerMark(w) {
    if (!w) return '<span class="wr none"></span>';
    var ts = X.model.writerTrusts(w);
    return '<span class="wr" title="' + esc(WRITER[w] || w) + '">' + ts.map(function (t) { return '<i class="z-' + esc(t) + '"></i>'; }).join("") + "<span>" + esc(w) + "</span></span>";
  }
  // pathHTML styles the parts that vary (<name>).
  function pathHTML(p) {
    return esc(p).replace(/&lt;([^&]*?)&gt;/g, '<span class="var">&lt;$1&gt;</span>');
  }

  // tree builds the outline of a tree's entries; parents an entry needs
  // but does not list are added without text.
  function build(t) {
    var root = { kids: new Map(), path: "" };
    (t.entries || []).forEach(function (e) {
      var parts = e.path.split("/").filter(Boolean), node = root, acc = "";
      parts.forEach(function (p, i) {
        acc = acc ? acc + "/" + p : p;
        if (!node.kids.has(p)) node.kids.set(p, { name: p, path: acc, kids: new Map(), entry: null });
        node = node.kids.get(p);
      });
      node.entry = e;
    });
    return root;
  }
  function outline(t, node, depth, sel) {
    var h = "";
    node.kids.forEach(function (n) {
      var e = n.entry, key = t.id + ":" + n.path, has = n.kids.size > 0;
      // Collapse a chain of parents without text into one row.
      var name = n.name, cur = n;
      while (!cur.entry && cur.kids.size === 1) { var only = cur.kids.values().next().value; name += "/" + only.name; cur = only; key = t.id + ":" + cur.path; }
      e = cur.entry; has = cur.kids.size > 0;
      h += '<div class="st-row' + (sel === key ? " sel" : "") + (has ? " has" : "") + '" data-key="' + esc(key) + '" style="--d:' + depth + '">' +
        '<span class="st-tw">' + (has ? U.icon("fwd", "caret") : "") + "</span>" +
        '<span class="st-name mono">' + pathHTML(name) + (has ? "/" : "") + "</span>" + writerMark(e && e.writer) +
        '<span class="st-text">' + (e ? X.md.inline(e.text || "", {}) : "") + "</span>" +
        '<span class="st-ref">' + (e && e.ref ? R.chip(e.ref, { cls: "small" }) : "") + "</span></div>";
      if (has) h += '<div class="st-kids">' + outline(t, cur, depth + 1, sel) + "</div>";
    });
    return h;
  }

  function modeClass(m) { return /^rw/.test(m || "") ? "rw" : /^ro/.test(m || "") ? "ro" : "other"; }

  function render(el, r) {
    var sel = r.params.sel || "";
    var h = '<div class="page st"><h1>State</h1><p class="lead">What is on disk and what is mounted into a box: each path with who writes it, what it holds and the code that knows it.</p>' +
      '<div class="st-legend">' + ["user", "host", "box", "both"].map(function (w) { return '<span class="lg">' + writerMark(w) + esc(WRITER[w]) + "</span>"; }).join("") + "</div>";
    (M.trees || []).forEach(function (t) {
      h += '<section class="st-tree" data-tree="' + esc(t.id) + '"><header class="st-th"><h2>' + esc(t.title) + '</h2><code class="st-root">' + esc(t.root) + "</code>" +
        '<span class="muted small">' + U.plural((t.entries || []).length, "path") + "</span></header>" +
        (t.summary ? '<p class="st-sum">' + X.md.inline(t.summary, {}) + "</p>" : "") +
        '<div class="st-outline">' + outline(t, build(t), 0, sel) + "</div></section>";
    });
    var mounts = M.mounts || [];
    if (mounts.length) {
      h += '<section class="st-mounts"><header class="st-th"><h2>Mounts of a box</h2><span class="muted small">' + U.plural(mounts.length, "mount") + "</span></header>" +
        '<div class="mt-head"><span class="z-host"><i class="zd"></i>on the host</span><span></span><span class="z-untrusted"><i class="zd"></i>in the box</span></div>' +
        mounts.map(function (m, i) {
          var mc = modeClass(m.mode);
          return '<div class="mt' + (sel === "mount:" + i ? " sel" : "") + '" data-key="mount:' + i + '">' +
            '<code class="mt-src">' + pathHTML(m.source) + "</code>" +
            '<span class="mt-arrow ' + mc + '" title="' + esc(mc === "rw" ? "the box can write here" : mc === "ro" ? "read-only in the box" : m.mode) + '"><span class="mt-mode">' + esc(m.mode) + "</span></span>" +
            '<code class="mt-dst">' + pathHTML(m.target) + "</code>" +
            '<div class="mt-text">' + X.md.inline(m.text || "", {}) + (m.ref ? " " + R.chip(m.ref, { cls: "small" }) : "") + "</div></div>";
        }).join("") +
        '<p class="small muted mt-note"><span class="mt-arrow rw sample"></span> the box can write there &nbsp; <span class="mt-arrow ro sample"></span> read-only in the box</p></section>';
    }
    if (!(M.trees || []).length && !mounts.length) h += '<p class="muted">The model describes no state yet.</p>';
    el.innerHTML = h + "</div>";
    el.addEventListener("click", function (ev) {
      var row = ev.target.closest(".st-row.has");
      if (row && !ev.target.closest("a, [data-ref]")) row.classList.toggle("closed");
    });
    var s = sel ? el.querySelector('[data-key="' + (window.CSS && CSS.escape ? CSS.escape(sel) : sel) + '"]') : null;
    if (s) s.scrollIntoView({ block: "center" });
  }

  X.router.register("state", { nav: "state", title: function () { return "State"; }, render: render });
})();
