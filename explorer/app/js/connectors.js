// The connector layer: draws connectors between HTML elements in an SVG
// that covers a host element. Boxes stay HTML (text wraps, everything is
// clickable); only the lines are SVG. Redrawn when the host or the window
// resizes; scrolling inside the host needs nothing because the SVG scrolls
// with the content.
//
//   var layer = X.connectors.create(host, {
//     above: false,              the SVG above the boxes (else below)
//     labels: "svg" | "html",    how labels are drawn (html: boxes that wrap,
//                                placed where they cover no obstacle)
//     obstacles: fn() -> [el],   boxes that routes and labels avoid
//     router: "grid",            orthogonal routes, for edges with avoid, through
//                                the gaps between obstacles (A* on a grid of `cell` px with
//                                costs for turns and for tracks taken by
//                                earlier edges, so parallel edges run side
//                                by side); edges keep curves without it
//     cell: 6,                   the router's grid
//     labelWhen: fn(edge) -> bool  which labels to show and place (default
//                                all); re-evaluated by setClass
//     labelAway: fn(edge) -> el  a box the edge's label should keep away
//                                from (the actor in focus: labels then sit
//                                near the other ends and spread out)
//     onDraw: fn(edges)          after every redraw (edge._g, edge._path)
//   })
//   layer.set([{from: el, to: el, cls: "imports", arrow: true,
//               curve: true, label: "text", sides: "auto"|"v"|"h"|[a, b],
//               bend: px, avoid: bool, hit: bool, ref: "reference",
//               id: "any", data: anything}, ...])
//   layer.redraw()
//   layer.each(fn(edge, groupEl))       e.g. to toggle classes
//   layer.setClass(fn(edge) -> "cls" | "")  extra class per edge
//   layer.edgeOf(el)                    the edge of a hit area or label
//   layer.pointOnEdge(edge, t)          a point at fraction t of its length
//   layer.destroy()
//
// host must be positioned (relative) and should be the scrolling element
// or inside it. Endpoints sit on the facing sides of the two boxes;
// several connectors on one side are spread along it, ordered by where the
// other end is, so that they do not cross near the box. Parallel edges
// (several between one pair) therefore run side by side. An edge from a
// box to itself is a loop on the side with the fewest other ends.
// avoid: the route is chosen among a few candidates (sides and bends) so
// that it crosses no obstacle other than its own ends. hit: a wide
// invisible stroke carries data-ref (the shared hover card and click) and
// data-cx (layer.edgeOf). Styling is by CSS: .cx-edge (path), .cx-head
// (arrowhead), .cx-label (SVG text) or .cx-lab (HTML label), .cx-hit, with
// the edge's classes on its <g class="cx ..."> and its HTML label.
X.connectors = (function () {
  "use strict";
  var NS = "http://www.w3.org/2000/svg";
  var SIDES = ["top", "right", "bottom", "left"];

  function create(host, opts) {
    opts = opts || {};
    var svg = document.createElementNS(NS, "svg");
    svg.setAttribute("class", "cx-layer" + (opts.above ? " above" : ""));
    svg.setAttribute("aria-hidden", "true");
    host.insertBefore(svg, host.firstChild);
    var labelBox = null;
    if (opts.labels === "html") {
      labelBox = document.createElement("div");
      labelBox.className = "cx-labels";
      host.appendChild(labelBox);
    }
    var edges = [], extra = null, raf = 0, destroyed = false, lastObst = [];

    var ro = window.ResizeObserver ? new ResizeObserver(schedule) : null;
    if (ro) ro.observe(host);
    window.addEventListener("resize", schedule);

    function schedule() {
      if (raf || destroyed) return;
      raf = requestAnimationFrame(function () { raf = 0; redraw(); });
    }

    function rel(el, hr) {
      var r = el.getBoundingClientRect();
      return {
        x: r.left - hr.left + host.scrollLeft, y: r.top - hr.top + host.scrollTop,
        w: r.width, h: r.height
      };
    }

    // sides chooses where a connector leaves a box: vertical when the
    // boxes do not overlap vertically, else horizontal.
    function sides(a, b, mode) {
      if (Array.isArray(mode)) return mode;
      var vert;
      if (mode === "v") vert = true;
      else if (mode === "h") vert = false;
      else vert = (b.y >= a.y + a.h - 2) || (a.y >= b.y + b.h - 2);
      if (vert) return b.y + b.h / 2 >= a.y + a.h / 2 ? ["bottom", "top"] : ["top", "bottom"];
      return b.x + b.w / 2 >= a.x + a.w / 2 ? ["right", "left"] : ["left", "right"];
    }

    function redraw() {
      if (destroyed) return;
      var hr = host.getBoundingClientRect();
      svg.setAttribute("width", host.scrollWidth);
      svg.setAttribute("height", host.scrollHeight);
      svg.style.width = host.scrollWidth + "px";
      svg.style.height = host.scrollHeight + "px";
      while (svg.firstChild) svg.removeChild(svg.firstChild);
      if (labelBox) labelBox.innerHTML = "";
      var boxes = new Map();
      function box(el) {
        if (!boxes.has(el)) boxes.set(el, rel(el, hr));
        return boxes.get(el);
      }
      var obst = [];
      if (opts.obstacles) {
        (opts.obstacles() || []).forEach(function (el) {
          if (el && el.isConnected && el.offsetParent !== null) obst.push({ el: el, b: box(el) });
        });
      }
      var live = edges.filter(function (e) {
        return e.from && e.to && e.from.isConnected && e.to.isConnected && e.from.offsetParent !== null && e.to.offsetParent !== null;
      });
      // First pass: sides (and, for avoiding edges, the route).
      var ends = new Map();
      live.forEach(function (e) {
        e._bend = e.bend || 0;
        if (e.from === e.to) return;
        var a = box(e.from), b = box(e.to);
        e._s = sides(a, b, e.sides);
        if (e.avoid && obst.length) route(e, a, b, obst);
        addEnd(ends, e.from, e._s[0], e, b, "from");
        addEnd(ends, e.to, e._s[1], e, a, "to");
      });
      // Loops go on the side of their box with the fewest other ends.
      var loopSides = new Map();
      live.forEach(function (e) {
        if (e.from !== e.to) return;
        var best = "top", n = Infinity;
        ["top", "right", "bottom", "left"].forEach(function (s) {
          var c = 0;
          ends.forEach(function (list, k) { if (k.el === e.from && k.side === s) c = list.length; });
          var taken = loopSides.get(e.from) || [];
          c += taken.filter(function (x) { return x === s; }).length * 2;
          if (c < n) { n = c; best = s; }
        });
        e._loop = best;
        loopSides.set(e.from, (loopSides.get(e.from) || []).concat([best]));
      });
      // Second pass: the order along each side.
      ends.forEach(function (list, key) {
        var side = key.side, horiz = side === "top" || side === "bottom";
        var hasLoop = (loopSides.get(key.el) || []).indexOf(side) >= 0;
        list.sort(function (p, q) {
          var d = horiz ? (p.other.x + p.other.w / 2) - (q.other.x + q.other.w / 2) : (p.other.y + p.other.h / 2) - (q.other.y + q.other.h / 2);
          return d || p.order - q.order;
        });
        list.forEach(function (it, i) {
          var t = (i + 1) / (list.length + 1);
          if (hasLoop) t = t * 0.62; // leave the far end of the side to the loop
          it.e["_t" + it.end] = t;
        });
      });
      var placed = [];
      lastObst = obst;
      // The router serves the edges that ask to avoid obstacles.
      var grid = opts.router === "grid" && live.some(function (e) { return e.avoid; }) ? makeGrid(host.scrollWidth, host.scrollHeight, obst, opts.cell || 6) : null;
      var order2 = live.slice();
      if (grid) {
        // Short edges first: they have few choices; long ones go around.
        order2.sort(function (p, q) { return manhattan(box(p.from), box(p.to)) - manhattan(box(q.from), box(q.to)); });
      }
      order2.forEach(function (e) {
        if (e.from === e.to) drawLoop(e, box(e.from), placed, obst);
        else if (grid && e.avoid && drawOrtho(e, box(e.from), box(e.to), grid, placed, obst)) { /* routed */ }
        else draw(e, box(e.from), box(e.to), placed, obst);
      });
      if (opts.onDraw) { try { opts.onDraw(live); } catch (err) { console.error(err); } }
    }

    var order = 0;
    function addEnd(ends, el, side, e, other, end) {
      var key = null;
      ends.forEach(function (_, k) { if (k.el === el && k.side === side) key = k; });
      if (!key) { key = { el: el, side: side }; ends.set(key, []); }
      ends.get(key).push({ e: e, other: other, end: end, order: order++ });
    }

    function manhattan(a, b) { return Math.abs(a.x + a.w / 2 - b.x - b.w / 2) + Math.abs(a.y + a.h / 2 - b.y - b.h / 2); }

    // ----- the grid router

    var DX = [0, 1, 0, -1], DY = [-1, 0, 1, 0]; // up, right, down, left
    var SIDE_DIR = { top: 0, right: 1, bottom: 2, left: 3 };
    function makeGrid(W, H, obst, cell) {
      while ((W / cell) * (H / cell) > 400000) cell += 2;
      var cols = Math.ceil(W / cell) + 1, rows = Math.ceil(H / cell) + 1;
      var owner = new Int16Array(cols * rows); // 0 free, else obstacle index + 1
      var pad = Math.max(3, cell * 0.6);
      obst.forEach(function (o, k) {
        var b = o.b;
        var c0 = Math.max(0, Math.floor((b.x - pad) / cell)), c1 = Math.min(cols - 1, Math.ceil((b.x + b.w + pad) / cell));
        var r0 = Math.max(0, Math.floor((b.y - pad) / cell)), r1 = Math.min(rows - 1, Math.ceil((b.y + b.h + pad) / cell));
        for (var r = r0; r <= r1; r++) for (var c = c0; c <= c1; c++) {
          var x = c * cell, y = r * cell;
          if (x > b.x - pad && x < b.x + b.w + pad && y > b.y - pad && y < b.y + b.h + pad) owner[r * cols + c] = k + 1;
        }
      });
      return { cell: cell, cols: cols, rows: rows, owner: owner, used: new Uint8Array(cols * rows), obst: obst, pad: pad };
    }
    // ring lists the free cells just outside a box, with the side each
    // faces; corners are left out so that routes leave a side straight.
    function ring(G, b) {
      var out = [], c = G.cell, d = G.pad + c * 0.5;
      var c0 = Math.ceil((b.x + 2) / c), c1 = Math.floor((b.x + b.w - 2) / c);
      var r0 = Math.ceil((b.y + 2) / c), r1 = Math.floor((b.y + b.h - 2) / c);
      var top = Math.floor((b.y - d) / c), bottom = Math.ceil((b.y + b.h + d) / c);
      var left = Math.floor((b.x - d) / c), right = Math.ceil((b.x + b.w + d) / c);
      function add(col, row, side) {
        if (col < 0 || row < 0 || col >= G.cols || row >= G.rows) return;
        var i = row * G.cols + col;
        if (G.owner[i]) return;
        out.push({ i: i, side: side });
      }
      for (var x = c0; x <= c1; x++) { add(x, top, "top"); add(x, bottom, "bottom"); }
      for (var y = r0; y <= r1; y++) { add(left, y, "left"); add(right, y, "right"); }
      return out;
    }
    function Heap() { this.f = []; this.v = []; }
    Heap.prototype.push = function (f, v) {
      var a = this.f, b = this.v, i = a.length;
      a.push(f); b.push(v);
      while (i > 0) { var p = (i - 1) >> 1; if (a[p] <= f) break; a[i] = a[p]; b[i] = b[p]; i = p; }
      a[i] = f; b[i] = v;
    };
    Heap.prototype.pop = function () {
      var a = this.f, b = this.v, top = b[0], lf = a.pop(), lv = b.pop(), n = a.length;
      if (n) {
        var i = 0;
        for (;;) {
          var l = 2 * i + 1, r = l + 1, m = i;
          var fm = lf;
          if (l < n && a[l] < fm) { m = l; fm = a[l]; }
          if (r < n && a[r] < fm) { m = r; }
          if (m === i) break;
          a[i] = a[m]; b[i] = b[m]; i = m;
        }
        a[i] = lf; b[i] = lv;
      }
      return top;
    };
    // gridRoute finds the cheapest path from a ring cell of a to one of b.
    function gridRoute(G, a, b) {
      var starts = ring(G, a), goals = ring(G, b);
      if (!starts.length || !goals.length) return null;
      var goalSide = new Map();
      goals.forEach(function (g) { goalSide.set(g.i, g.side); });
      var cols = G.cols, n = cols * G.rows, TURN = 5, CONG = 7;
      var gs = new Float32Array(n * 4).fill(Infinity), came = new Int32Array(n * 4).fill(-1);
      var bx0 = (b.x) / G.cell, bx1 = (b.x + b.w) / G.cell, by0 = b.y / G.cell, by1 = (b.y + b.h) / G.cell;
      function h(i) {
        var x = i % cols, y = (i - x) / cols;
        return Math.max(0, bx0 - x, x - bx1) + Math.max(0, by0 - y, y - by1);
      }
      var heap = new Heap();
      // Leaving on the side that faces the target costs nothing.
      var fx = b.x + b.w / 2 - a.x - a.w / 2, fy = b.y + b.h / 2 - a.y - a.h / 2;
      starts.forEach(function (s) {
        var d = SIDE_DIR[s.side];
        var away = (DX[d] * fx + DY[d] * fy) < 0 ? 6 : 0;
        var st = s.i * 4 + d, g = away + G.used[s.i] * CONG;
        if (g < gs[st]) { gs[st] = g; heap.push(g + h(s.i), st); }
      });
      var end = -1, guard = 0;
      while (heap.f.length && guard++ < 600000) {
        var st = heap.pop(), i = st >> 2, d = st & 3, g = gs[st];
        if (goalSide.has(i) && SIDE_DIR[goalSide.get(i)] !== d) { end = st; break; }
        var x = i % cols, y = (i - x) / cols;
        for (var nd = 0; nd < 4; nd++) {
          if (nd === ((d + 2) & 3)) continue;
          var nx = x + DX[nd], ny = y + DY[nd];
          if (nx < 0 || ny < 0 || nx >= cols || ny >= G.rows) continue;
          var ni = ny * cols + nx;
          if (G.owner[ni]) continue;
          var ng = g + 1 + (nd !== d ? TURN : 0) + G.used[ni] * CONG;
          // Entering a box other than straight on costs a turn.
          if (goalSide.has(ni) && SIDE_DIR[goalSide.get(ni)] !== ((nd + 2) & 3)) ng += TURN;
          var ns = ni * 4 + nd;
          if (ng < gs[ns]) { gs[ns] = ng; came[ns] = st; heap.push(ng + h(ni), ns); }
        }
      }
      if (end < 0) return null;
      var cells = [];
      for (var s2 = end; s2 >= 0; s2 = came[s2]) cells.push(s2);
      cells.reverse();
      var startSide = ["top", "right", "bottom", "left"][cells[0] & 3];
      var pts = cells.map(function (q) { var k = q >> 2, cx = k % cols; return [cx * G.cell, (k - cx) / cols * G.cell]; });
      // Mark the track as taken, and its neighbours a little.
      cells.forEach(function (q) {
        var k = q >> 2;
        G.used[k] = Math.min(250, G.used[k] + 2);
        var kx = k % cols;
        if (kx > 0) G.used[k - 1] = Math.min(250, G.used[k - 1] + 1);
        if (kx < cols - 1) G.used[k + 1] = Math.min(250, G.used[k + 1] + 1);
        if (k >= cols) G.used[k - cols] = Math.min(250, G.used[k - cols] + 1);
        if (k + cols < n) G.used[k + cols] = Math.min(250, G.used[k + cols] + 1);
      });
      return { pts: pts, from: startSide, to: goalSide.get(cells[cells.length - 1] >> 2) };
    }
    // drawOrtho draws a routed edge: from the box's edge, along the cells,
    // into the other box's edge, with rounded corners.
    function drawOrtho(e, a, b, G, placed, obst) {
      var r = gridRoute(G, a, b);
      if (!r) return false;
      var pts = r.pts.slice();
      var p0 = pts[0], pn = pts[pts.length - 1];
      var s0 = r.from, s1 = r.to;
      var start = s0 === "top" ? [p0[0], a.y] : s0 === "bottom" ? [p0[0], a.y + a.h] : s0 === "left" ? [a.x, p0[1]] : [a.x + a.w, p0[1]];
      var tip = s1 === "top" ? [pn[0], b.y] : s1 === "bottom" ? [pn[0], b.y + b.h] : s1 === "left" ? [b.x, pn[1]] : [b.x + b.w, pn[1]];
      pts.unshift(start);
      pts.push(tip);
      pts = simplify(pts);
      var gap = e.arrow ? 6 : 0, last = pts[pts.length - 1], prev = pts[pts.length - 2];
      var len = Math.hypot(last[0] - prev[0], last[1] - prev[1]) || 1;
      var endPt = [last[0] - (last[0] - prev[0]) / len * gap, last[1] - (last[1] - prev[1]) / len * gap];
      var drawn = pts.slice(0, -1).concat([endPt]);
      var d = rounded(drawn, 7);
      var g = group(e);
      var path = pathEl("cx-edge", d);
      g.appendChild(path);
      e._path = path;
      e._pts = pts;
      if (e.arrow) g.appendChild(arrowHead(tip, prev));
      // Labels: the middles of the longest segments first.
      var segs = [];
      for (var i = 1; i < pts.length; i++) segs.push([pts[i - 1], pts[i], Math.hypot(pts[i][0] - pts[i - 1][0], pts[i][1] - pts[i - 1][1])]);
      segs.sort(function (p, q) { return q[2] - p[2]; });
      var cands = [];
      segs.slice(0, 6).forEach(function (sg) { [0.5, 0.35, 0.65, 0.2, 0.8].forEach(function (t) { cands.push([sg[0][0] + (sg[1][0] - sg[0][0]) * t, sg[0][1] + (sg[1][1] - sg[0][1]) * t]); }); });
      finish(e, g, d, cands, placed, obst);
      return true;
    }
    function simplify(pts) {
      var out = [pts[0]];
      for (var i = 1; i < pts.length - 1; i++) {
        var a = out[out.length - 1], b = pts[i], c = pts[i + 1];
        var col = (Math.abs(a[0] - b[0]) < 0.5 && Math.abs(b[0] - c[0]) < 0.5) || (Math.abs(a[1] - b[1]) < 0.5 && Math.abs(b[1] - c[1]) < 0.5);
        if (!col) out.push(b);
      }
      out.push(pts[pts.length - 1]);
      return out;
    }
    function rounded(pts, rad) {
      var d = "M" + f(pts[0]);
      for (var i = 1; i < pts.length - 1; i++) {
        var a = pts[i - 1], b = pts[i], c = pts[i + 1];
        var l1 = Math.hypot(b[0] - a[0], b[1] - a[1]), l2 = Math.hypot(c[0] - b[0], c[1] - b[1]);
        var r = Math.min(rad, l1 / 2, l2 / 2);
        var p = [b[0] - (b[0] - a[0]) / (l1 || 1) * r, b[1] - (b[1] - a[1]) / (l1 || 1) * r];
        var q = [b[0] + (c[0] - b[0]) / (l2 || 1) * r, b[1] + (c[1] - b[1]) / (l2 || 1) * r];
        d += "L" + f(p) + "Q" + f(b) + " " + f(q);
      }
      return d + "L" + f(pts[pts.length - 1]);
    }

    function point(b, side, t) {
      // Spread along the middle 70% of the side.
      var f = 0.15 + 0.7 * (t == null ? 0.5 : t);
      switch (side) {
        case "top": return [b.x + b.w * f, b.y];
        case "bottom": return [b.x + b.w * f, b.y + b.h];
        case "left": return [b.x, b.y + b.h * f];
        default: return [b.x + b.w, b.y + b.h * f];
      }
    }
    function normal(side) {
      return side === "top" ? [0, -1] : side === "bottom" ? [0, 1] : side === "left" ? [-1, 0] : [1, 0];
    }

    // geometry is the cubic of an edge between two boxes.
    function geometry(e, a, b, s, tf, tt, bend) {
      var p0 = point(a, s[0], tf), p3 = point(b, s[1], tt);
      var n0 = normal(s[0]), n3 = normal(s[1]);
      var dist = Math.hypot(p3[0] - p0[0], p3[1] - p0[1]);
      var k = e.curve === false ? 0 : Math.max(18, Math.min(120, dist * 0.42));
      if (s[0] === s[1]) k = Math.max(k, 34); // an arc out of one side and back
      var arrowGap = e.arrow ? 6 : 0;
      var pe = [p3[0] + n3[0] * arrowGap, p3[1] + n3[1] * arrowGap];
      var c1 = [p0[0] + n0[0] * k, p0[1] + n0[1] * k], c2 = [pe[0] + n3[0] * k, pe[1] + n3[1] * k];
      if (bend) {
        var dx = pe[0] - p0[0], dy = pe[1] - p0[1], len = Math.hypot(dx, dy) || 1;
        var px = -dy / len * bend, py = dx / len * bend;
        c1 = [c1[0] + px, c1[1] + py]; c2 = [c2[0] + px, c2[1] + py];
      }
      return { p0: p0, c1: c1, c2: c2, pe: pe, p3: p3, straight: e.curve === false && !bend };
    }

    // route picks, for an edge that avoids obstacles, the sides and bend
    // of the shortest candidate that crosses none (else the fewest).
    function route(e, a, b, obst) {
      var others = obst.filter(function (o) { return o.el !== e.from && o.el !== e.to; });
      if (!others.length) return;
      var base = e._s, cands = [];
      var span = Math.max(30, Math.min(90, Math.hypot(b.x - a.x, b.y - a.y) * 0.25));
      [0, 1, -1, 2, -2].forEach(function (k) { cands.push({ s: base, bend: k * span }); });
      var vert = base[0] === "top" || base[0] === "bottom";
      if (vert) { cands.push({ s: ["left", "left"], bend: 0 }); cands.push({ s: ["right", "right"], bend: 0 }); }
      else { cands.push({ s: ["top", "top"], bend: 0 }); cands.push({ s: ["bottom", "bottom"], bend: 0 }); }
      var best = null;
      cands.forEach(function (c, i) {
        var g = geometry(e, a, b, c.s, 0.5, 0.5, c.bend);
        var pts = sample(g, 28), hits = 0, len = 0;
        for (var j = 1; j < pts.length; j++) len += Math.hypot(pts[j][0] - pts[j - 1][0], pts[j][1] - pts[j - 1][1]);
        others.forEach(function (o) { if (pts.some(function (p) { return inside(p, o.b, 3); })) hits++; });
        var score = hits * 100000 + len + i * 4;
        if (!best || score < best.score) best = { score: score, c: c };
      });
      e._s = best.c.s;
      e._bend = best.c.bend;
    }
    function sample(g, n) {
      var out = [];
      for (var i = 0; i <= n; i++) {
        var t = i / n;
        out.push(g.straight ? [g.p0[0] + (g.pe[0] - g.p0[0]) * t, g.p0[1] + (g.pe[1] - g.p0[1]) * t] : bez(g.p0, g.c1, g.c2, g.pe, t));
      }
      return out;
    }
    function inside(p, b, pad) {
      return p[0] > b.x - pad && p[0] < b.x + b.w + pad && p[1] > b.y - pad && p[1] < b.y + b.h + pad;
    }

    function group(e) {
      var g = document.createElementNS(NS, "g");
      g.setAttribute("class", "cx " + (e.cls || "") + (extra ? " " + (extra(e) || "") : ""));
      return g;
    }
    function pathEl(cls, d) {
      var p = document.createElementNS(NS, "path");
      p.setAttribute("class", cls);
      p.setAttribute("d", d);
      return p;
    }
    function arrowHead(tip, from) {
      var dx = tip[0] - from[0], dy = tip[1] - from[1];
      var len = Math.hypot(dx, dy) || 1;
      dx /= len; dy /= len;
      var L = 7, W = 3.6, bx = tip[0] - dx * L, by = tip[1] - dy * L;
      return pathEl("cx-head", "M" + f(tip) + "L" + f([bx - dy * W, by + dx * W]) + "L" + f([bx + dy * W, by - dx * W]) + "Z");
    }

    function finish(e, g, d, labelCands, placed, obst) {
      e._cands = labelCands;
      if (e.hit) {
        var hit = pathEl("cx-hit", d);
        if (e.ref) hit.setAttribute("data-ref", e.ref);
        hit.setAttribute("data-cx", String(edges.indexOf(e)));
        g.appendChild(hit);
      }
      e._g = g;
      svg.appendChild(g);
      if (e.label && (!opts.labelWhen || opts.labelWhen(e))) label(e, labelCands, placed, obst);
    }

    function draw(e, a, b, placed, obst) {
      var g0 = geometry(e, a, b, e._s, e._tfrom, e._tto, e._bend);
      var g = group(e);
      var d = g0.straight ? "M" + f(g0.p0) + "L" + f(g0.pe) : "M" + f(g0.p0) + "C" + f(g0.c1) + " " + f(g0.c2) + " " + f(g0.pe);
      var path = pathEl("cx-edge", d);
      g.appendChild(path);
      e._path = path;
      if (e.arrow) g.appendChild(arrowHead(g0.p3, g0.straight ? g0.p0 : g0.c2));
      var cands = [0.5, 0.4, 0.6, 0.3, 0.7, 0.22, 0.78].map(function (t) {
        return g0.straight ? [g0.p0[0] + (g0.pe[0] - g0.p0[0]) * t, g0.p0[1] + (g0.pe[1] - g0.p0[1]) * t] : bez(g0.p0, g0.c1, g0.c2, g0.pe, t);
      });
      finish(e, g, d, cands, placed, obst);
    }

    // drawLoop draws an edge from a box to itself: out of one side near
    // its far end and back, with the arrowhead on the way in.
    function drawLoop(e, b, placed, obst) {
      var s = e._loop || "top", n = normal(s), R = 30;
      var p0 = point(b, s, 0.72), p3 = point(b, s, 0.94);
      var along = (s === "top" || s === "bottom") ? [1, 0] : [0, 1];
      var c1 = [p0[0] + n[0] * R - along[0] * 10, p0[1] + n[1] * R - along[1] * 10];
      var c2 = [p3[0] + n[0] * R + along[0] * 14, p3[1] + n[1] * R + along[1] * 14];
      var pe = [p3[0] + n[0] * (e.arrow ? 6 : 0), p3[1] + n[1] * (e.arrow ? 6 : 0)];
      var g = group(e);
      var d = "M" + f(p0) + "C" + f(c1) + " " + f(c2) + " " + f(pe);
      var path = pathEl("cx-edge", d);
      g.appendChild(path);
      e._path = path;
      if (e.arrow) g.appendChild(arrowHead(p3, c2));
      var top = bez(p0, c1, c2, pe, 0.5);
      var cand = [top[0] + n[0] * 4, top[1] + n[1] * 4];
      finish(e, g, d, [cand], placed, obst);
    }

    // label places an edge's label at the first candidate point where it
    // covers no obstacle and no label placed before; else where it covers
    // the least.
    function label(e, cands, placed, obst) {
      if (!labelBox) {
        var m = cands[0];
        var t = document.createElementNS(NS, "text");
        t.setAttribute("class", "cx-label");
        t.setAttribute("x", m[0].toFixed(1));
        t.setAttribute("y", (m[1] - 4).toFixed(1));
        t.setAttribute("text-anchor", "middle");
        t.textContent = e.label;
        e._g.appendChild(t);
        return;
      }
      var el = document.createElement("div");
      el.className = "cx-lab " + (e.cls || "") + (extra ? " " + (extra(e) || "") : "");
      el.textContent = e.label;
      if (e.ref) el.setAttribute("data-ref", e.ref);
      el.setAttribute("data-cx", String(edges.indexOf(e)));
      labelBox.appendChild(el);
      var w = el.offsetWidth, h = el.offsetHeight, best = null;
      var loop = e.from === e.to;
      var away = opts.labelAway ? opts.labelAway(e) : null;
      if (away && away.isConnected) {
        var hr = host.getBoundingClientRect(), ab = rel(away, hr), ax = ab.x + ab.w / 2, ay = ab.y + ab.h / 2;
        cands = cands.slice().sort(function (p, q) { return Math.hypot(q[0] - ax, q[1] - ay) - Math.hypot(p[0] - ax, p[1] - ay); });
      }
      cands.forEach(function (c, i) {
        var r = { x: c[0] - w / 2, y: c[1] - h / 2, w: w, h: h };
        if (loop) {
          var n = normal(e._loop || "top");
          r.x += n[0] * (w / 2); r.y += n[1] * (h / 2);
        }
        var cover = 0;
        obst.forEach(function (o) { cover += overlap(r, o.b); });
        placed.forEach(function (p) { cover += overlap(r, p) * 2; });
        var score = cover * 1000 + i;
        if (!best || score < best.score) best = { score: score, r: r };
      });
      el.style.left = Math.round(best.r.x) + "px";
      el.style.top = Math.round(best.r.y) + "px";
      placed.push(best.r);
      e._label = el;
    }
    function overlap(a, b) {
      var x = Math.max(0, Math.min(a.x + a.w, b.x + b.w) - Math.max(a.x, b.x));
      var y = Math.max(0, Math.min(a.y + a.h, b.y + b.h) - Math.max(a.y, b.y));
      return x * y;
    }

    function bez(p0, p1, p2, p3, t) {
      var u = 1 - t;
      return [u * u * u * p0[0] + 3 * u * u * t * p1[0] + 3 * u * t * t * p2[0] + t * t * t * p3[0],
        u * u * u * p0[1] + 3 * u * u * t * p1[1] + 3 * u * t * t * p2[1] + t * t * t * p3[1]];
    }
    function f(p) { return p[0].toFixed(1) + "," + p[1].toFixed(1); }

    return {
      svg: svg,
      set: function (list) { edges = list || []; redraw(); },
      redraw: redraw,
      schedule: schedule,
      edges: function () { return edges; },
      each: function (fn) {
        edges.forEach(function (e) { if (e._g) fn(e, e._g, e._label || null); });
      },
      // setClass changes the edges' classes in place (no new routes).
      setClass: function (fn) {
        extra = fn;
        edges.forEach(function (e) {
          var c = (e.cls || "") + (extra ? " " + (extra(e) || "") : "");
          if (e._g) e._g.setAttribute("class", "cx " + c);
          if (e._label) e._label.className = "cx-lab " + c;
        });
        if (opts.labelWhen && labelBox) {
          // Place the labels that now show, avoiding each other only.
          labelBox.innerHTML = "";
          var placed = [];
          edges.forEach(function (e) { e._label = null; });
          edges.forEach(function (e) { if (e._g && e.label && e._cands && opts.labelWhen(e)) label(e, e._cands, placed, lastObst); });
        }
      },
      edgeOf: function (el) {
        var n = el && el.closest ? el.closest("[data-cx]") : null;
        return n ? edges[+n.getAttribute("data-cx")] || null : null;
      },
      pointOnEdge: function (e, t) {
        if (!e._path) return null;
        var L = e._path.getTotalLength(), p = e._path.getPointAtLength(L * t);
        return [p.x, p.y];
      },
      destroy: function () {
        destroyed = true;
        if (ro) ro.disconnect();
        window.removeEventListener("resize", schedule);
        if (raf) cancelAnimationFrame(raf);
        svg.remove();
        if (labelBox) labelBox.remove();
      }
    };
  }

  return { create: create, SIDES: SIDES };
})();
