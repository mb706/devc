// The page for an unknown route.
(function () {
  "use strict";
  var esc = X.util.esc;
  X.router.register("not-found", {
    title: function () { return "Not found"; },
    render: function (el, r) {
      el.innerHTML = '<div class="page narrow"><h1>Nothing here</h1><p class="muted">No view is called <code>' + esc(r.view) +
        '</code>. Try the <a href="' + esc(X.router.href("overview")) + '">overview</a> or search with <kbd>/</kbd>.</p></div>';
    }
  });
})();
