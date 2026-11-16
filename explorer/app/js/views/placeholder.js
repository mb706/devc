// Placeholders for the views of the model (overview, runtime, concerns,
// flows, state, glossary, documents) until they are built, and the page
// for an unknown route. A later file registers the real view under the
// same name; X.router.register replaces the placeholder.
(function () {
  "use strict";
  var U = X.util, D = X.data, esc = U.esc;

  var PAGES = {
    overview: ["Overview", "The system in one picture, and a reading order."],
    runtime: ["Runtime", "Zones, actors and channels: what runs where, whom it trusts, what crosses each boundary and where it is checked."],
    concerns: ["Concerns", "One page per concern: explanation, diagrams, invariants with the code that enforces them, key symbols, flows, tests."],
    flows: ["Flows", "A sequence diagram beside the steps, with the code of the current step next to it."],
    state: ["State", "What is on disk and what is mounted into a box."],
    glossary: ["Glossary", "The project's vocabulary."],
    docs: ["Documents", "The spec, the guides and the development records."]
  };

  function stats() {
    var c = D.core.build.counts;
    var items = [
      [c.packages, "packages", X.router.href("packages")],
      [c.lines.code, "lines of code", X.router.href("packages", "size")],
      [c.testLines.code, "lines of tests", X.router.href("packages", "size")],
      [c.symbols, "declarations", null],
      [c.commands, "commands", X.router.href("interfaces", firstSurface("commands"))],
      [c.rpc, "RPC methods", X.router.href("interfaces", firstSurface("rpc"))],
      [c.routes, "routes", X.router.href("interfaces", firstSurface("http"))],
      [c.config, "configuration keys", X.router.href("interfaces", firstSurface("config"))]
    ];
    return '<div class="stats">' + items.map(function (it) {
      var inner = '<div class="stat-n num">' + U.fmt(it[0]) + '</div><div class="stat-l">' + esc(it[1]) + "</div>";
      return it[2] ? '<a class="stat" href="' + esc(it[2]) + '">' + inner + "</a>" : '<div class="stat">' + inner + "</div>";
    }).join("") + "</div>";
  }
  function firstSurface(kind) {
    var s = (D.model.surfaces || []).filter(function (x) { return x.kind === kind; })[0];
    return s ? s.id : "";
  }

  Object.keys(PAGES).forEach(function (name) {
    X.router.register(name, {
      nav: name,
      title: function () { return PAGES[name][0]; },
      render: function (el) {
        el.innerHTML = '<div class="page narrow placeholder">' +
          "<h1>" + esc(PAGES[name][0]) + "</h1>" +
          '<p class="lead">' + esc(PAGES[name][1]) + "</p>" +
          '<p class="muted">This view is not built yet. Meanwhile: the <a href="' + esc(X.router.href("packages")) + '">package map</a>, ' +
          'the <a href="' + esc(X.router.href("code")) + '">code</a>, the <a href="' + esc(X.router.href("interfaces")) + '">tables of entry points</a>, ' +
          "or search with <kbd>/</kbd>.</p>" +
          (name === "overview" ? stats() : "") + "</div>";
      }
    });
  });

  X.router.register("not-found", {
    title: function () { return "Not found"; },
    render: function (el, r) {
      el.innerHTML = '<div class="page narrow"><h1>Nothing here</h1><p class="muted">No view is called <code>' + esc(r.view) +
        '</code>. Try the <a href="' + esc(X.router.href("overview")) + '">overview</a> or search with <kbd>/</kbd>.</p></div>';
    }
  });
})();
