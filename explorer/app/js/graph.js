// Concern diagrams of kind "graph": nodes on the grid the model gives, as
// HTML boxes (zone colours, a reference on click), and edges with labels
// through the connector layer. Edges are routed orthogonally through the
// gaps between the boxes (the connector layer's grid router), labels sit
// where they cover no box, edges between the same pair run side by side,
// and an edge from a node to itself is a loop. Hovering a node lights its
// edges.
//
//   var g = X.graph.render(container, diagram, {concern: id})
//   g.destroy()
X.graph = (function () {
  "use strict";
  var U = X.util, D = X.data, R = X.refs, esc = U.esc;

  function trustOf(zoneId) { var z = zoneId ? D.zone(zoneId) : null; return z ? z.trust : ""; }

  function render(container, d, opts) {
    opts = opts || {};
    var nodes = d.nodes || [], edges = d.edges || [];
    var cols = Math.max(1, Math.max.apply(null, nodes.map(function (n) { return n.col || 1; }).concat([1])));
    var rows = Math.max(1, Math.max.apply(null, nodes.map(function (n) { return n.row || 1; }).concat([1])));
    var byId = new Map();
    nodes.forEach(function (n) { byId.set(n.id, n); });

    // Gaps make room for the labels that run between neighbours.
    // Dense graphs (many edges per node) need more room between the boxes.
    var dense = Math.max(0, Math.min(1, (edges.length / Math.max(1, nodes.length) - 1) / 1.2));
    var colGap = Math.round(64 + 28 * dense), rowGap = Math.round(60 + 30 * dense);
    edges.forEach(function (e) {
      var a = byId.get(e.from), b = byId.get(e.to);
      if (!a || !b || !e.label) return;
      var w = Math.min(e.label.length, 24) * 6.3 + 22;
      if (a.row === b.row && a.col !== b.col) colGap = Math.max(colGap, Math.min(150, w / Math.max(1, Math.abs(a.col - b.col) * 0.9)));
      if (a.col === b.col && a.row !== b.row) rowGap = Math.max(rowGap, e.label.length > 24 ? 76 : 62);
    });

    var fig = U.h("figure.gd-fig");
    if (opts.concern) fig.setAttribute("data-diagram", opts.concern + "/" + d.id);
    var scroll = U.h("div.gd-scroll");
    var grid = U.h("div.gd", { style: "grid-template-columns:repeat(" + cols + ",minmax(104px,190px));grid-template-rows:repeat(" + rows + ",auto);column-gap:" + Math.round(colGap) + "px;row-gap:" + rowGap + "px" });
    var els = new Map();
    nodes.forEach(function (n) {
      var t = trustOf(n.zone);
      var el = U.h("div.gd-node" + (t ? ".z-" + t + ".zoned" : "") + (n.ref ? ".has-ref" : ""), {
        style: "grid-column:" + (n.col || 1) + ";grid-row:" + (n.row || 1),
        "data-node": n.id
      });
      if (n.ref) {
        el.setAttribute("data-ref", n.ref);
        el.setAttribute("tabindex", "0");
      }
      el.innerHTML = '<span class="gd-label">' + esc(n.label || n.id) + "</span>" +
        (n.ref ? '<span class="gd-ref mono">' + esc(X.model.symbolName(n.ref)) + "</span>" : "");
      grid.appendChild(el);
      els.set(n.id, el);
    });
    scroll.appendChild(grid);
    fig.appendChild(scroll);
    fig.appendChild(U.h("figcaption", { html: U.icon("diagram") + "<span>" + esc(d.title || d.id) + '</span><span class="muted">' +
      U.plural(nodes.length, "node") + " · " + U.plural(edges.length, "edge") + (nodesWithRef(nodes) ? " · boxes with a name under them lead to code" : "") + "</span>" }));
    container.appendChild(fig);
    // Narrow the column gaps (not below 48 px) before the diagram scrolls.
    var avail = scroll.clientWidth - 44;
    if (cols > 1 && avail > 0 && cols * 104 + (cols - 1) * colGap > avail) {
      colGap = Math.max(48, Math.floor((avail - cols * 104) / (cols - 1)));
      grid.style.columnGap = colGap + "px";
    }

    var layer = X.connectors.create(grid, {
      labels: "html",
      router: "grid",
      obstacles: function () { return Array.from(els.values()); }
    });
    var list = edges.map(function (e) {
      return {
        from: els.get(e.from), to: els.get(e.to), arrow: true, label: e.label || "", ref: e.ref || "",
        hit: !!e.ref, avoid: true, cls: "gd-e" + (e.ref ? " has-ref" : ""), data: e
      };
    }).filter(function (e) { return e.from && e.to; });
    layer.set(list);

    var hover = null;
    function focus(id) {
      if (id === hover) return;
      hover = id;
      grid.classList.toggle("focus", !!id);
      els.forEach(function (el, nid) {
        var on = !!id && (nid === id || list.some(function (e) { return (e.data.from === id && e.data.to === nid) || (e.data.to === id && e.data.from === nid); }));
        el.classList.toggle("lit", on);
      });
      layer.setClass(function (e) { return !id ? "" : (e.data.from === id || e.data.to === id) ? "hl" : "dim"; });
    }
    grid.addEventListener("mouseover", function (ev) {
      var n = ev.target.closest(".gd-node");
      focus(n ? n.getAttribute("data-node") : null);
    });
    grid.addEventListener("mouseleave", function () { focus(null); });

    return { el: fig, layer: layer, destroy: function () { layer.destroy(); } };
  }
  function nodesWithRef(nodes) { return nodes.some(function (n) { return n.ref; }); }

  return { render: render };
})();
