// The Glossary (#/glossary?sel=<id>&q=<filter>): the terms in alphabetical
// order with a filter, each with its other names, text, references and
// related terms. Also the term's hover card (a [[term:…]] anywhere shows
// the definition) and its inspector view, with where the model uses it.
(function () {
  "use strict";
  var U = X.util, D = X.data, M = D.model, R = X.refs, esc = U.esc;

  function sorted() { return U.sortBy((M.terms || []).slice(), function (t) { return t.title.toLowerCase(); }); }
  function textOf(t) { return [t.title, t.id].concat(t.aka || []).concat([X.md.plainText(t.text || "")]).join(" ").toLowerCase(); }

  function termHTML(t, sel) {
    return '<article class="gl-t' + (t.id === sel ? " sel" : "") + '" data-term="' + esc(t.id) + '" data-text="' + esc(textOf(t)) + '">' +
      '<header class="gl-h"><h2><a href="' + esc(X.router.href("glossary", "", { sel: t.id })) + '" data-ref="term:' + esc(t.id) + '" data-nocard>' + esc(t.title) + "</a></h2>" +
      ((t.aka || []).length ? '<span class="gl-aka">also ' + t.aka.map(function (a) { return "<i>" + esc(a) + "</i>"; }).join(", ") + "</span>" : "") + "</header>" +
      '<div class="md gl-text">' + X.md.render(t.text || "", {}) + "</div>" +
      ((t.refs || []).length ? '<div class="gl-row"><span class="muted small">in the code</span>' + t.refs.map(function (r) { return R.chip(r, { cls: "small" }); }).join("") + "</div>" : "") +
      ((t.see || []).length ? '<div class="gl-row"><span class="muted small">see</span>' + t.see.map(function (s) { return R.chip("term:" + s, { cls: "small" }); }).join("") + "</div>" : "") +
      "</article>";
  }

  function render(el, r) {
    var terms = sorted(), sel = r.params.sel || "";
    var letters = [];
    terms.forEach(function (t) { var l = t.title[0].toUpperCase(); if (letters.indexOf(l) < 0) letters.push(l); });
    var h = '<div class="page gl"><h1>Glossary</h1><p class="lead">The project\'s vocabulary. A term named anywhere in the explorer shows its definition on hover.</p>' +
      '<div class="gl-bar"><input type="search" placeholder="Filter ' + terms.length + ' terms…" value="' + esc(r.params.q || "") + '" spellcheck="false">' +
      '<nav class="gl-letters">' + letters.map(function (l) { return '<a href="#" data-letter="' + esc(l) + '">' + esc(l) + "</a>"; }).join("") + '</nav><span class="muted small gl-n"></span></div>';
    var cur = "";
    h += '<div class="gl-list">';
    terms.forEach(function (t) {
      var l = t.title[0].toUpperCase();
      if (l !== cur) { cur = l; h += '<div class="gl-letter" data-l="' + esc(l) + '">' + esc(l) + "</div>"; }
      h += termHTML(t, sel);
    });
    h += "</div>" + (terms.length ? "" : '<p class="muted">The model has no terms yet.</p>') + "</div>";
    el.innerHTML = h;
    var input = el.querySelector(".gl-bar input");
    function filter(q) {
      var toks = q.toLowerCase().split(/\s+/).filter(Boolean), n = 0;
      el.querySelectorAll(".gl-t").forEach(function (a) {
        var on = toks.every(function (k) { return a.getAttribute("data-text").indexOf(k) >= 0; });
        a.classList.toggle("hidden", !on);
        if (on) n++;
      });
      el.querySelectorAll(".gl-letter").forEach(function (L) {
        var next = L.nextElementSibling, any = false;
        while (next && !next.classList.contains("gl-letter")) { if (!next.classList.contains("hidden")) any = true; next = next.nextElementSibling; }
        L.classList.toggle("hidden", !any);
      });
      el.querySelector(".gl-n").textContent = toks.length ? n + " of " + terms.length : "";
    }
    input.addEventListener("input", U.debounce(function () { filter(input.value); X.router.setParams({ q: input.value || null }); }, 120));
    el.querySelector(".gl-letters").addEventListener("click", function (ev) {
      var a = ev.target.closest("[data-letter]");
      if (!a) return;
      ev.preventDefault();
      var L = el.querySelector('.gl-letter[data-l="' + a.getAttribute("data-letter") + '"]');
      if (L) L.scrollIntoView({ block: "start" });
    });
    if (r.params.q) filter(r.params.q);
    scrollToSel(el, sel);
  }
  function scrollToSel(el, sel) {
    el.querySelectorAll(".gl-t.sel").forEach(function (a) { a.classList.remove("sel"); });
    if (!sel) return;
    var a = el.querySelector('.gl-t[data-term="' + (window.CSS && CSS.escape ? CSS.escape(sel) : sel) + '"]');
    if (a) { a.classList.add("sel"); a.scrollIntoView({ block: "center" }); }
  }

  var mounted = null;
  X.router.register("glossary", {
    nav: "glossary",
    title: function () { return "Glossary"; },
    render: function (el, r) { mounted = el; render(el, r); },
    update: function (r, prev) {
      if (!mounted) return false;
      if ((r.params.sel || "") !== ((prev && prev.params.sel) || "")) scrollToSel(mounted, r.params.sel || "");
      return true;
    },
    unmount: function () { mounted = null; }
  });

  R.define("term", {
    card: function (info) {
      var t = info.obj;
      return ((t.aka || []).length ? '<div class="hc-row muted">also ' + esc(t.aka.join(", ")) + "</div>" : "") +
        '<div class="hc-doc">' + X.md.cardText(t.text) + "</div>" +
        ((t.see || []).length ? '<div class="hc-row"><span class="muted">see</span> ' + t.see.map(function (s) { return R.chip("term:" + s, { cls: "small" }); }).join("") + "</div>" : "");
    }
  });
  X.inspector.register("term", function (info, body) {
    var t = info.obj;
    var h = ((t.aka || []).length ? '<p class="muted small">also ' + t.aka.map(function (a) { return "<i>" + esc(a) + "</i>"; }).join(", ") + "</p>" : "") +
      '<div class="md ins-lead">' + X.md.render(t.text || "", {}) + "</div>" +
      ((t.refs || []).length ? '<section class="sp-sec"><h3 class="sp-h">In the code</h3><div class="chips">' + t.refs.map(function (r) { return R.chip(r); }).join("") + "</div></section>" : "") +
      ((t.see || []).length ? '<section class="sp-sec"><h3 class="sp-h">See</h3><div class="chips">' + t.see.map(function (s) { return R.chip("term:" + s); }).join("") + "</div></section>" : "") +
      X.backlinks.section("term:" + t.id).replace("In the model", "Used in the model");
    body.appendChild(U.h("div.insp-pad.sp", { html: h }));
  });
  X.search.addSource({
    kind: "term", title: "Glossary", order: 4,
    items: function () {
      return (M.terms || []).map(function (t) { return { ref: "term:" + t.id, name: t.title, keys: [t.title, t.id].concat(t.aka || []), detail: X.md.plainText(U.firstSentence(t.text || "")), exported: true, weight: 50 }; });
    }
  });
})();
