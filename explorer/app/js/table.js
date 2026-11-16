// A table with a filter box and sortable columns, rendered as HTML strings
// so that a few hundred rows stay fast. Optionally a tree: rows in tree
// order with a depth; filtering keeps the ancestors of matching rows, and
// sorting by a column other than the first flattens the tree.
//
//   var t = X.table.create(container, {
//     columns: [{id, title, html(row) -> HTML, sort(row) -> value,
//                cls, width, align}],
//     rows: [...],
//     key(row) -> string,               identity (selection, data-key)
//     text(row) -> string,              what the filter searches
//     query: "", onQuery(q),            initial filter; called on change
//     sort: {col: id, dir: 1|-1},       initial sort (none: data order)
//     tree: {depth(row), parent(row) -> key or ""},
//     selected: key, onSelect(row, ev),
//     placeholder: "Filter…", empty: "Nothing matches.",
//     toolbar: [elements]               extra controls beside the filter
//   })
//   t.setQuery(q), t.select(key, scroll), t.setRows(rows), t.el, t.render()
X.table = (function () {
  "use strict";
  var U = X.util, esc = U.esc;

  function create(container, o) {
    var state = { q: o.query || "", sort: o.sort || null, sel: o.selected || null, rows: o.rows || [] };
    var el = U.h("div.xt");
    var bar = U.h("div.xt-bar");
    var input = U.h("input", { type: "search", placeholder: o.placeholder || "Filter…", value: state.q, spellcheck: "false" });
    var count = U.h("span.xt-count.muted");
    bar.appendChild(input);
    (o.toolbar || []).forEach(function (t) { bar.appendChild(t); });
    bar.appendChild(count);
    var scroller = U.h("div.xt-scroll");
    var table = U.h("table.xt-table");
    scroller.appendChild(table);
    el.appendChild(bar);
    el.appendChild(scroller);
    container.appendChild(el);

    var texts = new Map();
    function textOf(r) {
      if (!texts.has(r)) texts.set(r, (o.text ? o.text(r) : "").toLowerCase());
      return texts.get(r);
    }

    function visibleRows() {
      var toks = state.q.toLowerCase().split(/\s+/).filter(Boolean);
      var rows = state.rows;
      var match = function (r) { var t = textOf(r); return toks.every(function (k) { return t.indexOf(k) >= 0; }); };
      var sorted = state.sort && !(o.tree && state.sort.col === o.columns[0].id && state.sort.dir === 1);
      if (o.tree && !sorted) {
        if (!toks.length) return { rows: rows, tree: true };
        var keep = new Set(), byKey = new Map();
        rows.forEach(function (r) { byKey.set(o.key(r), r); });
        rows.forEach(function (r) {
          if (!match(r)) return;
          var k = o.key(r);
          while (k && !keep.has(k)) { keep.add(k); var pr = byKey.get(k); k = pr ? o.tree.parent(pr) : ""; }
        });
        return { rows: rows.filter(function (r) { return keep.has(o.key(r)); }), tree: true, matched: rows.filter(match).length };
      }
      var out = toks.length ? rows.filter(match) : rows.slice();
      if (sorted) {
        var col = o.columns.filter(function (c) { return c.id === state.sort.col; })[0];
        if (col) {
          var fn = col.sort || function (r) { return String(col.html(r)).replace(/<[^>]+>/g, "").toLowerCase(); };
          var dir = state.sort.dir;
          out = U.sortBy(out, fn);
          if (dir < 0) out.reverse();
        }
      }
      return { rows: out, tree: false };
    }

    function render() {
      var v = visibleRows();
      var h = "<thead><tr>" + o.columns.map(function (c) {
        var s = state.sort && state.sort.col === c.id ? (state.sort.dir > 0 ? " ▲" : " ▼") : "";
        return '<th data-col="' + esc(c.id) + '"' + (c.width ? ' style="width:' + c.width + '"' : "") +
          (c.cls ? ' class="' + c.cls + '"' : "") + ">" + esc(c.title) + '<span class="xt-sort">' + s + "</span></th>";
      }).join("") + "</tr></thead><tbody>";
      if (!v.rows.length) h += '<tr class="xt-empty"><td colspan="' + o.columns.length + '">' + esc(o.empty || "Nothing matches.") + "</td></tr>";
      var rowsHtml = v.rows.map(function (r) {
        var k = o.key ? o.key(r) : "";
        var cls = (k && k === state.sel ? "sel" : "") + (o.rowClass ? " " + (o.rowClass(r) || "") : "");
        return '<tr data-key="' + esc(k) + '"' + (cls.trim() ? ' class="' + cls.trim() + '"' : "") + ">" +
          o.columns.map(function (c, i) {
            var inner = c.html(r);
            if (i === 0 && v.tree && o.tree) inner = '<span class="xt-ind" style="width:' + (o.tree.depth(r) * 16) + 'px"></span>' + inner;
            return "<td" + (c.cls ? ' class="' + c.cls + '"' : "") + ">" + inner + "</td>";
          }).join("") + "</tr>";
      });
      table.innerHTML = h + rowsHtml.join("") + "</tbody>";
      var total = state.rows.length;
      var shown = v.matched != null ? v.matched : v.rows.length;
      count.textContent = shown === total ? U.fmt(total) + " rows" : U.fmt(shown) + " of " + U.fmt(total);
    }

    input.addEventListener("input", U.debounce(function () {
      state.q = input.value;
      render();
      if (o.onQuery) o.onQuery(state.q);
    }, 120));
    table.addEventListener("click", function (ev) {
      var th = ev.target.closest("th[data-col]");
      if (th) {
        var id = th.getAttribute("data-col");
        if (state.sort && state.sort.col === id) state.sort = state.sort.dir > 0 ? { col: id, dir: -1 } : null;
        else state.sort = { col: id, dir: 1 };
        render();
        return;
      }
      if (ev.defaultPrevented || U.isModifiedClick(ev)) return;
      var tr = ev.target.closest("tr[data-key]");
      var own = ev.target.closest("[data-row]");
      if (!tr || (!own && ev.target.closest("a, button, input, [data-ref]"))) return;
      if (own) ev.preventDefault();
      var k = tr.getAttribute("data-key");
      var row = state.rows.filter(function (r) { return o.key(r) === k; })[0];
      if (row) { select(k); if (o.onSelect) o.onSelect(row, ev); }
    });

    function select(k, scroll) {
      state.sel = k;
      Array.prototype.forEach.call(table.querySelectorAll("tr.sel"), function (tr) { tr.classList.remove("sel"); });
      var tr = k ? table.querySelector('tr[data-key="' + cssEsc(k) + '"]') : null;
      if (tr) {
        tr.classList.add("sel");
        if (scroll) tr.scrollIntoView({ block: "center" });
      }
    }

    render();
    return {
      el: el, input: input, render: render, select: select,
      setQuery: function (q) { state.q = q || ""; input.value = state.q; render(); },
      setRows: function (rows) { state.rows = rows; texts.clear(); render(); }
    };
  }

  function cssEsc(s) { return window.CSS && CSS.escape ? CSS.escape(s) : String(s).replace(/["\\]/g, "\\$&"); }

  return { create: create };
})();
