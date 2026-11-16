// The Overview (#/overview): the model's overview.md, the reading order
// with a checkbox per concern, flow and tour (kept in localStorage), a
// small runtime map that leads to the Runtime view, a legend of the zone
// colours and the shapes, and the numbers of the build.
(function () {
  "use strict";
  var U = X.util, D = X.data, M = D.model, R = X.refs, esc = U.esc;
  var mini = null;

  function readSet() { return new Set(U.load("read", []) || []); }

  function readingOrder() {
    var order = X.model.readingOrder(), done = readSet();
    var seenConcerns = new Set();
    var h = '<ol class="ro">';
    order.forEach(function (ref) {
      var info = R.resolve(ref), o = info.obj;
      var nested = info.kind === "flow" && o.concern && seenConcerns.has(o.concern);
      if (info.kind === "concern") seenConcerns.add(o.id);
      // Concerns carry a sentence; their flows stay one line each.
      var sum = o.summary && !nested ? U.firstSentence(X.md.plainText(o.summary)) : "";
      var kind = info.kind === "flow" ? (o.kind === "tour" ? "tour" : "flow") : "concern";
      h += '<li class="ro-i' + (nested ? " nested" : "") + (done.has(ref) ? " done" : "") + '">' +
        '<label class="ro-check" title="Mark as read"><input type="checkbox" data-read="' + esc(ref) + '"' + (done.has(ref) ? " checked" : "") + "></label>" +
        '<div class="ro-body"><a class="ro-title" href="' + esc(R.href(ref)) + '" data-ref="' + esc(ref) + '" data-act="full">' + U.icon(kind === "tour" ? "list" : kind) +
        "<span>" + esc(o.title) + '</span></a><span class="ro-kind">' + kind + "</span>" +
        (sum ? '<div class="ro-sum">' + X.doc.plain(sum) + "</div>" : "") + "</div></li>";
    });
    return h + "</ol>";
  }

  function legend() {
    var zones = M.zones || [];
    var trusts = [];
    zones.forEach(function (z) { if (trusts.indexOf(z.trust) < 0) trusts.push(z.trust); });
    var h = '<div class="ov-leg"><h3 class="sp-label">Zone colours</h3><ul class="ov-zones">' + trusts.map(function (t) {
      var zs = zones.filter(function (z) { return z.trust === t; });
      return '<li class="z-' + esc(t) + '"><i class="ov-sw"></i><span class="ov-trust">' + esc(t) + "</span>" +
        zs.map(function (z) { return R.chip("zone:" + z.id, { cls: "flat small", noZone: true, plain: true }); }).join("") + "</li>";
    }).join("") + "</ul>";
    var kinds = new Set((M.actors || []).map(function (a) { return a.kind; }));
    h += '<h3 class="sp-label">Shapes</h3><ul class="ov-shapes">' +
      ["process", "service", "store", "person"].filter(function (k) { return kinds.has(k); }).map(function (k) {
        return "<li>" + U.icon(X.runtimeMap.KIND_ICON[k]) + "<span>" + k + "</span></li>";
      }).join("") +
      "<li>" + U.icon("editor") + "<span>devc code (started by a command or an entry point)</span></li>" +
      '<li><i class="lgl"></i><span>a channel, from who connects to who listens</span></li>' +
      '<li><i class="lgl bnd"><b></b></i><span>a channel across a trust boundary; the gate marks where what arrives is checked</span></li>' +
      '<li><i class="ov-strip"><b class="z-host"></b><b class="z-untrusted"></b></i><span>a strip on a name: the zones where that code may run</span></li>' +
      "</ul></div>";
    return h;
  }

  function stats() {
    var c = D.core.build.counts;
    function first(kind) { var s = (M.surfaces || []).filter(function (x) { return x.kind === kind; })[0]; return s ? s.id : ""; }
    var items = [
      [c.packages, "packages", X.router.href("packages")],
      [c.lines.code, "lines of code", X.router.href("packages", "size")],
      [c.testLines.code, "lines of tests", X.router.href("packages", "size")],
      [c.commands, "commands", X.router.href("interfaces", first("commands"))],
      [c.rpc, "RPC methods", X.router.href("interfaces", first("rpc"))],
      [c.routes, "routes", X.router.href("interfaces", first("http"))],
      [(M.concerns || []).length, "concerns", X.router.href("concerns")],
      [(M.flows || []).length, "flows and tours", X.router.href("flows")]
    ];
    return '<div class="stats ov-stats">' + items.map(function (it) {
      return '<a class="stat" href="' + esc(it[2]) + '"><div class="stat-n num">' + U.fmt(it[0]) + '</div><div class="stat-l">' + esc(it[1]) + "</div></a>";
    }).join("") + "</div>";
  }

  function render(el) {
    var name = U.lastElem(D.core.build.module || "") || "code";
    var order = X.model.readingOrder();
    var done = readSet();
    var nDone = order.filter(function (r) { return done.has(r); }).length;
    el.innerHTML = '<div class="ov">' +
      '<div class="ov-main"><h1 class="ov-h1"><span class="mono">' + esc(name) + '</span><span class="ov-sub">how it is built</span></h1>' +
      '<div class="md ov-text">' + X.md.render(M.overview || "", { headingTag: 2 }) + "</div>" +
      '<section class="ov-ro"><h2 class="sec-h">Reading order <span class="count ro-count">' + nDone + " of " + order.length + " read</span></h2>" +
      (order.length ? readingOrder() : '<p class="muted">The model has no concerns or flows yet.</p>') + "</section></div>" +
      '<aside class="ov-side">' +
      '<figure class="ov-map"><div class="ov-map-in"></div><figcaption><a href="#/runtime">' + U.icon("actor") + "Runtime: what runs where, and whom it trusts</a>" +
      '<span class="muted small">' + U.plural((M.zones || []).length, "zone") + " · " + U.plural((M.actors || []).length, "actor") + " · " + U.plural((M.channels || []).length, "channel") + "</span></figcaption></figure>" +
      legend() + '<h3 class="sp-label">This build</h3>' + stats() + "</aside></div>";
    if ((M.zones || []).length) {
      mini = X.runtimeMap.render(el.querySelector(".ov-map-in"), { mini: true, onClick: function (ev) { if (!ev.target.closest("a")) X.router.go("#/runtime"); } });
    }
    el.querySelector(".ov-ro").addEventListener("change", function (ev) {
      var ref = ev.target.getAttribute("data-read");
      if (!ref) return;
      var s = readSet();
      if (ev.target.checked) s.add(ref); else s.delete(ref);
      U.save("read", Array.from(s));
      ev.target.closest(".ro-i").classList.toggle("done", ev.target.checked);
      var n = order.filter(function (r) { return s.has(r); }).length;
      el.querySelector(".ro-count").textContent = n + " of " + order.length + " read";
    });
  }

  X.router.register("overview", {
    nav: "overview",
    title: function () { return "Overview"; },
    render: render,
    unmount: function () { if (mini) mini.destroy(); mini = null; }
  });
})();
