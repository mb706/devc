// Routing with the URL fragment: #/<view>/<rest>?<key>=<value>&...
// Back, forward, reload and bookmarks work because the fragment is the
// whole state of the page (the inspector's target is the parameter "i").
// References and paths appear as they are; only characters that would
// break the fragment are escaped.
//
//   X.router.register(name, view)   view: {title(route), render(el, route),
//                                   update(route, prev) -> true if handled
//                                   in place, unmount(), nav: name of the
//                                   navigation entry to light}
//   X.router.href(view, rest, params) -> "#/…"
//   X.router.go(href)               navigate (a new history entry)
//   X.router.replace(href)          navigate without a history entry
//   X.router.setParams(obj, push)   change parameters of the current route
//                                   (a value of null removes it)
//   X.router.current                {view, rest, params}
X.router = (function () {
  "use strict";
  var views = {}, current = null, mounted = null, mainKey = "";
  var listeners = [];

  function enc(s) {
    return String(s).replace(/[^A-Za-z0-9\-_.~\/:@,!*'()$;=]/g, function (c) {
      return encodeURIComponent(c);
    }).replace(/=/g, "%3D");
  }
  function dec(s) {
    try { return decodeURIComponent(s); } catch (e) { return s; }
  }

  function href(view, rest, params) {
    var h = "#/" + view + (rest ? "/" + enc(rest).replace(/%2F/gi, "/") : "");
    var keys = params ? Object.keys(params).filter(function (k) { return params[k] != null && params[k] !== ""; }) : [];
    if (keys.length) h += "?" + keys.map(function (k) { return k + "=" + enc(params[k]); }).join("&");
    return h;
  }

  function parse(hash) {
    var h = (hash || "").replace(/^#\/?/, "");
    var q = h.indexOf("?"), path = q < 0 ? h : h.slice(0, q), query = q < 0 ? "" : h.slice(q + 1);
    var slash = path.indexOf("/");
    var view = dec(slash < 0 ? path : path.slice(0, slash));
    var rest = slash < 0 ? "" : dec(path.slice(slash + 1));
    var params = {};
    query.split("&").forEach(function (kv) {
      if (!kv) return;
      var i = kv.indexOf("=");
      if (i < 0) params[dec(kv)] = "";
      else params[dec(kv.slice(0, i))] = dec(kv.slice(i + 1));
    });
    return { view: view || "", rest: rest, params: params };
  }

  function keyOf(r) {
    var p = {};
    Object.keys(r.params).sort().forEach(function (k) { if (k !== "i") p[k] = r.params[k]; });
    return r.view + "/" + r.rest + "?" + JSON.stringify(p);
  }

  function register(name, view) { views[name] = view; }

  function handle() {
    var r = parse(location.hash);
    if (!r.view || !views[r.view]) {
      if (!r.view) { replace(href(X.router.defaultView || "overview")); return; }
    }
    var prev = current;
    current = r;
    var k = keyOf(r);
    if (k !== mainKey) {
      mainKey = k;
      var v = views[r.view] || views["not-found"];
      var handled = false;
      if (mounted && mounted.view === v && v.update) {
        try { handled = v.update(r, prev) === true; } catch (e) { console.error(e); handled = false; }
      }
      if (!handled) mount(v, r);
      var title = (v.title ? v.title(r) : r.view);
      document.title = (title ? title + " · " : "") + (X.appName || "Explorer");
    }
    if (X.inspector) X.inspector.sync(r.params.i || "");
    listeners.forEach(function (f) { f(r, prev); });
  }

  function mount(v, r) {
    if (mounted && mounted.view.unmount) { try { mounted.view.unmount(); } catch (e) { console.error(e); } }
    X.hovercard && X.hovercard.hide();
    var main = document.getElementById("main");
    main.innerHTML = "";
    var el = document.createElement("div");
    el.className = "view view-" + r.view;
    main.appendChild(el);
    mounted = { view: v, el: el };
    try { v.render(el, r); } catch (e) {
      console.error(e);
      el.innerHTML = '<div class="error">This view failed: ' + X.util.esc(e.message) + "</div>";
    }
  }

  function go(h) {
    if (h === location.hash) return;
    location.hash = h;
  }
  function replace(h) {
    try { history.replaceState(history.state, "", h); } catch (e) { location.replace(h); return; }
    handle();
  }
  function setParams(obj, push) {
    var r = current || parse(location.hash);
    var p = {};
    Object.keys(r.params).forEach(function (k) { p[k] = r.params[k]; });
    Object.keys(obj).forEach(function (k) { if (obj[k] == null || obj[k] === "") delete p[k]; else p[k] = obj[k]; });
    var h = href(r.view, r.rest, p);
    if (push) go(h); else replace(h);
  }
  // withInspector keeps the current inspector target on a new route.
  function hrefKeep(view, rest, params) {
    var p = {};
    Object.keys(params || {}).forEach(function (k) { p[k] = params[k]; });
    if (current && current.params.i && p.i === undefined) p.i = current.params.i;
    return href(view, rest, p);
  }

  window.addEventListener("hashchange", handle);

  return {
    remount: function () { if (current) mount(views[current.view] || views["not-found"], current); },
    register: register, views: views, href: href, hrefKeep: hrefKeep, parse: parse, go: go, replace: replace,
    setParams: setParams, start: handle, onChange: function (f) { listeners.push(f); },
    get current() { return current; }
  };
})();
