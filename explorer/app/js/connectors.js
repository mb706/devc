// The connector layer: draws connectors between HTML elements in an SVG
// that covers a host element. Boxes stay HTML (text wraps, everything is
// clickable); only the lines are SVG. Redrawn when the host or the window
// resizes; scrolling inside the host needs nothing because the SVG scrolls
// with the content.
//
//   var layer = X.connectors.create(host, {above: false})
//   layer.set([{from: el, to: el, cls: "imports", arrow: true,
//               curve: true, label: "text", sides: "auto"|"v"|"h",
//               id: "any", data: anything}, ...])
//   layer.redraw()
//   layer.each(fn(edge, pathEl))      e.g. to toggle classes
//   layer.setClass(fn(edge) -> "cls" | "")  extra class per edge
//   layer.destroy()
//
// host must be positioned (relative) and should be the scrolling
// element or inside it. Endpoints sit on the facing sides of the two
// boxes; several connectors on one side are spread along it. Styling is
// by CSS: .cx-edge (path), .cx-head (arrowhead), .cx-label (text) with
// the edge's classes on its <g class="cx ...">.
X.connectors = (function () {
  "use strict";
  var NS = "http://www.w3.org/2000/svg";

  function create(host, opts) {
    opts = opts || {};
    var svg = document.createElementNS(NS, "svg");
    svg.setAttribute("class", "cx-layer" + (opts.above ? " above" : ""));
    svg.setAttribute("aria-hidden", "true");
    host.insertBefore(svg, host.firstChild);
    var edges = [], extra = null, raf = 0, destroyed = false;

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

    // side chooses where a connector leaves a box: vertical when the boxes
    // do not overlap vertically, else horizontal.
    function sides(a, b, mode) {
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
      var boxes = new Map();
      function box(el) {
        if (!boxes.has(el)) boxes.set(el, rel(el, hr));
        return boxes.get(el);
      }
      // First pass: sides and the order along each side.
      var ends = new Map();
      var live = edges.filter(function (e) {
        return e.from && e.to && e.from.isConnected && e.to.isConnected && e.from.offsetParent !== null && e.to.offsetParent !== null;
      });
      live.forEach(function (e) {
        var a = box(e.from), b = box(e.to);
        var s = sides(a, b, e.sides);
        e._s = s;
        addEnd(ends, e.from, s[0], e, b);
        addEnd(ends, e.to, s[1], e, a);
      });
      ends.forEach(function (list, key) {
        var side = key.side, horiz = side === "top" || side === "bottom";
        list.sort(function (p, q) {
          return horiz ? (p.other.x + p.other.w / 2) - (q.other.x + q.other.w / 2) : (p.other.y + p.other.h / 2) - (q.other.y + q.other.h / 2);
        });
        list.forEach(function (it, i) { it.e["_t" + (it.e.from === key.el && it.e._s[0] === side ? "from" : "to")] = (i + 1) / (list.length + 1); });
      });
      live.forEach(function (e) { draw(e, box(e.from), box(e.to)); });
    }

    function addEnd(ends, el, side, e, other) {
      var key = null;
      ends.forEach(function (_, k) { if (k.el === el && k.side === side) key = k; });
      if (!key) { key = { el: el, side: side }; ends.set(key, []); }
      ends.get(key).push({ e: e, other: other });
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

    function draw(e, a, b) {
      var s = e._s;
      var p0 = point(a, s[0], e._tfrom), p3 = point(b, s[1], e._tto);
      var n0 = normal(s[0]), n3 = normal(s[1]);
      var dist = Math.hypot(p3[0] - p0[0], p3[1] - p0[1]);
      var k = e.curve === false ? 0 : Math.max(18, Math.min(120, dist * 0.42));
      var arrowGap = e.arrow ? 6 : 0;
      var pe = [p3[0] + n3[0] * arrowGap, p3[1] + n3[1] * arrowGap];
      var c1 = [p0[0] + n0[0] * k, p0[1] + n0[1] * k], c2 = [pe[0] + n3[0] * k, pe[1] + n3[1] * k];
      var g = document.createElementNS(NS, "g");
      g.setAttribute("class", "cx " + (e.cls || "") + (extra ? " " + (extra(e) || "") : ""));
      var path = document.createElementNS(NS, "path");
      path.setAttribute("class", "cx-edge");
      path.setAttribute("d", e.curve === false ?
        "M" + f(p0) + "L" + f(pe) :
        "M" + f(p0) + "C" + f(c1) + " " + f(c2) + " " + f(pe));
      g.appendChild(path);
      if (e.arrow) {
        // The arrowhead points along the curve's last tangent.
        var dx = pe[0] - (e.curve === false ? p0[0] : c2[0]), dy = pe[1] - (e.curve === false ? p0[1] : c2[1]);
        var len = Math.hypot(dx, dy) || 1;
        dx /= len; dy /= len;
        var tip = p3, L = 7, W = 3.6;
        var bx = tip[0] - dx * L, by = tip[1] - dy * L;
        var head = document.createElementNS(NS, "path");
        head.setAttribute("class", "cx-head");
        head.setAttribute("d", "M" + f(tip) + "L" + f([bx - dy * W, by + dx * W]) + "L" + f([bx + dy * W, by - dx * W]) + "Z");
        g.appendChild(head);
      }
      if (e.label) {
        var m = e.curve === false ? [(p0[0] + pe[0]) / 2, (p0[1] + pe[1]) / 2] : bez(p0, c1, c2, pe, 0.5);
        var t = document.createElementNS(NS, "text");
        t.setAttribute("class", "cx-label");
        t.setAttribute("x", m[0].toFixed(1));
        t.setAttribute("y", (m[1] - 4).toFixed(1));
        t.setAttribute("text-anchor", "middle");
        t.textContent = e.label;
        g.appendChild(t);
      }
      e._g = g;
      svg.appendChild(g);
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
      each: function (fn) { edges.forEach(function (e) { if (e._g) fn(e, e._g); }); },
      setClass: function (fn) { extra = fn; redraw(); },
      destroy: function () {
        destroyed = true;
        if (ro) ro.disconnect();
        window.removeEventListener("resize", schedule);
        if (raf) cancelAnimationFrame(raf);
        svg.remove();
      }
    };
  }

  return { create: create };
})();
