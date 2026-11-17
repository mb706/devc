// The Flows view. Index (#/flows): flows grouped by concern, tours apart,
// each with its trigger and its actors. Player (#/flows/<id>?step=<n>):
// the sequence diagram beside the current step (its text, checks and the
// connection the tool found from the previous step), with the step's code
// in the inspector. j/k or the arrow keys step through; the step is in the
// URL. A tour uses the player without the diagram; a narrow window gets a
// compact list of steps instead of the diagram.
//
// The sequence diagram: one lifeline per actor, headed in its zone's
// colour, the zones as bands; one row per step. A step of one actor is a
// box on its lifeline naming its code; a message is an arrow with its
// label and a mark for how it travels (via); replies are dashed; a message
// that crosses a trust boundary is heavier, with a gate where it enters
// the receiver's zone. Sections are headings across the lifelines.
//
//   X.seq.render(host, flow, {onSelect(no)}) -> {select(no), destroy()}
(function () {
  "use strict";
  var U = X.util, D = X.data, M = D.model, R = X.refs, esc = U.esc;
  var NS = "http://www.w3.org/2000/svg";

  var VIA = {
    exec: ["v-exec", "starts a program"], rpc: ["rpc", "RPC call"], http: ["route", "HTTP request"], event: ["event", "session event"],
    cmd: ["command", "runs a devc command"], reply: ["v-reply", "reply"], file: ["file", "through a file"], signal: ["v-signal", "signal"], stream: ["v-stream", "stream"],
    net: ["v-net", "network connection to a listener"]
  };
  function viaKind(v) { return (v || "").split(":")[0]; }
  function viaIcon(v) { var k = VIA[viaKind(v)]; return k ? U.icon(k[0], "via") : ""; }
  function viaTitle(v) {
    var k = viaKind(v), t = VIA[k] ? VIA[k][1] : (v || "");
    var rest = (v || "").slice(k.length + 1);
    return rest ? t + ": " + rest : t;
  }
  function actorTitle(id) { var a = D.actor(id); return a ? a.title : id; }
  function zoneTrust(id) { var z = D.zoneOfActor(id); return z ? z.trust : "external"; }

  // status of a step: the worst of its connections.
  function status(s) {
    var st = s.unchecked ? "unchecked" : "";
    (s.connections || []).forEach(function (c) {
      if (c.status === "missing") st = "missing";
      else if (c.status === "unchecked" && st !== "missing") st = "unchecked";
    });
    return st;
  }

  // ----- the sequence diagram

  var ROW = 36, SEC = 32, GUTTER = 40, HEAD = 64;
  // textWidth measures a label as the diagram draws it.
  var measure = null;
  function textWidth(t, mono) {
    if (!measure) measure = document.createElement("canvas").getContext("2d");
    var cs = getComputedStyle(document.documentElement);
    measure.font = "11.5px " + (mono ? cs.getPropertyValue("--mono") : cs.getPropertyValue("--font"));
    return measure.measureText(t || "").width;
  }

  function renderSeq(host, flow, opts) {
    opts = opts || {};
    var lanes = (flow.actors || []).slice();
    (flow.steps || []).forEach(function (s) { [s.at, s.from, s.to].forEach(function (a) { if (a && lanes.indexOf(a) < 0) lanes.push(a); }); });
    var steps = X.model.steps(flow);
    var root = U.h("div.seq");
    host.appendChild(root);
    var cur = 0, rowsByNo = new Map();

    function draw() {
      var W = Math.max(host.clientWidth, 200);
      var colW = Math.max(84, Math.floor((W - GUTTER - 10) / Math.max(1, lanes.length)));
      var width = GUTTER + colW * lanes.length + 6;
      var xs = lanes.map(function (_, i) { return GUTTER + colW * (i + 0.5); });
      var laneOf = new Map();
      lanes.forEach(function (a, i) { laneOf.set(a, i); });
      // Bands: consecutive lifelines of one zone.
      var bands = [];
      lanes.forEach(function (a, i) {
        var z = D.zoneOfActor(a), id = z ? z.id : "";
        var last = bands[bands.length - 1];
        if (last && last.zone === id) last.to = i;
        else bands.push({ zone: id, z: z, from: i, to: i });
      });
      var bandOf = lanes.map(function (_, i) { return bands.filter(function (b) { return i >= b.from && i <= b.to; })[0]; });

      // Rows.
      var y = 0, rows = [];
      (flow.steps || []).forEach(function (s, i) {
        if (s.section) { rows.push({ sec: s.section, y: y, h: SEC }); y += SEC; return; }
        rows.push({ s: s, i: i, no: X.model.noOf(flow, i), y: y, h: ROW });
        y += ROW;
      });
      var H = y + 14;

      // Narrow lanes give the actor's name all the room, on two lines.
      root.classList.toggle("narrow", colW < 128);
      var html = '<div class="seq-head" style="width:' + width + 'px">';
      bands.forEach(function (b) {
        var x0 = GUTTER + colW * b.from, w = colW * (b.to - b.from + 1);
        html += '<div class="seq-band' + (b.z ? " z-" + esc(b.z.trust) : "") + '" style="left:' + x0 + "px;width:" + w + 'px"' + (b.z ? ' data-ref="zone:' + esc(b.z.id) + '" data-nocard title="' + esc(b.z.title) + '"' : "") + ">" + esc(b.z ? b.z.title : "") + "</div>";
      });
      lanes.forEach(function (a, i) {
        var act = D.actor(a);
        html += '<div class="seq-actor z-' + esc(zoneTrust(a)) + '" style="left:' + (GUTTER + colW * i + 4) + "px;width:" + (colW - 8) + 'px" data-ref="actor:' + esc(a) + '">' +
          U.icon((X.runtimeMap && X.runtimeMap.KIND_ICON[act && act.kind]) || "actor", "seq-kind") + "<span>" + esc(actorTitle(a)) + "</span></div>";
      });
      html += "</div>";

      html += '<div class="seq-body" style="width:' + width + "px;height:" + H + 'px">';
      // Zone tints and lifelines, then arrows, in one SVG behind the rows.
      var svg = '<svg class="seq-svg" width="' + width + '" height="' + H + '" aria-hidden="true">';
      bands.forEach(function (b, k) {
        if (!b.z) return;
        svg += '<rect class="seq-tint z-' + esc(b.z.trust) + '" x="' + (GUTTER + colW * b.from) + '" y="0" width="' + colW * (b.to - b.from + 1) + '" height="' + H + '"/>';
        if (k > 0) svg += '<line class="seq-edge" x1="' + (GUTTER + colW * b.from) + '" y1="0" x2="' + (GUTTER + colW * b.from) + '" y2="' + H + '"/>';
      });
      xs.forEach(function (x) { svg += '<line class="seq-life" x1="' + x + '" y1="0" x2="' + x + '" y2="' + H + '"/>'; });
      rows.forEach(function (r) {
        if (r.sec || !(r.s.from && r.s.to)) return;
        var a = laneOf.get(r.s.from), b = laneOf.get(r.s.to);
        var yy = r.y + 24, x1 = xs[a], x2 = xs[b];
        var cross = X.model.crosses(r.s), reply = r.s.via === "reply";
        var cls = "seq-msg" + (cross ? " bnd" : "") + (reply ? " reply" : "") + " n" + r.no;
        if (a === b) {
          svg += '<g class="' + cls + '"><path class="seq-line" d="M' + (x1 + 4) + "," + (yy - 8) + "h24v12h-20\"/>" +
            '<path class="seq-head-a" d="M' + (x1 + 4) + "," + (yy + 4) + "l7,-3.6v7.2z\"/></g>";
          return;
        }
        var dir = x2 > x1 ? 1 : -1;
        var sx = x1 + dir * 4, ex = x2 - dir * 5;
        svg += '<g class="' + cls + '"><line class="seq-line" x1="' + sx + '" y1="' + yy + '" x2="' + (ex - dir * 6) + '" y2="' + yy + '"/>' +
          '<path class="seq-head-a" d="M' + ex + "," + yy + "l" + (-dir * 8) + ",-4v8z\"/>";
        if (cross) {
          // The gate: where the message enters the receiver's band.
          var bb = bandOf[b], gx = dir > 0 ? GUTTER + colW * bb.from : GUTTER + colW * (bb.to + 1);
          if (bandOf[a] === bb) gx = (x1 + x2) / 2;
          svg += '<path class="seq-gate" d="M' + (gx - 5.5) + "," + yy + "l5.5,-4.5l5.5,4.5l-5.5,4.5z\"/>";
        }
        svg += "</g>";
      });
      svg += "</svg>";
      html += svg;

      rows.forEach(function (r) {
        if (r.sec) {
          html += '<div class="seq-sec" style="top:' + r.y + "px;height:" + r.h + 'px"><span>' + esc(r.sec) + "</span></div>";
          return;
        }
        var s = r.s, st = status(s);
        html += '<div class="seq-row" data-no="' + r.no + '" style="top:' + r.y + "px;height:" + r.h + 'px" title="' + esc(rowTitle(s, r.no)) + '">' +
          '<span class="seq-no' + (st ? " " + st : "") + '">' + r.no + (st === "unchecked" ? '<i class="seq-st" title="the connection to this step is not verified by the tool"></i>' : st === "missing" ? '<i class="seq-st miss" title="no connection found to this step"></i>' : "") + "</span>";
        // A row holds one step, so a label or box may reach past its lanes:
        // centred on its arrow or lifeline, kept inside the diagram.
        var right = width - 6;
        if (s.from && s.to) {
          var a = laneOf.get(s.from), b = laneOf.get(s.to);
          var text = s.label || viaTitle(s.via);
          var need = textWidth(text, true) + 26, span = Math.abs(xs[b] - xs[a]) - 16;
          var w = Math.min(Math.max(span, need), right - GUTTER), mid = (xs[a] + xs[b]) / 2;
          if (a === b) { w = Math.min(need, right - xs[a] - 34); mid = xs[a] + 34 + w / 2; }
          var left = Math.max(GUTTER + 2, Math.min(right - w, mid - w / 2));
          html += '<div class="seq-lab' + (s.via === "reply" ? " reply" : "") + (need > span + 4 ? " wide" : "") + '" style="left:' + Math.round(left) + "px;width:" + Math.round(w) + 'px">' +
            '<span class="seq-via" title="' + esc(viaTitle(s.via)) + '">' + viaIcon(s.via) + "</span><span class=\"seq-lt\">" + esc(text) + "</span></div>";
        } else if (s.at) {
          var k = laneOf.get(s.at), name = s.label || (s.ref ? X.model.symbolName(s.ref) : (s.external ? firstWords(s.text) : ""));
          var bw = Math.min(Math.max(colW - 12, textWidth(name, !!s.ref) + 18), right - GUTTER - 2, colW * 2.6);
          var bl = Math.max(GUTTER + 2, Math.min(right - bw, xs[k] - bw / 2));
          html += '<div class="seq-box z-' + esc(zoneTrust(s.at)) + (s.external ? " ext" : "") + '" style="left:' + Math.round(bl) + "px;width:" + Math.round(bw) + 'px"><span class="' + (s.ref ? "mono" : "") + '">' + esc(name) + "</span></div>";
        }
        html += "</div>";
      });
      html += "</div>";
      root.style.width = width + "px";
      root.innerHTML = html;
      rowsByNo = new Map();
      root.querySelectorAll(".seq-row").forEach(function (el) { rowsByNo.set(+el.getAttribute("data-no"), el); });
      mark(false);
    }
    function rowTitle(s, no) {
      var who = s.at ? actorTitle(s.at) : actorTitle(s.from) + " → " + actorTitle(s.to);
      return no + ". " + who + (s.label ? ": " + s.label : "") + (s.text ? "\n" + X.md.plainText(U.firstSentence(s.text)) : "");
    }
    function firstWords(t) { var w = X.md.plainText(t || "").split(/\s+/).slice(0, 3).join(" "); return w ? w + "…" : "external"; }

    function mark(scroll) {
      root.querySelectorAll(".seq-row.cur").forEach(function (el) { el.classList.remove("cur"); });
      root.querySelectorAll(".seq-msg.cur").forEach(function (el) { el.classList.remove("cur"); });
      var el = rowsByNo.get(cur);
      if (!el) return;
      el.classList.add("cur");
      var g = root.querySelector(".seq-msg.n" + cur);
      if (g) g.classList.add("cur");
      if (scroll) {
        var sc = host, top = el.offsetTop + HEAD, h = sc.clientHeight;
        if (top - HEAD < sc.scrollTop + 10 || top + ROW > sc.scrollTop + h - 20) sc.scrollTop = Math.max(0, top - HEAD - h / 3);
      }
    }
    root.addEventListener("click", function (ev) {
      if (ev.target.closest("[data-ref]")) return;
      var r = ev.target.closest(".seq-row");
      if (r && opts.onSelect) opts.onSelect(+r.getAttribute("data-no"));
    });
    draw();
    // A resize (the inspector opening, the window) redraws and keeps the
    // current step in view.
    var ro = window.ResizeObserver ? new ResizeObserver(U.debounce(function () { if (root.isConnected) { draw(); mark(true); } }, 60)) : null;
    if (ro) ro.observe(host);
    return {
      select: function (no) { cur = no; mark(true); },
      minWidth: GUTTER + 84 * lanes.length,
      destroy: function () { if (ro) ro.disconnect(); }
    };
  }
  X.seq = { render: renderSeq };

  // ----- the compact list of steps

  function stepList(flow, cur) {
    var h = '<ol class="fl-list">';
    (flow.steps || []).forEach(function (s, i) {
      if (s.section) { h += '<li class="fl-sec">' + esc(s.section) + "</li>"; return; }
      var no = X.model.noOf(flow, i), st = status(s);
      var who = s.at ? actorDot(s.at) : actorDot(s.from) + '<span class="fl-arrow' + (X.model.crosses(s) ? " bnd" : "") + '">' + (X.model.crosses(s) ? '<i class="gate"></i>' : "") + "→</span>" + actorDot(s.to);
      // A tour's step may name only code: the name, once.
      if (!s.at && !s.from) who = "";
      var what = s.from ? (viaIcon(s.via) + esc(s.label || viaTitle(s.via))) : s.ref ? '<span class="mono">' + esc(X.model.symbolName(s.ref)) + "</span>" : "";
      h += '<li class="fl-step' + (no === cur ? " cur" : "") + '" data-no="' + no + '"><span class="seq-no' + (st ? " " + st : "") + '">' + no + "</span>" +
        '<span class="fl-who">' + who + '</span><span class="fl-what">' + what + "</span>" +
        '<span class="fl-text">' + X.doc.plain(U.firstSentence(X.md.plainText(s.text || ""))) + "</span></li>";
    });
    return h + "</ol>";
  }
  function actorDot(id) {
    return '<span class="fl-actor z-' + esc(zoneTrust(id)) + '"><i class="zd"></i>' + esc(actorTitle(id)) + "</span>";
  }

  // ----- the step panel

  // chain draws a path of the call graph: → a call, ⇢ a call through an
  // interface, ⤳ a mention that is no call (the function handed over as a
  // value, or held in a table).
  function chain(path, dyn, val) {
    var d = new Set(dyn || []), v = new Set(val || []);
    return '<div class="fs-chain">' + (path || []).map(function (r, i) {
      if (!i) return R.chip(r, { cls: "small" });
      var hop = v.has(i) ? ["val", "⤳", "passes it as a value or holds it in a table: the tool sees it handed over, not called"] :
        d.has(i) ? ["dyn", "⇢", "a call through an interface"] : ["", "→", "calls"];
      return '<span class="fs-hop ' + hop[0] + '" title="' + hop[2] + '">' + hop[1] + "</span>" + R.chip(r, { cls: "small" });
    }).join("") + "</div>";
  }
  // weak: a note under a path that rests on a function passed as a value.
  function weakNote(c) {
    return (c.valueEdges || []).length ? '<p class="small fs-weak">' + U.icon("info") + "Part of this path is a function passed as a value (⤳): the tool sees it handed over, not called. Weaker evidence than a call.</p>" : "";
  }
  var KIND_TEXT = { same: "the same code", path: "a path in the call graph", rpc: "through the RPC method", route: "through the route", event: "through the event type", command: "through the command", reply: "the answer to a call returns to its caller" };
  function connection(flow, s, c) {
    var fromNo = X.model.noOf(flow, c.from);
    var head = '<div class="fs-ch"><a class="stepno" href="' + esc(X.router.href("flows", flow.id, { step: fromNo })) + '" data-step="' + fromNo + '">' + fromNo + "</a>" +
      '<span class="muted small">continues in</span>' + R.chip("actor:" + c.actor, { cls: "small" });
    var body = "";
    if (c.status === "found") {
      head += (c.valueEdges || []).length ? '<span class="tag ok weak" title="the path passes a function as a value">verified, weaker</span>' : '<span class="tag ok">verified</span>';
      body += '<p class="small fs-how">' + esc(KIND_TEXT[c.kind] || c.kind || "") + (c.via ? " " + R.chip(c.via, { cls: "small" }) : "") + "</p>";
      if (c.path && c.path.length) body += chain(c.path, c.dynamicEdges, c.valueEdges) + weakNote(c);
    } else if (c.status === "unchecked") {
      head += '<span class="tag">not verified</span>';
      body += '<div class="fs-uncheck"><b>The tool cannot see this connection.</b> The model says: ' + X.md.inline(s.unchecked || "", {}) + "</div>";
    } else {
      head += '<span class="tag warn">not found</span>';
      body += '<div class="fs-uncheck warn">No path in the call graph from the code of step ' + fromNo + " to this step's code. <span class=\"muted\">The build reports this as a warning.</span></div>";
    }
    if (c.note) body += '<p class="muted small fs-note">' + X.md.inline(c.note, {}) + "</p>";
    return '<div class="fs-conn ' + esc(c.status) + '">' + head + "</div>" + body + "</div>";
  }
  function firstDoc(ref) {
    var s = D.sym(ref);
    return s && s.doc ? '<div class="sp-first">' + X.doc.inline(U.firstSentence(s.doc), { pkg: s.package }) + "</div>" : "";
  }

  function stepPanel(flow, entry, total) {
    var s = entry.step, no = entry.no;
    var h = '<div class="fs-nav"><button class="btn icon" data-a="prev" title="Previous step (k or ↑)"' + (no <= 1 ? " disabled" : "") + ">" + U.icon("back") + "</button>" +
      '<span class="fs-count">Step <b>' + no + "</b> of " + total + "</span>" +
      '<button class="btn icon" data-a="next" title="Next step (j or ↓)"' + (no >= total ? " disabled" : "") + ">" + U.icon("fwd") + "</button>" +
      '<span class="fs-keys muted small"><kbd>j</kbd> <kbd>k</kbd></span></div>';
    if (entry.section) h += '<div class="fs-section">' + esc(entry.section) + "</div>";
    var who;
    if (s.from && s.to) {
      var cross = X.model.crosses(s), ch = X.model.channelOf(s);
      who = '<div class="fs-who">' + R.chip("actor:" + s.from) + '<span class="fs-arrow' + (cross ? " bnd" : "") + (s.via === "reply" ? " reply" : "") + '">' + (cross ? '<i class="gate"></i>' : "") + "</span>" + R.chip("actor:" + s.to) + "</div>" +
        '<div class="fs-msg">' + viaIcon(s.via) + '<span class="fs-label mono">' + esc(s.label || "") + "</span></div>" +
        '<div class="fs-meta">' + (s.via && /^(rpc|http|event|cmd):/.test(s.via) ? R.chip(s.via, { cls: "small" }) : '<span class="tag">' + esc(viaTitle(s.via)) + "</span>") +
        (ch ? '<span class="muted small">on</span>' + R.chip("channel:" + ch.id, { cls: "small" }) : "") +
        (cross ? '<span class="tag warn" title="sender and receiver trust each other differently">crosses a trust boundary</span>' : "") + "</div>";
    } else if (s.at) {
      who = '<div class="fs-who">' + R.chip("actor:" + s.at) + (s.external ? '<span class="tag">not devc code</span>' : "") + "</div>";
    } else who = "";
    h += who;
    if (s.text) h += '<div class="md fs-text">' + X.md.render(s.text, {}) + "</div>";
    if (s.detail) h += '<div class="md fs-detail">' + X.md.render(s.detail, {}) + "</div>";
    if (s.ref) h += '<section class="sp-sec"><h3 class="sp-h">Code <span class="muted small fs-insp">in the inspector</span></h3>' + R.chip(s.ref) + firstDoc(s.ref) + "</section>";
    if ((s.checks || []).length) h += '<section class="sp-sec"><h3 class="sp-h">Checks what crossed <span class="count">' + s.checks.length + '</span></h3><ul class="sp-members">' +
      s.checks.map(function (c) { return "<li>" + R.chip(c) + firstDoc(c) + "</li>"; }).join("") + "</ul></section>";
    if (flow.kind !== "tour") {
      var conns = s.connections || [];
      h += '<section class="sp-sec"><h3 class="sp-h">From the previous step</h3>';
      var shown = conns.some(function (c) { return c.status === "unchecked"; });
      if (s.unchecked && !shown) {
        // The author's reason, where the tool has no step to connect from.
        h += '<div class="fs-conn unchecked"><div class="fs-ch"><span class="muted small">The connection to this step</span><span class="tag">not verified</span></div>' +
          '<div class="fs-uncheck"><b>The tool cannot see this connection.</b> The model says: ' + X.md.inline(s.unchecked, {}) + "</div></div>";
      } else if (!conns.length) h += '<p class="muted small">' + (no === 1 ? "The first step." : s.external ? "Not devc code: nothing to connect." : "No earlier step of " + (s.at ? "this actor" : "these actors") + " has code to connect from.") + "</p>";
      conns.forEach(function (c) { h += connection(flow, s, c); });
      h += "</section>";
      if (s.sender) {
        h += '<section class="sp-sec"><h3 class="sp-h">How the code sends it</h3>' +
          (s.sender.status === "found" ? '<p class="small fs-how">' + esc(KIND_TEXT[s.sender.kind] || "") + (s.sender.via ? " " + R.chip(s.sender.via, { cls: "small" }) : "") + "</p>" + chain(s.sender.path, s.sender.dynamicEdges, s.sender.valueEdges) + weakNote(s.sender) : '<div class="fs-uncheck warn">The step\'s code does not send ' + esc(s.via) + ".</div>") +
          (s.sender.note ? '<p class="muted small fs-note">' + X.md.inline(s.sender.note, {}) + "</p>" : "") + "</section>";
      }
    }
    return h;
  }

  // ----- the player

  var S = null;

  function player(el, r, flow) {
    var tour = flow.kind === "tour";
    var steps = X.model.steps(flow);
    el.innerHTML = '<div class="fp' + (tour ? " tour" : "") + '">' +
      '<header class="fp-head"><div class="fp-title">' + U.icon(tour ? "list" : "flow") + "<h1>" + esc(flow.title) + "</h1>" +
      (tour ? '<span class="tag">tour</span>' : "") +
      (flow.concern ? R.chip("concern:" + flow.concern, { cls: "small" }) : "") +
      (flow.trigger ? '<code class="fp-trigger" title="trigger">' + esc(flow.trigger) + "</code>" : "") +
      '<span class="fp-tools"><a class="btn ghost small" href="' + esc(X.router.hrefKeep("runtime", "", { flow: flow.id })) + '" title="The flow\'s actors and channels on the runtime map">' + U.icon("actor") + '<span class="lbl">On the runtime map</span></a>' +
      '<button class="btn ghost small" data-a="legend" title="What the marks of the diagram mean">' + U.icon("info") + '<span class="lbl">Legend</span></button></span></div>' +
      (flow.summary ? '<p class="fp-sum">' + X.md.inline(flow.summary, {}) + "</p>" : "") +
      '<div class="fp-legend hidden">' + legend() + "</div></header>" +
      '<div class="fp-main"><div class="fp-diagram"></div><aside class="fp-step"></aside></div></div>';
    S = { el: el, flow: flow, steps: steps, no: 0, seq: null };
    var diag = el.querySelector(".fp-diagram");
    if (!tour && steps.length) {
      S.seq = renderSeq(diag, flow, { onSelect: function (no) { go(no); } });
      var ro = window.ResizeObserver ? new ResizeObserver(function () { compact(); }) : null;
      if (ro) ro.observe(diag);
      S.ro = ro;
    }
    S.listBox = U.h("div.fp-listbox");
    diag.appendChild(S.listBox);
    S.listBox.addEventListener("click", function (ev) {
      var li = ev.target.closest(".fl-step");
      if (li && !ev.target.closest("[data-ref]")) go(+li.getAttribute("data-no"));
    });
    el.querySelector('[data-a="legend"]').onclick = function () { el.querySelector(".fp-legend").classList.toggle("hidden"); };
    el.querySelector(".fp-step").addEventListener("click", function (ev) {
      var b = ev.target.closest("[data-a]");
      if (b && b.getAttribute("data-a") === "prev") go(S.no - 1);
      if (b && b.getAttribute("data-a") === "next") go(S.no + 1);
      var sn = ev.target.closest("[data-step]");
      if (sn && !U.isModifiedClick(ev)) { ev.preventDefault(); go(+sn.getAttribute("data-step")); }
    });
    compact();
    show(r);
  }
  // compact: the list replaces the diagram when the lifelines do not fit.
  function compact() {
    if (!S) return;
    var diag = S.el.querySelector(".fp-diagram");
    var useList = !S.seq || diag.clientWidth < S.seq.minWidth;
    diag.classList.toggle("as-list", useList);
    if (useList) S.listBox.innerHTML = stepList(S.flow, S.no);
  }

  function legend() {
    var vias = new Set();
    (M.flows || []).forEach(function (f) { (f.steps || []).forEach(function (s) { if (s.via) vias.add(viaKind(s.via)); }); });
    var h = Object.keys(VIA).filter(function (k) { return vias.has(k); }).map(function (k) { return '<span class="lg">' + U.icon(VIA[k][0]) + esc(VIA[k][1]) + "</span>"; }).join("");
    h += '<span class="lg"><i class="lgl"></i>message</span><span class="lg"><i class="lgl dash"></i>reply</span>' +
      '<span class="lg"><i class="lgl bnd"><b></b></i>crosses a trust boundary, gate where it enters</span>' +
      '<span class="lg"><span class="seq-box-lg">name</span>an actor runs this code</span>' +
      '<span class="lg"><span class="seq-no unchecked">4<i class="seq-st"></i></span>connection not verified by the tool</span>' +
      '<span class="lg"><span class="seq-no missing">4<i class="seq-st miss"></i></span>no connection found</span>';
    return h;
  }

  // stepTarget: what the inspector shows for a step: its code, else the
  // actor (external steps have no code).
  function stepTarget(s) { return s.ref || (s.at ? "actor:" + s.at : s.from ? "actor:" + s.from : ""); }

  function go(no) {
    if (!S || !S.steps.length) return;
    no = Math.max(1, Math.min(S.steps.length, no));
    if (no === S.no) return;
    var e = S.steps[no - 1], s = e.step;
    var target = stepTarget(s);
    var p = {};
    Object.keys(X.router.current.params).forEach(function (k) { p[k] = X.router.current.params[k]; });
    p.step = String(no);
    if (target) p.i = target;
    X.router.replace(X.router.href("flows", S.flow.id, p));
  }

  function show(r) {
    var total = S.steps.length;
    if (!total) { S.el.querySelector(".fp-step").innerHTML = '<p class="muted pad">This flow has no steps.</p>'; return; }
    var no = Math.max(1, Math.min(total, parseInt(r.params.step, 10) || 1));
    var first = !S.no;
    S.no = no;
    var e = S.steps[no - 1];
    var panel = S.el.querySelector(".fp-step");
    panel.innerHTML = stepPanel(S.flow, e, total);
    panel.scrollTop = 0;
    if (S.seq) S.seq.select(no);
    var diag = S.el.querySelector(".fp-diagram");
    if (diag.classList.contains("as-list")) {
      S.listBox.innerHTML = stepList(S.flow, no);
      var li = S.listBox.querySelector(".fl-step.cur");
      if (li) { var lr = li.getBoundingClientRect(), dr = diag.getBoundingClientRect(); if (lr.top < dr.top || lr.bottom > dr.bottom) diag.scrollTop += lr.top - dr.top - dr.height / 3; }
    }
    // Opening the player shows the first step's code in the inspector.
    // (After the route that mounted the view has finished, which would
    // otherwise close the inspector again.)
    if (first && !r.params.i) {
      var target = stepTarget(e.step), fid = S.flow.id;
      if (target) setTimeout(function () {
        if (S && S.flow.id === fid && X.router.current && !X.router.current.params.i) X.router.replace(X.router.href("flows", fid, Object.assign({}, X.router.current.params, { step: String(S.no), i: target })));
      }, 0);
    }
  }

  document.addEventListener("keydown", function (ev) {
    if (!S || !S.el.isConnected || ev.altKey || ev.ctrlKey || ev.metaKey) return;
    var t = ev.target, typing = t && (t.tagName === "INPUT" || t.tagName === "TEXTAREA" || t.tagName === "SELECT" || t.isContentEditable);
    if (typing || document.querySelector(".modal.on")) return;
    if (ev.key === "j" || ev.key === "ArrowDown" || ev.key === "ArrowRight") { ev.preventDefault(); go(S.no + 1); }
    else if (ev.key === "k" || ev.key === "ArrowUp" || ev.key === "ArrowLeft") { ev.preventDefault(); go(S.no - 1); }
  });

  // ----- the index

  // The flows of each concern in the model's order, the tours apart; the
  // groups flow in columns so that sixty flows fit on a few screens, and a
  // filter narrows them by title, trigger, summary, actor or concern.
  function flowRow(f) {
    var n = X.model.steps(f).length;
    // (Names, not chips: the entry is a link, and links do not nest.)
    var actors = (f.actors || []).map(actorDot).join("");
    var c = f.concern ? D.concern(f.concern) : null;
    var text = [f.title, f.id, f.trigger || "", X.md.plainText(f.summary || ""), c ? c.title : ""].concat((f.actors || []).map(actorTitle)).join(" ").toLowerCase();
    return '<a class="fx-row" href="' + esc(X.router.href("flows", f.id)) + '" data-ref="flow:' + esc(f.id) + '" data-act="full" data-nocard data-text="' + esc(text) + '">' +
      '<div class="fx-top">' + U.icon(f.kind === "tour" ? "list" : "flow") + '<span class="fx-title">' + esc(f.title) + '</span><span class="muted small num">' + U.plural(n, "step") + "</span></div>" +
      (f.trigger ? '<code class="fx-trigger" title="' + esc(f.trigger) + '">' + esc(f.trigger) + "</code>" : "") +
      (f.summary ? '<p class="fx-sum">' + X.doc.plain(X.md.plainText(f.summary)) + "</p>" : "") +
      (actors ? '<div class="fx-actors">' + actors + "</div>" : "") + "</a>";
  }
  function group(head, list) {
    return '<section class="fx-group"><h2 class="fx-gh">' + head + ' <span class="count">' + list.length + "</span></h2>" + list.map(flowRow).join("") + "</section>";
  }
  function index(el, r) {
    var all = M.flows || [];
    var flows = all.filter(function (f) { return f.kind !== "tour"; }), tours = all.filter(function (f) { return f.kind === "tour"; });
    var h = '<div class="page fx"><h1>Flows</h1><p class="lead">What happens, step by step: each flow is a sequence across actors, with the code of every step and the connection the build found between them. Tours are reading orders through the code.</p>';
    if (!all.length) { el.innerHTML = h + '<p class="muted">The model has no flows yet.</p></div>'; return; }
    h += '<div class="fx-bar"><input type="search" placeholder="Filter ' + all.length + ' flows and tours: a title, a command, an actor…" value="' + esc(r.params.q || "") + '" spellcheck="false">' +
      '<span class="muted small fx-n">' + U.plural(flows.length, "flow") + " · " + U.plural(tours.length, "tour") + "</span></div>";
    var groups = (M.concerns || []).map(function (c) { return { c: c, list: flows.filter(function (f) { return f.concern === c.id; }) }; }).filter(function (g) { return g.list.length; });
    var other = flows.filter(function (f) { return !f.concern || !D.concern(f.concern); });
    h += '<div class="fx-cols">';
    groups.forEach(function (g) { h += group(R.chip("concern:" + g.c.id, { cls: "flat" }), g.list); });
    if (other.length) h += group("Other flows", other);
    if (tours.length) h += group(U.icon("list") + "Tours", tours);
    h += '</div><p class="muted fx-none hidden">No flow matches.</p></div>';
    el.innerHTML = h;
    var input = el.querySelector(".fx-bar input"), count = el.querySelector(".fx-n"), none = el.querySelector(".fx-none");
    function filter(q) {
      var toks = q.toLowerCase().split(/\s+/).filter(Boolean), n = 0;
      el.querySelectorAll(".fx-row").forEach(function (a) {
        var on = toks.every(function (k) { return a.getAttribute("data-text").indexOf(k) >= 0; });
        a.classList.toggle("hidden", !on);
        if (on) n++;
      });
      el.querySelectorAll(".fx-group").forEach(function (g) { g.classList.toggle("hidden", !g.querySelector(".fx-row:not(.hidden)")); });
      count.textContent = toks.length ? n + " of " + all.length : U.plural(flows.length, "flow") + " · " + U.plural(tours.length, "tour");
      none.classList.toggle("hidden", n > 0);
    }
    input.addEventListener("input", U.debounce(function () { filter(input.value); X.router.setParams({ q: input.value || null }); }, 120));
    if (r.params.q) filter(r.params.q);
  }

  X.router.register("flows", {
    nav: "flows",
    title: function (r) { var f = D.flow(r.rest); return f ? f.title : "Flows"; },
    render: function (el, r) {
      if (!r.rest) { S = null; index(el, r); return; }
      var f = D.flow(r.rest);
      if (!f) { el.innerHTML = '<div class="page narrow"><h1>No such flow</h1><p class="muted"><code>' + esc(r.rest) + '</code> is not a flow of the model. See the <a href="#/flows">flows</a>.</p></div>'; return; }
      player(el, r, f);
    },
    update: function (r, prev) {
      // The index keeps its filter in the URL; typing must not re-render it.
      if (!S && !r.rest && prev && !prev.rest) return true;
      if (!S || !r.rest || r.rest !== S.flow.id) return false;
      show(r);
      return true;
    },
    unmount: function () { if (S) { if (S.seq) S.seq.destroy(); if (S.ro) S.ro.disconnect(); } S = null; }
  });

  // ----- references, cards, inspector, search

  R.define("flow", {
    card: function (info) {
      var f = info.obj, n = X.model.steps(f).length;
      return '<div class="hc-row muted">' + (f.kind === "tour" ? "tour" : "flow") + " · " + U.plural(n, "step") + (f.concern && D.concern(f.concern) ? " · " + esc(D.concern(f.concern).title) : "") + "</div>" +
        (f.trigger ? '<div class="hc-sig mono">' + esc(f.trigger) + "</div>" : "") +
        (f.summary ? '<div class="hc-doc">' + X.md.cardText(f.summary) + "</div>" : "") +
        ((f.actors || []).length ? '<div class="hc-runs">' + R.actorList(f.actors) + "</div>" : "");
    }
  });
  X.inspector.register("flow", function (info, body) {
    var f = info.obj;
    var h = (f.summary ? '<div class="md ins-lead">' + X.md.render(f.summary, {}) + "</div>" : "") +
      '<dl class="facts">' + (f.trigger ? "<dt>Trigger</dt><dd><code>" + esc(f.trigger) + "</code></dd>" : "") +
      (f.concern ? "<dt>Concern</dt><dd>" + R.chip("concern:" + f.concern) + "</dd>" : "") + "</dl>" +
      ((f.actors || []).length ? '<section class="sp-sec"><h3 class="sp-h">Actors</h3>' + R.actorList(f.actors) + "</section>" : "") +
      '<section class="sp-sec"><h3 class="sp-h">Steps <span class="count">' + X.model.steps(f).length + "</span></h3>" + stepList(f, 0).replace(/<li class="fl-step([^"]*)" data-no="(\d+)">/g, function (m, c, no) {
        return '<li class="fl-step' + c + '" data-no="' + no + '" data-href="' + esc(X.router.href("flows", f.id, { step: no })) + '">';
      }) + "</section>" +
      '<p><a class="btn" href="' + esc(X.router.href("flows", f.id)) + '">' + U.icon("full") + "Open the player</a></p>";
    var el = U.h("div.insp-pad.sp", { html: h });
    el.addEventListener("click", function (ev) {
      var li = ev.target.closest("[data-href]");
      if (li && !ev.target.closest("[data-ref]")) X.router.go(li.getAttribute("data-href"));
    });
    body.appendChild(el);
  });
  X.search.addSource({
    kind: "flow", title: "Flows and tours", order: 3,
    items: function () {
      return (M.flows || []).map(function (f) { return { ref: "flow:" + f.id, name: f.title, keys: [f.title, f.id, f.trigger || ""], detail: X.md.plainText(U.firstSentence(f.summary || "")), exported: true, weight: 60 }; });
    }
  });
})();
