// The shell: the top bar (navigation, search, build, settings, theme) and
// the start of the app. Loaded last, after every view has registered.
X.shell = (function () {
  "use strict";
  var U = X.util, esc = U.esc;
  var main = document.getElementById("main");

  if (!X.data) {
    main.innerHTML = '<div class="page narrow"><h1>No data</h1><p>The file <code>data/core.js</code> did not load. Build the site with <code>./dev explore</code> and open <code>explorer/dist/index.html</code>.</p></div>';
    return { openSettings: function () {} };
  }
  var D = X.data, build = D.core.build;
  var module = build.module || "";
  X.appName = (U.lastElem(module) || "code") + " explorer";

  var NAV = [
    ["overview", "Overview"], ["runtime", "Runtime"], ["packages", "Packages"], ["concerns", "Concerns"],
    ["flows", "Flows"], ["interfaces", "Interfaces"], ["state", "State"], ["glossary", "Glossary"],
    ["code", "Code"], ["docs", "Documents"]
  ];

  function topbar() {
    var bar = document.getElementById("topbar");
    var commit = (build.commit || "").slice(0, 7);
    var when = build.time ? new Date(build.time) : null;
    var whenText = when && !isNaN(when) ? when.toLocaleDateString(undefined, { day: "numeric", month: "short" }) + " " +
      when.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" }) : "";
    var title = "Commit " + (build.commit || "unknown") + (build.dirty ? " with uncommitted changes" : "") +
      "\nBuilt " + (build.time || "") + " in " + (build.seconds || 0).toFixed(1) + " s with " + (build.goVersion || "") +
      (build.typeErrors && build.typeErrors.length ? "\n" + build.typeErrors.length + " type errors" : "");
    bar.innerHTML =
      '<a class="brand" href="#/overview" title="' + esc(module) + '"><span class="brand-mark"></span><span class="mono">' + esc(U.lastElem(module) || "code") + '</span><span class="brand-sub">explorer</span></a>' +
      '<nav class="nav">' + NAV.map(function (n) { return '<a href="#/' + n[0] + '" data-nav="' + n[0] + '">' + esc(n[1]) + "</a>"; }).join("") + "</nav>" +
      '<button class="search-btn" type="button" title="Search (/ or Ctrl+K)">' + U.icon("search") + "<span>Search</span><kbd>/</kbd></button>" +
      '<span class="build" title="' + esc(title) + '"><span class="mono">' + esc(commit || "no commit") + "</span>" +
      (build.dirty ? '<span class="dirty" title="uncommitted changes">modified</span>' : "") +
      (whenText ? '<span class="when">' + esc(whenText) + "</span>" : "") + "</span>" +
      '<button class="btn ghost icon" data-a="theme" title="Light or dark">' + U.icon("moon") + "</button>" +
      '<button class="btn ghost icon" data-a="settings" title="Settings">' + U.icon("gear") + "</button>";
    bar.querySelector(".search-btn").onclick = function () { X.search.open(null, "names"); };
    bar.querySelector('[data-a="settings"]').onclick = function (ev) { ev.stopPropagation(); openSettings(); };
    var themeBtn = bar.querySelector('[data-a="theme"]');
    function themeIcon() { themeBtn.innerHTML = U.icon(document.documentElement.dataset.theme === "dark" ? "sun" : "moon"); }
    themeBtn.onclick = function () {
      X.settings.set("theme", document.documentElement.dataset.theme === "dark" ? "light" : "dark");
      themeIcon();
    };
    X.settings.on("theme", themeIcon);
    themeIcon();
  }

  function markNav(r) {
    var v = X.router.views[r.view];
    var name = (v && v.nav) || r.view;
    document.querySelectorAll("#topbar [data-nav]").forEach(function (a) {
      a.classList.toggle("on", a.getAttribute("data-nav") === name);
    });
  }

  // ----- settings

  var pop = null;
  function openSettings() {
    if (pop) { closeSettings(); return; }
    var s = X.settings;
    pop = U.h("div.popover.settings");
    var ed = s.get("editor");
    pop.innerHTML =
      '<h3>Settings</h3>' +
      '<div class="set-row"><span class="set-l">Theme</span><div class="seg" data-s="theme">' +
      ["system", "light", "dark"].map(function (t) { return '<button data-v="' + t + '"' + (s.get("theme") === t ? ' class="on"' : "") + ">" + t[0].toUpperCase() + t.slice(1) + "</button>"; }).join("") +
      "</div></div>" +
      '<div class="set-row"><span class="set-l">Tests</span><label class="check"><input type="checkbox" data-s="tests"' + (s.get("tests") ? " checked" : "") + "> show test files, symbols and uses</label></div>" +
      '<div class="set-row"><span class="set-l">Open in editor</span><select data-s="editor">' +
      '<option value="">None</option>' +
      Object.keys(s.presets).map(function (k) { return '<option value="' + k + '"' + (ed === k ? " selected" : "") + ">" + esc(s.presets[k].title) + "</option>"; }).join("") +
      '<option value="custom"' + (ed === "custom" ? " selected" : "") + ">Custom…</option></select></div>" +
      '<div class="set-row' + (ed === "custom" ? "" : " hidden") + '" data-row="tpl"><span class="set-l">Link</span><input type="text" data-s="editorTemplate" spellcheck="false" placeholder="scheme://file/{path}:{line}" value="' + esc(s.get("editorTemplate")) + '"></div>' +
      '<p class="small muted">{path} is the absolute path (the repository is <code>' + esc(build.root || "?") + "</code>), {line} the line, {file} the path in the repository. Copying <i>path:line</i> always works.</p>" +
      '<div class="set-row"><span class="set-l">Inspector</span><button class="btn" data-s="resetWidth">Reset the width</button></div>';
    document.getElementById("layer-popups").appendChild(pop);
    var btn = document.querySelector('#topbar [data-a="settings"]').getBoundingClientRect();
    pop.style.top = (btn.bottom + 6) + "px";
    pop.style.right = Math.max(8, window.innerWidth - btn.right) + "px";
    pop.addEventListener("click", function (ev) {
      var b = ev.target.closest('[data-s="theme"] [data-v]');
      if (b) {
        s.set("theme", b.getAttribute("data-v"));
        pop.querySelectorAll('[data-s="theme"] [data-v]').forEach(function (x) { x.classList.toggle("on", x === b); });
      }
      if (ev.target.closest('[data-s="resetWidth"]')) { s.set("inspectorWidth", 520); var i = document.getElementById("inspector"); if (i) i.style.width = "520px"; }
    });
    pop.addEventListener("change", function (ev) {
      var k = ev.target.getAttribute("data-s");
      if (k === "tests") { s.set("tests", ev.target.checked); rerender(); }
      if (k === "editor") { s.set("editor", ev.target.value); pop.querySelector('[data-row="tpl"]').classList.toggle("hidden", ev.target.value !== "custom"); }
      if (k === "editorTemplate") s.set("editorTemplate", ev.target.value.trim());
    });
    setTimeout(function () { document.addEventListener("mousedown", outside, true); }, 0);
  }
  function outside(ev) { if (pop && !pop.contains(ev.target) && !ev.target.closest('[data-a="settings"]')) closeSettings(); }
  function closeSettings() {
    if (pop) pop.remove();
    pop = null;
    document.removeEventListener("mousedown", outside, true);
  }
  document.addEventListener("keydown", function (ev) { if (ev.key === "Escape" && pop) closeSettings(); });

  // rerender mounts the current view again (after a setting changed).
  function rerender() { X.router.remount(); }

  topbar();
  X.router.onChange(markNav);
  X.router.defaultView = "overview";
  X.router.start();
  document.documentElement.dataset.app = "started";
  X.startedAt = performance.now();
  return { openSettings: openSettings, rerender: rerender };
})();
