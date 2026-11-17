// Settings kept in localStorage (through X.util.load/save, which never
// throw): theme, whether tests are shown, the inspector's width, the
// "open in editor" link and which channels the runtime map shows at rest. X.settings.on(key, fn) runs fn after a change.
X.settings = (function () {
  "use strict";
  var U = X.util;

  var defaults = {
    theme: "system",        // system | light | dark
    tests: true,            // show test files, test symbols and test uses
    inspectorWidth: 520,
    editor: "",             // "" (none), "vscode", "cursor" or "custom"
    editorTemplate: "",     // used when editor is "custom"
    rtChannels: "focus"     // runtime map: channels shown at rest (focus | bnd | all)
  };
  var presets = {
    vscode: { title: "VS Code", template: "vscode://file/{path}:{line}" },
    cursor: { title: "Cursor", template: "cursor://file/{path}:{line}" }
  };
  var values = {};
  Object.keys(defaults).forEach(function (k) { values[k] = U.load(k, defaults[k]); });
  var listeners = {};

  function get(k) { return values[k]; }
  function set(k, v) {
    values[k] = v;
    U.save(k, v);
    (listeners[k] || []).forEach(function (f) { f(v); });
  }
  function on(k, f) { (listeners[k] = listeners[k] || []).push(f); }

  function applyTheme() {
    var t = values.theme;
    var dark = t === "dark" || (t !== "light" && window.matchMedia && matchMedia("(prefers-color-scheme: dark)").matches);
    document.documentElement.dataset.theme = dark ? "dark" : "light";
  }
  on("theme", applyTheme);
  if (window.matchMedia) {
    var mq = matchMedia("(prefers-color-scheme: dark)");
    var f = function () { if (values.theme === "system") applyTheme(); };
    if (mq.addEventListener) mq.addEventListener("change", f); else if (mq.addListener) mq.addListener(f);
  }

  // editorURL is the "open in editor" link for a file and line, or "" when
  // none is configured. The repository's absolute path comes from the build.
  function editorTemplate() {
    var e = values.editor;
    if (!e) return "";
    if (e === "custom") return values.editorTemplate || "";
    return presets[e] ? presets[e].template : "";
  }
  function absPath(file) {
    var root = (X.data && X.data.core.build.root) || "";
    return root ? root.replace(/\/$/, "") + "/" + file : file;
  }
  function editorURL(file, line) {
    var t = editorTemplate();
    if (!t) return "";
    return t.replace("{path}", absPath(file)).replace("{line}", String(line || 1)).replace("{file}", file);
  }

  return { get: get, set: set, on: on, presets: presets, editorURL: editorURL, absPath: absPath, applyTheme: applyTheme };
})();
