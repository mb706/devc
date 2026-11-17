// Hover cards: hovering over any element with data-ref shows, after a short
// delay, what the reference is (X.refs.card). The card stays while the
// pointer is on it, so its own references can be hovered and clicked.
// Escape, a click elsewhere or scrolling hides it.
//
//   X.hovercard.hide()
//   X.hovercard.delay   milliseconds before a card appears (default 380)
X.hovercard = (function () {
  "use strict";
  var card = null, timer = null, hideTimer = null, anchor = null, shownFor = null;
  var api = { delay: 380, hide: hide };

  function el() {
    if (!card) {
      card = document.createElement("div");
      card.className = "hovercard";
      card.setAttribute("role", "tooltip");
      card.addEventListener("mouseenter", function () { clearTimeout(hideTimer); });
      card.addEventListener("mouseleave", scheduleHide);
      document.getElementById("layer-popups").appendChild(card);
    }
    return card;
  }

  function show(target) {
    var ref = target.getAttribute("data-ref");
    if (!ref || !document.body.contains(target)) return;
    var c = el();
    c.innerHTML = X.refs.card(ref);
    c.style.left = "0px"; c.style.top = "0px";
    c.classList.add("on");
    shownFor = target;
    place(c, target.getBoundingClientRect());
  }

  // place puts the card below the anchor, or above when there is no room,
  // kept inside the window.
  function place(c, r) {
    var w = c.offsetWidth, hgt = c.offsetHeight, vw = window.innerWidth, vh = window.innerHeight;
    var x = Math.min(Math.max(8, r.left), vw - w - 8);
    var y = r.bottom + 6;
    if (y + hgt > vh - 8 && r.top - hgt - 6 > 8) y = r.top - hgt - 6;
    else if (y + hgt > vh - 8) y = Math.max(8, vh - hgt - 8);
    c.style.left = Math.round(x) + "px";
    c.style.top = Math.round(y) + "px";
  }

  function hide() {
    clearTimeout(timer); clearTimeout(hideTimer);
    timer = null; anchor = null; shownFor = null;
    if (card) card.classList.remove("on");
  }
  function scheduleHide() {
    clearTimeout(hideTimer);
    hideTimer = setTimeout(hide, 180);
  }

  document.addEventListener("mouseover", function (ev) {
    var t = ev.target.closest ? ev.target.closest("[data-ref]") : null;
    if (card && card.contains(ev.target) && !t) { clearTimeout(hideTimer); return; }
    if (!t || t.hasAttribute("data-nocard")) {
      if (anchor || shownFor) { clearTimeout(timer); anchor = null; if (shownFor && !(card && card.contains(ev.target))) scheduleHide(); }
      return;
    }
    // Any reference cancels a pending hide: leaving one reference for the
    // next crosses a gap that schedules the hide, and the hide would
    // otherwise take down the next card (or cancel it before it shows).
    clearTimeout(hideTimer);
    if (t === shownFor || t === anchor) return;
    anchor = t;
    clearTimeout(timer);
    // Moving from one reference to the next while a card is open swaps
    // quickly; the first card waits for the full delay.
    timer = setTimeout(function () { if (anchor === t) show(t); }, shownFor ? 120 : api.delay);
  });
  document.addEventListener("mousedown", function (ev) { if (!(card && card.contains(ev.target))) hide(); }, true);
  document.addEventListener("keydown", function (ev) { if (ev.key === "Escape") hide(); });
  document.addEventListener("scroll", function (ev) { if (!(card && card.contains(ev.target))) hide(); }, true);
  window.addEventListener("blur", hide);

  return api;
})();
