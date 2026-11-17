// The Runtime view: zones as tinted regions on the model's grid, actors as
// cards inside them, channels as connectors. Channels that cross a trust
// boundary are heavier and carry a mark where they enter the receiving
// zone, where what arrives is checked. Labels appear on hover and
// selection; hovering an actor lights its channels and dims the rest.
// Inside a zone, actors are placed next to the zones and actors they talk
// to (a few rounds of barycentric placement on the zone's slots), which
// keeps connectors short and off other cards. Below the map: every
// boundary channel in a table. Filters keep one concern or one flow lit.
//
// Route: #/runtime?concern=<id>&flow=<id>&i=<actor:|channel:|zone:…>
//
//   X.runtimeMap.render(host, {mini}) -> {el, setFilter(set|null),
//     select(ref), redraw(), destroy()}   (the overview shows it small)
// Also the inspector's views of actors, zones and channels.
(function () {
  "use strict";
  var U = X.util, D = X.data, M = D.model, R = X.refs, esc = U.esc;
  var KIND_ICON = { process: "k-process", store: "k-store", person: "k-person", service: "k-service" };

  function isCode(a) { return !!((a.commands && a.commands.length) || (a.entry && a.entry.length)); }
  function trust(zoneId) { var z = D.zone(zoneId); return z ? z.trust : "external"; }

  // ----- the map

  function renderMap(host, opts) {
    opts = opts || {};
    var mini = !!opts.mini;
    var zones = M.zones || [], actors = M.actors || [], channels = M.channels || [];
    var cols = 1, rows = 1;
    zones.forEach(function (z) {
      cols = Math.max(cols, (z.col || 1) + (z.colspan || 1) - 1);
      rows = Math.max(rows, (z.row || 1) + (z.rowspan || 1) - 1);
    });
    var count = {};
    actors.forEach(function (a) { count[a.zone] = (count[a.zone] || 0) + 1; });
    // Columns are as wide as the busiest zone in them needs.
    var weights = [];
    for (var c = 1; c <= cols; c++) {
      var w = 1;
      zones.forEach(function (z) {
        var c0 = z.col || 1, c1 = c0 + (z.colspan || 1) - 1;
        if (c >= c0 && c <= c1) w = Math.max(w, (count[z.id] || 0) / ((z.colspan || 1) * Math.sqrt(z.rowspan || 1)));
      });
      weights.push(Math.max(1.4, Math.min(5, w)));
    }
    var map = U.h("div.rt-map" + (mini ? ".mini" : ""), { style: "grid-template-columns:" + weights.map(function (w) { return w.toFixed(2) + "fr"; }).join(" ") + ";grid-template-rows:repeat(" + rows + ",auto)" });
    var zoneEls = new Map(), bodyEls = new Map(), cardEls = new Map();
    zones.forEach(function (z) {
      var el = U.h("section.rt-zone.z-" + z.trust, { style: "grid-column:" + (z.col || 1) + " / span " + (z.colspan || 1) + ";grid-row:" + (z.row || 1) + " / span " + (z.rowspan || 1), "data-zone": z.id });
      el.innerHTML = '<header class="rt-zh" data-ref="zone:' + esc(z.id) + '"><span class="rt-zl"><i class="zd z-' + esc(z.trust) + '"></i><span class="rt-zt">' + esc(z.title) + "</span>" +
        (mini ? "" : '<span class="rt-ztrust">' + esc(z.trust) + "</span>") + "</span></header>";
      var body = U.h("div.rt-actors");
      el.appendChild(body);
      map.appendChild(el);
      zoneEls.set(z.id, el);
      bodyEls.set(z.id, body);
    });
    actors.forEach(function (a) {
      var body = bodyEls.get(a.zone);
      if (!body) return;
      var el = U.h("div.rt-actor.z-" + trust(a.zone) + (isCode(a) ? ".code" : ""), { "data-actor": a.id, "data-ref": "actor:" + a.id });
      if (mini) el.setAttribute("data-nocard", "");
      el.innerHTML = U.icon(KIND_ICON[a.kind] || "actor", "rt-kind") + '<span class="rt-title">' + esc(a.title) + "</span>" +
        (isCode(a) && !mini ? '<span class="rt-code" title="devc code: started by commands or entry points">' + U.icon("editor") + "</span>" : "");
      body.appendChild(el);
      cardEls.set(a.id, el);
    });
    host.appendChild(map);

    var nbrs = new Map();
    actors.forEach(function (a) { nbrs.set(a.id, []); });
    channels.forEach(function (ch) {
      if (nbrs.has(ch.from) && nbrs.has(ch.to) && ch.from !== ch.to) { nbrs.get(ch.from).push(ch.to); nbrs.get(ch.to).push(ch.from); }
    });

    // place puts each zone's actors on a grid of slots, near what they talk to.
    function place() {
      var hr = map.getBoundingClientRect();
      // Narrow maps (the inspector open on a laptop) get smaller cards.
      var narrow = !mini && hr.width < 1000;
      map.classList.toggle("narrow", narrow);
      var minW = mini ? 78 : narrow ? 96 : 124, gap = mini ? 6 : narrow ? 20 : 30, rowH = mini ? 30 : narrow ? 54 : 64;
      var slots = new Map(), pos = new Map();
      zones.forEach(function (z) {
        var body = bodyEls.get(z.id), n = count[z.id] || 0;
        if (!n) return;
        var r = body.getBoundingClientRect();
        var k = Math.max(1, Math.min(n, Math.floor((r.width + gap) / (minW + gap))));
        var nr = Math.ceil(n / k);
        // A tall zone (it spans rows) may use fewer columns and more rows.
        body.style.gridTemplateColumns = "repeat(" + k + ", minmax(0, 1fr))";
        var list = [], cw = r.width / k;
        for (var i = 0; i < nr * k; i++) list.push({ r: Math.floor(i / k), c: i % k, x: r.left - hr.left + cw * (i % k + 0.5), y: r.top - hr.top + rowH * (Math.floor(i / k) + 0.5) });
        slots.set(z.id, list);
        var mine = actors.filter(function (a) { return a.zone === z.id; });
        mine.forEach(function (a, i) { pos.set(a.id, list[i]); });
      });
      // Local search: swap two actors of a zone, or move one to a free
      // slot, while the total length of channels (Manhattan) shrinks.
      function cost(id, p) {
        var c = 0;
        nbrs.get(id).forEach(function (b) { var q = pos.get(b); if (q) c += Math.abs(p.x - q.x) + Math.abs(p.y - q.y); });
        return c;
      }
      for (var pass = 0; pass < 12; pass++) {
        var better = false;
        zones.forEach(function (z) {
          var list = slots.get(z.id);
          if (!list) return;
          var mine = actors.filter(function (a) { return a.zone === z.id; }).map(function (a) { return a.id; });
          for (var i = 0; i < mine.length; i++) {
            for (var j = 0; j < list.length; j++) {
              var a = mine[i], pa = pos.get(a), sj = list[j];
              if (pa === sj) continue;
              var b = null;
              mine.forEach(function (x) { if (pos.get(x) === sj) b = x; });
              var before = cost(a, pa) + (b ? cost(b, sj) : 0);
              pos.set(a, sj); if (b) pos.set(b, pa);
              var after = cost(a, sj) + (b ? cost(b, pa) : 0);
              if (after < before - 0.5) better = true;
              else { pos.set(a, pa); if (b) pos.set(b, sj); }
            }
          }
        });
        if (!better) break;
      }
      actors.forEach(function (a) {
        var s = pos.get(a.id), el = cardEls.get(a.id);
        if (s && el) { el.style.gridRow = String(s.r + 1); el.style.gridColumn = String(s.c + 1); }
      });
    }

    var layer = X.connectors.create(map, {
      labels: mini ? "svg" : "html",
      router: "grid",
      // Cards, and the titles of zones, which lines should not run through.
      obstacles: function () { return Array.from(cardEls.values()).concat(Array.from(map.querySelectorAll(".rt-zh .rt-zl"))); },
      // Labels of the lit channels: hovered or selected, else those a
      // filter keeps.
      labelWhen: function (e) { return / (hl|on)\b/.test(" " + layerClass(e)); },
      labelAway: function () { var f = state.hover || state.sel; return f && f.kind === "actor" ? cardEls.get(f.id) : null; },
      onDraw: gates
    });
    // The small map (the overview) shows zones and actors only: at that
    // size channels are noise; the Runtime view has them.
    var edgeList = mini ? [] : channels.map(function (ch) {
      return {
        from: cardEls.get(ch.from), to: cardEls.get(ch.to), arrow: !mini, curve: true, avoid: !mini,
        label: mini ? "" : ch.title, ref: mini ? "" : "channel:" + ch.id, hit: !mini,
        cls: "rt-ch" + (ch.boundary ? " bnd" : ""), data: ch
      };
    }).filter(function (e) { return e.from && e.to; });

    // gates marks each boundary channel where it enters the receiving
    // zone (or halfway, when both ends share a zone).
    function gates(live) {
      if (mini) return;
      var hr = map.getBoundingClientRect();
      live.forEach(function (e) {
        if (!e.data.boundary || !e._path || e.from === e.to) return;
        var tz = zoneEls.get((D.actor(e.data.to) || {}).zone), fz = zoneEls.get((D.actor(e.data.from) || {}).zone);
        var L = e._path.getTotalLength(), at = 0.5;
        if (tz && tz !== fz) {
          var r = tz.getBoundingClientRect();
          var box = { x: r.left - hr.left + map.scrollLeft, y: r.top - hr.top + map.scrollTop, w: r.width, h: r.height };
          var lo = 0, hi = 1, inside = function (t) { var p = e._path.getPointAtLength(L * t); return p.x >= box.x && p.x <= box.x + box.w && p.y >= box.y && p.y <= box.y + box.h; };
          var first = -1;
          for (var i = 0; i <= 40; i++) if (inside(i / 40)) { first = i / 40; break; }
          if (first > 0) {
            lo = first - 1 / 40; hi = first;
            for (var k = 0; k < 10; k++) { var mid = (lo + hi) / 2; if (inside(mid)) hi = mid; else lo = mid; }
            at = hi;
          }
        }
        var p = e._path.getPointAtLength(L * at), q = e._path.getPointAtLength(Math.min(L, L * at + 2));
        var ang = Math.atan2(q.y - p.y, q.x - p.x) * 180 / Math.PI;
        var g = document.createElementNS("http://www.w3.org/2000/svg", "path");
        g.setAttribute("class", "cx-gate");
        g.setAttribute("d", "M-5.5,0L0,-4.5L5.5,0L0,4.5Z");
        g.setAttribute("transform", "translate(" + p.x.toFixed(1) + "," + p.y.toFixed(1) + ") rotate(" + ang.toFixed(1) + ")");
        e._g.appendChild(g);
      });
    }

    // ----- focus: hover, selection, filter

    var state = { hover: null, sel: null, filter: null };
    function lit() {
      var f = state.hover || state.sel;
      var actorsOn = null, chOn = null;
      if (f) {
        actorsOn = new Set(); chOn = new Set();
        if (f.kind === "actor") {
          actorsOn.add(f.id);
          channels.forEach(function (ch) { if (ch.from === f.id || ch.to === f.id) { chOn.add(ch.id); actorsOn.add(ch.from); actorsOn.add(ch.to); } });
        } else if (f.kind === "channel") {
          var ch = D.channel(f.id);
          if (ch) { chOn.add(ch.id); actorsOn.add(ch.from); actorsOn.add(ch.to); }
        } else if (f.kind === "zone") {
          actors.forEach(function (a) { if (a.zone === f.id) actorsOn.add(a.id); });
          channels.forEach(function (ch) { if (actorsOn.has(ch.from) !== actorsOn.has(ch.to)) chOn.add(ch.id); });
          channels.forEach(function (ch) { if (chOn.has(ch.id)) { actorsOn.add(ch.from); actorsOn.add(ch.to); } });
        }
      }
      return { f: f, actors: actorsOn, ch: chOn };
    }
    var classFn = function () { return ""; };
    function layerClass(e) { return classFn(e) || ""; }
    function apply() {
      var l = lit(), flt = state.filter;
      map.classList.toggle("focus", !!l.f);
      map.classList.toggle("filtered", !!flt);
      cardEls.forEach(function (el, id) {
        el.classList.toggle("lit", !!l.actors && l.actors.has(id));
        el.classList.toggle("me", !!l.f && l.f.kind === "actor" && l.f.id === id);
        el.classList.toggle("out", !!flt && !flt.actors.has(id));
      });
      zoneEls.forEach(function (el, id) { el.classList.toggle("me", !!l.f && l.f.kind === "zone" && l.f.id === id); });
      classFn = function (e) {
        var id = e.data.id, c = [];
        if (l.ch) c.push(l.ch.has(id) ? (l.f.kind === "channel" ? "hl sel" : "hl") : "dim");
        if (flt && !flt.channels.has(id)) c.push("out");
        else if (flt && !l.ch) c.push("on");
        // Channels the reader did not ask for are not drawn at all in
        // the quieter modes: only those of the hovered or selected thing,
        // those a filter keeps, and (in "bnd") those across a boundary.
        var asked = (l.ch && l.ch.has(id)) || (flt && flt.channels.has(id));
        var mode = X.settings.get("rtChannels") || "focus";
        if (!asked && (mode === "focus" || (mode === "bnd" && !e.data.boundary))) c.push("quiet");
        return c.join(" ");
      };
      layer.setClass(classFn);
    }

    function parseRef(ref) {
      var m = /^(actor|channel|zone):(.+)$/.exec(ref || "");
      return m ? { kind: m[1], id: m[2] } : null;
    }
    map.addEventListener("mouseover", function (ev) {
      var t = ev.target.closest(".rt-actor, .cx-hit, .cx-lab, .rt-zh");
      var f = null;
      if (t && t.classList.contains("rt-actor")) f = { kind: "actor", id: t.getAttribute("data-actor") };
      else if (t && t.classList.contains("rt-zh")) f = { kind: "zone", id: t.parentNode.getAttribute("data-zone") };
      else if (t) { var e = layer.edgeOf(t); if (e) f = { kind: "channel", id: e.data.id }; }
      var key = f ? f.kind + f.id : "", cur = state.hover ? state.hover.kind + state.hover.id : "";
      if (key === cur) return;
      state.hover = f;
      apply();
    });
    map.addEventListener("mouseleave", function () { if (state.hover) { state.hover = null; apply(); } });
    if (mini && opts.onClick) map.addEventListener("click", opts.onClick);

    function relayout() { place(); layer.set(edgeList); apply(); }
    relayout();
    var ro = window.ResizeObserver ? new ResizeObserver(U.debounce(function () { if (map.isConnected) { place(); layer.redraw(); } }, 60)) : null;
    if (ro) ro.observe(map);

    return {
      el: map, layer: layer, cards: cardEls,
      select: function (ref) { state.sel = parseRef(ref); apply(); },
      setFilter: function (f) { state.filter = f; apply(); },
      refresh: function () { apply(); },
      redraw: function () { place(); layer.redraw(); },
      destroy: function () { if (ro) ro.disconnect(); layer.destroy(); }
    };
  }

  X.runtimeMap = { render: renderMap, isCode: isCode, KIND_ICON: KIND_ICON };

  // ----- the view

  var S = null;

  function filterOf(r) {
    if (r.params.flow && D.flow(r.params.flow)) {
      return { actors: new Set(X.model.actorsOf("flow", r.params.flow)), channels: new Set(X.model.channelsOf("flow", r.params.flow).map(function (c) { return c.id; })), ref: "flow:" + r.params.flow };
    }
    if (r.params.concern && D.concern(r.params.concern)) {
      return { actors: new Set(X.model.actorsOf("concern", r.params.concern)), channels: new Set(X.model.channelsOf("concern", r.params.concern).map(function (c) { return c.id; })), ref: "concern:" + r.params.concern };
    }
    return null;
  }

  function legend() {
    var kinds = new Set((M.actors || []).map(function (a) { return a.kind; }));
    var h = ["process", "service", "store", "person"].filter(function (k) { return kinds.has(k); }).map(function (k) {
      return '<span class="lg">' + U.icon(KIND_ICON[k]) + k + "</span>";
    }).join("");
    h += '<span class="lg">' + U.icon("editor") + "devc code</span>";
    h += '<span class="lg"><i class="lgl"></i>channel</span>';
    h += '<span class="lg"><i class="lgl bnd"><b></b></i>crosses a trust boundary, checked where it enters</span>';
    return h;
  }

  function options(list, cur, none) {
    return '<option value="">' + esc(none) + "</option>" + list.map(function (x) {
      return '<option value="' + esc(x.id) + '"' + (x.id === cur ? " selected" : "") + ">" + esc(x.title) + "</option>";
    }).join("");
  }

  function render(el, r) {
    var flows = (M.flows || []).filter(function (f) { return f.kind !== "tour"; });
    el.innerHTML = '<div class="rt">' +
      '<div class="rt-bar">' +
      '<label class="rt-sel">Concern <select data-f="concern">' + options(M.concerns || [], r.params.concern, "all") + "</select></label>" +
      '<label class="rt-sel">Flow <select data-f="flow">' + options(flows, r.params.flow, "all") + "</select></label>" +
      '<label class="rt-sel">Channels <select data-f="ch">' +
      [["focus", "of what you point at"], ["bnd", "across a trust boundary"], ["all", "all"]].map(function (o) {
        return '<option value="' + o[0] + '"' + ((X.settings.get("rtChannels") || "focus") === o[0] ? " selected" : "") + ">" + o[1] + "</option>";
      }).join("") + "</select></label>" +
      '<span class="rt-filter-note muted small"></span>' +
      '<div class="pk-legend rt-legend">' + legend() + "</div></div>" +
      '<div class="rt-body"><div class="rt-wrap"></div>' +
      '<div class="rt-hint muted small">Hover an actor to see its channels, or pick a concern or a flow; the Channels menu shows more at rest. Click an actor or a channel to inspect it. Zones: hover the title for what runs there and whom it trusts.</div>' +
      '<section class="rt-bounds"><h2 class="sec-h">Boundaries <span class="count"></span></h2>' +
      '<p class="lead">Every channel that crosses a trust boundary: what crosses it and where what arrives is checked.</p><div class="rt-btable"></div></section>' +
      "</div></div>";
    S = { el: el, r: r };
    S.map = renderMap(el.querySelector(".rt-wrap"), {});
    el.querySelector(".rt-bar").addEventListener("change", function (ev) {
      var f = ev.target.getAttribute("data-f");
      if (!f) return;
      if (f === "ch") { X.settings.set("rtChannels", ev.target.value); S.map.refresh(); return; }
      var p = {}; p[f] = ev.target.value || null;
      if (f === "flow" && ev.target.value) p.concern = null;
      if (f === "concern" && ev.target.value) p.flow = null;
      X.router.setParams(p, true);
    });
    boundaries(el.querySelector(".rt-btable"), el.querySelector(".rt-bounds .count"));
    sync(r);
  }

  function sync(r) {
    var f = filterOf(r);
    S.map.setFilter(f);
    S.map.select(r.params.i || "");
    var note = S.el.querySelector(".rt-filter-note");
    note.innerHTML = f ? "lit: " + R.chip(f.ref, { cls: "small" }) + ' <a href="' + esc(X.router.hrefKeep("runtime", "", {})) + '">show all</a>' : "";
    S.el.querySelector('[data-f="concern"]').value = r.params.concern || "";
    S.el.querySelector('[data-f="flow"]').value = r.params.flow || "";
  }

  function boundaries(box, countEl) {
    var rows = (M.channels || []).filter(function (c) { return c.boundary; });
    countEl.textContent = rows.length;
    X.table.create(box, {
      columns: [
        { id: "ch", title: "Channel", width: "17%", html: function (c) { return R.chip("channel:" + c.id); }, sort: function (c) { return c.title; } },
        { id: "ends", title: "From → to", width: "21%", html: function (c) {
          return '<div class="rt-ends">' + R.chip("actor:" + c.from, { cls: "small" }) + '<span class="muted">→</span>' + R.chip("actor:" + c.to, { cls: "small" }) + "</div>";
        }, sort: function (c) { return c.from + " " + c.to; } },
        { id: "carries", title: "What crosses", html: function (c) {
          return '<div class="rt-carries">' + X.md.inline(c.carries || "", {}) + (c.transport ? '<div class="muted small">' + X.md.inline(c.transport, {}) + "</div>" : "") + "</div>";
        } },
        { id: "checks", title: "Checked at", width: "26%", html: function (c) {
          return (c.checks || []).length ? '<div class="chips">' + c.checks.map(function (x) { return R.chip(x, { cls: "small" }); }).join("") + "</div>" : '<span class="tag warn">no check named</span>';
        }, sort: function (c) { return -(c.checks || []).length; } }
      ],
      rows: rows,
      key: function (c) { return c.id; },
      text: function (c) { return [c.title, c.from, c.to, c.carries, c.transport].concat(c.checks || []).join(" "); },
      placeholder: "Filter boundaries…",
      onSelect: function (c) { X.inspector.open("channel:" + c.id); }
    });
  }

  X.router.register("runtime", {
    nav: "runtime",
    title: function () { return "Runtime"; },
    render: render,
    update: function (r) {
      if (!S) return false;
      S.r = r;
      sync(r);
      return true;
    },
    unmount: function () { if (S && S.map) S.map.destroy(); S = null; }
  });
  // The inspector only changes "i": follow it on the map.
  X.router.onChange(function (r) { if (S && r.view === "runtime") S.map.select(r.params.i || ""); });

  // ----- references, cards and the inspector

  function runtimeHref(ref) { return X.router.href("runtime", "", { i: ref }); }
  function channelLine(ch, from) {
    var other = from ? ch.to : ch.from;
    return '<li class="rt-cl' + (ch.boundary ? " bnd" : "") + '"><span class="rt-dir">' + (from ? "→" : "←") + "</span>" +
      R.chip("actor:" + other, { cls: "flat" }) + R.chip("channel:" + ch.id, { cls: "small" }) +
      (ch.boundary ? '<i class="gate" title="crosses a trust boundary"></i>' : "") + "</li>";
  }
  function flowSteps(f, test) {
    return X.model.steps(f).filter(function (st) { return test(st.step); }).map(function (st) {
      return '<a class="stepno" href="' + esc(X.router.href("flows", f.id, { step: st.no })) + '">' + st.no + "</a>";
    }).join("");
  }

  R.define("actor", {
    href: function (o, k) { return runtimeHref("actor:" + k); },
    card: function (info) {
      var o = info.obj, z = D.zone(o.zone);
      var chs = X.model.channelsOf("actor", o.id);
      return '<div class="hc-row">' + R.zoneDot(z) + " " + esc(z ? z.title : o.zone) + ' · <span class="muted">' + esc(o.kind) + "</span>" +
        (isCode(o) ? ' <span class="tag">devc code</span>' : "") + "</div>" +
        (o.summary ? '<div class="hc-doc">' + X.md.cardText(o.summary) + "</div>" : "") +
        '<div class="hc-row muted">' + U.plural(chs.length, "channel") + " · " + U.plural(X.model.flowsOf("actor", o.id).length, "flow") + "</div>";
    }
  });
  R.define("zone", {
    href: function (o, k) { return runtimeHref("zone:" + k); },
    card: function (info) {
      var o = info.obj, n = (M.actors || []).filter(function (a) { return a.zone === o.id; }).length;
      return '<div class="hc-row">' + R.zoneDot(o) + ' <span class="muted">trust: ' + esc(o.trust) + " · " + U.plural(n, "actor") + "</span></div>" +
        (o.summary ? '<div class="hc-doc">' + X.md.cardText(o.summary) + "</div>" : "");
    }
  });
  R.define("channel", {
    href: function (o, k) { return runtimeHref("channel:" + k); },
    card: function (info) {
      var o = info.obj;
      return '<div class="hc-row">' + R.chip("actor:" + o.from, { cls: "small" }) + '<span class="muted">→</span>' + R.chip("actor:" + o.to, { cls: "small" }) +
        (o.boundary ? ' <span class="tag warn">trust boundary</span>' : "") + "</div>" +
        (o.carries ? '<div class="hc-doc">' + X.md.cardText(o.carries) + "</div>" : "") +
        (o.transport ? '<div class="hc-row muted">' + X.md.inline(o.transport, {}) + "</div>" : "") +
        ((o.checks || []).length ? '<div class="hc-row"><span class="muted">checked at</span> ' + o.checks.slice(0, 3).map(function (c) { return R.chip(c, { cls: "small" }); }).join("") + "</div>" : "");
    }
  });

  function sec(title, n, body) {
    return '<section class="sp-sec"><h3 class="sp-h">' + esc(title) + (n != null ? ' <span class="count">' + n + "</span>" : "") + "</h3>" + body + "</section>";
  }
  function firstDoc(ref) {
    var s = D.sym(ref);
    return s && s.doc ? '<div class="sp-first">' + X.doc.inline(U.firstSentence(s.doc), { pkg: s.package }) + "</div>" : "";
  }

  X.inspector.register("actor", function (info, body) {
    var a = info.obj, z = D.zone(a.zone);
    var h = '<div class="ins-head z-' + esc(z ? z.trust : "external") + '">' + U.icon(KIND_ICON[a.kind] || "actor") +
      '<span>' + esc(a.kind) + " in </span>" + R.chip("zone:" + a.zone, { cls: "small" }) + (isCode(a) ? '<span class="tag">devc code</span>' : '<span class="tag">not devc code</span>') + "</div>";
    if (a.summary) h += '<div class="doc prose ins-lead">' + X.md.render(a.summary, {}) + "</div>";
    var starts = (a.commands || []).map(function (c) { return R.chip("cmd:" + c); }).concat((a.entry || []).map(function (e) { return R.chip(e); }));
    if (starts.length) h += sec("Started by", starts.length, '<div class="chips">' + starts.join("") + "</div>");
    var cmds = D.core.commands.filter(function (c) { return c.actor === a.id && (a.commands || []).indexOf(c.path) < 0; });
    if (cmds.length) {
      var shown = cmds.slice(0, 14);
      h += sec("Its other commands", cmds.length, '<div class="chips">' + shown.map(function (c) { return R.chip("cmd:" + c.path, { cls: "small" }); }).join("") +
        (cmds.length > shown.length ? ' <a class="small" href="' + esc(X.router.href("interfaces", (M.surfaces || []).filter(function (s) { return s.kind === "commands"; }).map(function (s) { return s.id; })[0] || "")) + '">and ' + (cmds.length - shown.length) + " more in the commands table</a>" : "") + "</div>");
    }
    var pk = D.core.packages.filter(function (p) { return p.actors && p.actors[a.id] > 0; })
      .sort(function (x, y) { return y.actors[a.id] - x.actors[a.id] || (x.path < y.path ? -1 : 1); });
    if (pk.length) {
      var reach = (D.core.reach.actors || []).filter(function (x) { return x.actor === a.id; })[0];
      h += sec("Code that may run in it", pk.length, '<div class="runs' + (pk.length > 12 ? " capped" : "") + '">' + pk.map(function (p) {
        var sh = p.actors[a.id];
        return '<div class="run-row z-' + esc(z ? z.trust : "external") + '">' + R.chip("pkg:" + p.path, { cls: "flat", noZone: true }) +
          '<span class="run-bar"><b style="width:' + Math.max(1.5, sh * 100).toFixed(1) + '%"></b></span><span class="num small muted">' + U.pct(sh) + "</span></div>";
      }).join("") + "</div>" + (pk.length > 12 ? '<button class="btn ghost small" data-a="allpk">Show all ' + pk.length + "</button>" : "") + '<p class="small muted">Share of each package\'s function lines reachable from its entry points' + (reach ? " (" + U.plural(reach.functions, "function") + " in all)" : "") +
        ". An over-approximation: " + esc(D.core.reach.note || "") + "</p>");
    }
    var out = (M.channels || []).filter(function (c) { return c.from === a.id; }), inn = (M.channels || []).filter(function (c) { return c.to === a.id; });
    if (out.length || inn.length) h += sec("Channels", out.length + inn.length, '<ul class="rt-cls">' + out.map(function (c) { return channelLine(c, true); }).join("") + inn.map(function (c) { return channelLine(c, false); }).join("") + "</ul>");
    var cs = X.model.concernsOfActor(a.id), fs = X.model.flowsOf("actor", a.id);
    if (cs.length) h += sec("Concerns", cs.length, '<div class="chips">' + cs.map(function (c) { return R.chip("concern:" + c); }).join("") + "</div>");
    if (fs.length) h += sec("Flows", fs.length, '<ul class="rt-flows">' + fs.map(function (f) {
      return "<li>" + R.chip("flow:" + f.id) + '<span class="stepnos">' + flowSteps(f, function (s) { return s.at === a.id || s.from === a.id || s.to === a.id; }) + "</span></li>";
    }).join("") + "</ul>");
    h += X.backlinks.section("actor:" + a.id);
    h += '<p><a class="btn" href="' + esc(X.router.href("runtime", "", { i: "actor:" + a.id })) + '">' + U.icon("full") + "Show on the runtime map</a></p>";
    var el = U.h("div.insp-pad.sp", { html: h });
    body.appendChild(el);
    var all = el.querySelector('[data-a="allpk"]');
    if (all) all.onclick = function () { el.querySelector(".runs.capped").classList.remove("capped"); all.remove(); };
  });

  X.inspector.register("channel", function (info, body) {
    var c = info.obj;
    var h = '<div class="ins-ends">' + R.chip("actor:" + c.from) + '<span class="ins-arrow' + (c.boundary ? " bnd" : "") + '">' + (c.boundary ? '<i class="gate"></i>' : "") + "</span>" + R.chip("actor:" + c.to) + "</div>";
    if (c.boundary) h += '<p class="ins-bnd"><span class="tag warn">trust boundary</span> <span class="muted small">What arrives at ' + esc((D.actor(c.to) || {}).title || c.to) + " comes from a less or differently trusted side.</span></p>";
    h += '<dl class="facts">' + (c.transport ? "<dt>Transport</dt><dd>" + X.md.inline(c.transport, {}) + "</dd>" : "") + (c.carries ? "<dt>Carries</dt><dd>" + X.md.inline(c.carries, {}) + "</dd>" : "") + "</dl>";
    if ((c.checks || []).length) h += sec("Where what arrives is checked", c.checks.length, '<ul class="sp-members">' + c.checks.map(function (x) { return "<li>" + R.chip(x) + firstDoc(x) + "</li>"; }).join("") + "</ul>");
    else if (c.boundary) h += sec("Where what arrives is checked", 0, '<p class="muted small">The model names no check for this boundary.</p>');
    if ((c.concerns || []).length) h += sec("Concerns", c.concerns.length, '<div class="chips">' + c.concerns.map(function (x) { return R.chip("concern:" + x); }).join("") + "</div>");
    var fs = X.model.flowsOf("channel", c.id);
    if (fs.length) h += sec("Flows that use it", fs.length, '<ul class="rt-flows">' + fs.map(function (f) {
      return "<li>" + R.chip("flow:" + f.id) + '<span class="stepnos">' + flowSteps(f, function (s) { var x = X.model.channelOf(s); return x && x.id === c.id; }) + "</span></li>";
    }).join("") + "</ul>");
    h += X.backlinks.section("channel:" + c.id);
    body.appendChild(U.h("div.insp-pad.sp", { html: h }));
  });

  X.inspector.register("zone", function (info, body) {
    var z = info.obj;
    var acts = (M.actors || []).filter(function (a) { return a.zone === z.id; });
    var ids = new Set(acts.map(function (a) { return a.id; }));
    var cross = (M.channels || []).filter(function (c) { return ids.has(c.from) !== ids.has(c.to); });
    var h = '<div class="ins-head z-' + esc(z.trust) + '"><i class="zd z-' + esc(z.trust) + '"></i><span>trust: ' + esc(z.trust) + "</span></div>";
    if (z.summary) h += '<div class="doc prose ins-lead">' + X.md.render(z.summary, {}) + "</div>";
    h += sec("Actors", acts.length, '<ul class="rt-cls">' + acts.map(function (a) {
      return '<li>' + U.icon(KIND_ICON[a.kind] || "actor") + R.chip("actor:" + a.id, { cls: "flat", noZone: true }) + (isCode(a) ? '<span class="muted small">devc code</span>' : "") + "</li>";
    }).join("") + "</ul>");
    if (cross.length) h += sec("Channels across its edge", cross.length, '<ul class="rt-cls">' + cross.map(function (c) { return channelLine(c, ids.has(c.from)); }).join("") + "</ul>");
    body.appendChild(U.h("div.insp-pad.sp", { html: h }));
  });

  X.search.addSource({
    kind: "actor", title: "Actors and channels", order: 6,
    items: function () {
      return (M.actors || []).map(function (a) { return { ref: "actor:" + a.id, name: a.title, keys: [a.title, a.id], detail: (D.zone(a.zone) || {}).title, exported: true, weight: 30 }; })
        .concat((M.channels || []).map(function (c) { return { ref: "channel:" + c.id, name: c.title, keys: [c.title, c.id], detail: ((D.actor(c.from) || {}).title || c.from) + " → " + ((D.actor(c.to) || {}).title || c.to), exported: true, weight: 20 }; }))
        .concat((M.zones || []).map(function (z) { return { ref: "zone:" + z.id, name: z.title, keys: [z.title, z.id], detail: "zone · trust " + z.trust, exported: true, weight: 20 }; }));
    }
  });
})();
