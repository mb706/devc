// Questions about the model that several views ask: the numbering of a
// flow's steps, which channel a message travels on and whether it crosses
// a trust boundary, who takes part in what, the reading order, and the
// zone colours of writers in the layout. Computed from the data only.
//
//   X.model.steps(flow)          -> [{i, no, step, section}] (no: 1-based
//                                   among steps that are not sections)
//   X.model.stepAt(flow, no)     -> that entry; X.model.noOf(flow, i)
//   X.model.channelOf(step)      -> the channel a message uses, or null
//   X.model.crosses(step)        -> true if a message crosses a trust
//                                   boundary (its channel says so; without a
//                                   channel, its ends' zones differ in trust)
//   X.model.flowsOf(kind, id)    -> flows that involve an actor, channel or
//                                   concern; actorsOf/channelsOf likewise
//   X.model.concernsOfActor(id), X.model.readingOrder()
//   X.model.writerTrusts(writer) -> trust values for a layout writer
//   X.model.refsIn(text)         -> the [[references]] of a Markdown text
//   X.model.symbolName(ref)      -> a short name for a step's code
X.model = (function () {
  "use strict";
  var D = X.data, M = D.model, U = X.util;

  var stepCache = new Map();
  function steps(flow) {
    if (stepCache.has(flow.id)) return stepCache.get(flow.id);
    var out = [], no = 0, section = "";
    (flow.steps || []).forEach(function (s, i) {
      if (s.section) { section = s.section; return; }
      no++;
      out.push({ i: i, no: no, step: s, section: section });
    });
    stepCache.set(flow.id, out);
    return out;
  }
  function stepAt(flow, no) { var l = steps(flow); return l[Math.max(1, Math.min(l.length, no)) - 1] || null; }
  function noOf(flow, i) {
    var l = steps(flow);
    for (var k = 0; k < l.length; k++) if (l[k].i === i) return l[k].no;
    return 0;
  }

  var byPair = new Map();
  (M.channels || []).forEach(function (c) {
    var k = c.from + "\u0000" + c.to;
    if (!byPair.has(k)) byPair.set(k, []);
    byPair.get(k).push(c);
  });
  // channelOf: a message travels on a channel from its sender to its
  // receiver; a reply on the channel of the call it answers.
  function channelOf(s) {
    if (!s.from || !s.to) return null;
    var fwd = byPair.get(s.from + "\u0000" + s.to) || [], rev = byPair.get(s.to + "\u0000" + s.from) || [];
    if (s.via === "reply") return rev[0] || fwd[0] || null;
    return fwd[0] || rev[0] || null;
  }
  function trustOfActor(id) { var z = D.zoneOfActor(id); return z ? z.trust : ""; }
  function crosses(s) {
    if (!s.from || !s.to || s.from === s.to) return false;
    var c = channelOf(s);
    if (c) return !!c.boundary;
    var a = trustOfActor(s.from), b = trustOfActor(s.to);
    return !!a && !!b && a !== b;
  }

  // ----- who takes part in what

  function flowActors(f) {
    var set = new Set(f.actors || []);
    (f.steps || []).forEach(function (s) { [s.at, s.from, s.to].forEach(function (a) { if (a) set.add(a); }); });
    return Array.from(set);
  }
  function flowChannels(f) {
    var set = new Set();
    (f.steps || []).forEach(function (s) { var c = channelOf(s); if (c) set.add(c.id); });
    return Array.from(set);
  }
  function flowsOf(kind, id) {
    return (M.flows || []).filter(function (f) {
      if (kind === "actor") return flowActors(f).indexOf(id) >= 0;
      if (kind === "channel") return flowChannels(f).indexOf(id) >= 0;
      if (kind === "concern") return f.concern === id;
      return false;
    });
  }
  function channelsOf(kind, id) {
    return (M.channels || []).filter(function (c) {
      if (kind === "actor") return c.from === id || c.to === id;
      if (kind === "concern") return (c.concerns || []).indexOf(id) >= 0 || flowsOf("concern", id).some(function (f) { return flowChannels(f).indexOf(c.id) >= 0; });
      if (kind === "flow") { var f = D.flow(id); return f ? flowChannels(f).indexOf(c.id) >= 0 : false; }
      return false;
    });
  }
  function actorsOf(kind, id) {
    var set = new Set();
    if (kind === "flow") { var f = D.flow(id); if (f) flowActors(f).forEach(function (a) { set.add(a); }); }
    if (kind === "concern") {
      channelsOf("concern", id).forEach(function (c) { set.add(c.from); set.add(c.to); });
      flowsOf("concern", id).forEach(function (f) { flowActors(f).forEach(function (a) { set.add(a); }); });
    }
    return Array.from(set);
  }
  function concernsOfActor(id) {
    var set = new Set();
    channelsOf("actor", id).forEach(function (c) { (c.concerns || []).forEach(function (x) { set.add(x); }); });
    flowsOf("actor", id).forEach(function (f) { if (f.concern) set.add(f.concern); });
    return (M.concerns || []).filter(function (c) { return set.has(c.id); }).map(function (c) { return c.id; });
  }

  // ----- reading order

  // readingOrder: the concerns and flows in the order overview.md first
  // mentions them, then every concern in the model's order, each followed
  // by its flows (tours after flows), then flows without a concern.
  function readingOrder() {
    var out = [], seen = new Set();
    function add(ref) { if (!seen.has(ref) && X.refs.resolve(ref).obj) { seen.add(ref); out.push(ref); } }
    refsIn(M.overview || "").forEach(function (r) { if (/^(concern|flow):/.test(r)) add(r); });
    function flowsOfC(id) {
      return U.sortBy((M.flows || []).filter(function (f) { return (f.concern || "") === id; }), function (f) { return (f.kind === "tour" ? "1" : "0") + f.title.toLowerCase(); });
    }
    (M.concerns || []).forEach(function (c) {
      add("concern:" + c.id);
      flowsOfC(c.id).forEach(function (f) { add("flow:" + f.id); });
    });
    flowsOfC("").forEach(function (f) { add("flow:" + f.id); });
    (M.flows || []).forEach(function (f) { add("flow:" + f.id); });
    return out;
  }

  // ----- layout writers

  // writerTrusts maps a writer of layout.toml to zone trust values:
  // "box" to the untrusted zones, "host" to the host, "user" to the zone of
  // the model's person actors, "both" to box and host.
  function writerTrusts(w) {
    if (w === "box") return ["untrusted"];
    if (w === "host") return ["host"];
    if (w === "both") return ["untrusted", "host"];
    if (w === "user") {
      var p = (M.actors || []).filter(function (a) { return a.kind === "person"; })[0];
      var z = p ? D.zone(p.zone) : null;
      return [z ? z.trust : "external"];
    }
    return [];
  }

  // ----- text

  var REF_RE = /\[\[([^\]|\n]+)(?:\|[^\]\n]+)?\]\]/g;
  function refsIn(text) {
    var out = [], m;
    REF_RE.lastIndex = 0;
    while ((m = REF_RE.exec(text || ""))) out.push(m[1].trim());
    return out;
  }

  // symbolName: "Daemon.apiSend" for a method, "resolveSession" for a
  // function; the label of any other reference.
  function symbolName(ref) {
    if (!ref) return "";
    var info = X.refs.resolve(ref);
    if (info.kind === "symbol" && info.obj) {
      var p = info.obj.parent ? D.sym(info.obj.parent) : null;
      return p ? p.name + "." + info.obj.name : info.obj.name;
    }
    return info.label;
  }

  return {
    steps: steps, stepAt: stepAt, noOf: noOf, channelOf: channelOf, crosses: crosses,
    flowActors: flowActors, flowChannels: flowChannels, flowsOf: flowsOf, channelsOf: channelsOf, actorsOf: actorsOf,
    concernsOfActor: concernsOfActor, readingOrder: readingOrder, writerTrusts: writerTrusts, refsIn: refsIn,
    symbolName: symbolName, trustOfActor: trustOfActor
  };
})();
