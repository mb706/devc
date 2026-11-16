// The build's data, indexed for lookup, and the loaders of the data files
// that arrive on demand (per-package chunks, documents). Every view reads
// facts through X.data; nothing else touches EXPLORER.
//
//   X.data.core                    the Core of schema.go
//   X.data.sym(ref), pkg(path), file(path), cmd(path), rpc(name),
//   route(key), event(name), config(key), env(name), document(path),
//   finding(id), workOrder(id), actor(id), zone(id), channel(id),
//   concern(id), flow(id), term(id), surface(id), layer(id), group(id)
//   X.data.children(ref)           methods, fields, interface methods
//   X.data.symbolsOfFile(path), symbolsOfPackage(path)
//   X.data.zonesOfActors(ids)      zone objects in model order
//   X.data.chunk(path) -> Promise<Chunk>; chunkOfPackage, chunkOfFile
//   X.data.source(path) -> Promise<SourceFile>
//   X.data.doc(path) -> Promise<DocText>
//   X.data.allChunks(onProgress) -> Promise (loads every chunk)
X.data = (function () {
  "use strict";
  var core = window.EXPLORER && EXPLORER.core;
  if (!core) return null;

  function index(arr, key) {
    var m = new Map();
    (arr || []).forEach(function (x) { m.set(x[key], x); });
    return m;
  }
  var M = core.model || {};
  var syms = index(core.symbols, "ref");
  var pkgs = index(core.packages, "path");
  var files = index(core.files, "path");
  var cmds = index(core.commands, "path");
  var rpcs = index(core.rpc, "name");
  var routes = index(core.routes, "key");
  var events = index(core.events, "name");
  var configs = index(core.config, "key");
  var envs = index(core.env, "name");
  // A document without headings arrives with null (a nil slice in Go).
  (core.documents || []).forEach(function (d) { if (!d.headings) d.headings = []; });
  var docs = index(core.documents, "path");
  var findings = index(core.findings, "id");
  var workOrders = index(core.workOrders, "id");
  var actors = index(M.actors, "id");
  var zones = index(M.zones, "id");
  var channels = index(M.channels, "id");
  var concerns = index(M.concerns, "id");
  var flows = index(M.flows, "id");
  var terms = index(M.terms, "id");
  var surfaces = index(M.surfaces, "id");
  var layers = index(M.layers, "id");
  var groups = index(M.groups, "id");

  // Members of a type (and fields of anonymous struct fields), by parent.
  var children = new Map(), byFile = new Map(), byPkg = new Map();
  core.symbols.forEach(function (s) {
    if (s.parent) push(children, s.parent, s);
    push(byFile, s.file, s);
    push(byPkg, s.package, s);
  });
  byFile.forEach(function (list) { list.sort(function (a, b) { return a.line - b.line || a.endLine - b.endLine; }); });
  function push(m, k, v) { var l = m.get(k); if (!l) m.set(k, l = []); l.push(v); }

  var implsOf = new Map(), ifacesOf = new Map();
  (core.implementations || []).forEach(function (im) {
    push(implsOf, im.interface, im);
    push(ifacesOf, im.type, im);
  });
  var configChildren = new Map();
  (core.config || []).forEach(function (c) { if (c.parent) push(configChildren, c.parent, c); });

  // The order of zones in the model gives a stable order to zone marks.
  var zoneOrder = new Map();
  (M.zones || []).forEach(function (z, i) { zoneOrder.set(z.id, i); });
  function zoneOfActor(id) { var a = actors.get(id); return a ? zones.get(a.zone) : null; }
  function zonesOfActors(ids) {
    var seen = new Map();
    (ids || []).forEach(function (id) { var z = zoneOfActor(id); if (z) seen.set(z.id, z); });
    return Array.from(seen.values()).sort(function (a, b) { return zoneOrder.get(a.id) - zoneOrder.get(b.id); });
  }
  // actorsOfPackage lists the actors with a share, largest first.
  function actorsOfPackage(p) {
    var a = (p && p.actors) || {};
    return Object.keys(a).filter(function (k) { return a[k] > 0; })
      .sort(function (x, y) { return a[y] - a[x] || (x < y ? -1 : 1); });
  }

  // ----- loading of data files

  // A data file is a script that calls explorerRegister, which resolves
  // the waiting promise. One load per file; a failed load may be retried.
  var pending = {};
  function chunkId(path) { return path.replace(/^data\/chunks\//, "").replace(/\.js$/, ""); }
  function loadScript(kind, key, path, store) {
    if (store[key]) return Promise.resolve(store[key]);
    var wk = kind + ":" + key;
    if (pending[wk]) return pending[wk];
    var p = new Promise(function (resolve, reject) {
      (EXPLORER.waiting[wk] = EXPLORER.waiting[wk] || []).push(resolve);
      var s = document.createElement("script");
      s.src = path;
      s.async = true;
      s.onerror = function () {
        delete pending[wk];
        delete EXPLORER.waiting[wk];
        s.remove();
        reject(new Error("could not load " + path + " (rebuild the site with ./dev explore)"));
      };
      document.head.appendChild(s);
    });
    pending[wk] = p;
    p.then(function () { delete pending[wk]; }, function () {});
    return p;
  }
  function chunk(path) { return loadScript("chunk", chunkId(path), path, EXPLORER.chunks); }
  function loadedChunk(path) { return path ? EXPLORER.chunks[chunkId(path)] || null : null; }
  function chunkOfPackage(p) {
    var pk = pkgs.get(p);
    return pk ? chunk(pk.chunk) : Promise.reject(new Error("no package " + p));
  }
  function chunkOfFile(f) {
    var fl = files.get(f);
    if (!fl) return Promise.reject(new Error("no file " + f));
    if (!fl.chunk) return Promise.reject(new Error(f + " is a binary file"));
    return chunk(fl.chunk);
  }
  var sourceCache = new Map();
  function source(f) {
    if (sourceCache.has(f)) return Promise.resolve(sourceCache.get(f));
    return chunkOfFile(f).then(function (c) {
      c.files.forEach(function (sf) { sourceCache.set(sf.path, sf); });
      var sf = sourceCache.get(f);
      if (!sf) throw new Error("no source for " + f);
      return sf;
    });
  }
  function loadedSource(f) {
    if (sourceCache.has(f)) return sourceCache.get(f);
    var fl = files.get(f), c = fl && loadedChunk(fl.chunk);
    if (!c) return null;
    c.files.forEach(function (sf) { sourceCache.set(sf.path, sf); });
    return sourceCache.get(f) || null;
  }
  function doc(path) {
    var d = docs.get(path);
    return d ? loadScript("doc", path, d.chunk, EXPLORER.docs) : Promise.reject(new Error("no document " + path));
  }
  function chunkPaths() {
    var s = new Set();
    core.packages.forEach(function (p) { if (p.chunk) s.add(p.chunk); });
    core.files.forEach(function (f) { if (f.chunk) s.add(f.chunk); });
    return Array.from(s).sort();
  }
  // allChunks loads every chunk, a few at a time, reporting progress.
  function allChunks(onProgress) {
    var paths = chunkPaths(), done = 0, i = 0;
    return new Promise(function (resolve) {
      var active = 0;
      function next() {
        while (active < 4 && i < paths.length) {
          active++;
          chunk(paths[i++]).catch(function () { return null; }).then(function () {
            active--; done++;
            if (onProgress) onProgress(done, paths.length);
            if (done === paths.length) resolve(); else next();
          });
        }
      }
      if (!paths.length) resolve(); else next();
    });
  }

  return {
    core: core,
    model: M,
    sym: function (r) { return syms.get(r); },
    pkg: function (p) { return pkgs.get(p); },
    file: function (p) { return files.get(p); },
    cmd: function (p) { return cmds.get(p); },
    rpc: function (n) { return rpcs.get(n); },
    route: function (k) { return routes.get(k); },
    event: function (n) { return events.get(n); },
    config: function (k) { return configs.get(k); },
    configChildren: function (k) { return configChildren.get(k) || []; },
    env: function (n) { return envs.get(n); },
    document: function (p) { return docs.get(p); },
    finding: function (id) { return findings.get(id); },
    workOrder: function (id) { return workOrders.get(id); },
    actor: function (id) { return actors.get(id); },
    zone: function (id) { return zones.get(id); },
    channel: function (id) { return channels.get(id); },
    concern: function (id) { return concerns.get(id); },
    flow: function (id) { return flows.get(id); },
    term: function (id) { return terms.get(id); },
    surface: function (id) { return surfaces.get(id); },
    layer: function (id) { return layers.get(id); },
    group: function (id) { return groups.get(id); },
    children: function (r) { return children.get(r) || []; },
    symbolsOfFile: function (p) { return byFile.get(p) || []; },
    symbolsOfPackage: function (p) { return byPkg.get(p) || []; },
    implementationsOf: function (iface) { return implsOf.get(iface) || []; },
    interfacesOf: function (type) { return ifacesOf.get(type) || []; },
    zoneOfActor: zoneOfActor,
    zonesOfActors: zonesOfActors,
    actorsOfPackage: actorsOfPackage,
    chunk: chunk,
    loadedChunk: loadedChunk,
    chunkOfPackage: chunkOfPackage,
    chunkOfFile: chunkOfFile,
    source: source,
    loadedSource: loadedSource,
    doc: doc,
    allChunks: allChunks,
    chunkPaths: chunkPaths
  };
})();
