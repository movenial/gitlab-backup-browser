// Copy buttons and live import status. No framework, no network besides
// polling /-/status.json while an import is running.
(function () {
  document.documentElement.classList.add("js");
  // Filter dropdowns apply as soon as they change.
  document.addEventListener("change", function (e) {
    var f = e.target.form;
    if (f && f.hasAttribute("data-autosubmit") && e.target.tagName === "SELECT") { clean(f); f.submit(); }
  });
  // Leave empty fields out of filter URLs (?sort=name, not ?q=&vis=&repo=&sort=name).
  function clean(f) {
    Array.prototype.forEach.call(f.elements, function (el) { if (el.name && el.value === "") el.disabled = true; });
  }
  document.addEventListener("submit", function (e) {
    if (e.target.hasAttribute("data-autosubmit")) clean(e.target);
  });
  window.addEventListener("pageshow", function () { // back button: re-enable
    document.querySelectorAll("form[data-autosubmit] [disabled]").forEach(function (el) { el.disabled = false; });
  });
  // Filter panels start open where there is room for them.
  if (window.matchMedia && window.matchMedia("(min-width: 641px)").matches) {
    document.querySelectorAll("details[data-open-wide]").forEach(function (d) { d.open = true; });
  }
  // Close dropdowns (top menu, branch picker) on outside click or Escape.
  var menus = "details.topmenu[open], details.menu[open]";
  document.addEventListener("click", function (e) {
    document.querySelectorAll(menus).forEach(function (m) {
      if (!m.contains(e.target)) m.removeAttribute("open");
    });
  });
  document.addEventListener("keydown", function (e) {
    if (e.key !== "Escape") return;
    document.querySelectorAll(menus).forEach(function (m) { m.removeAttribute("open"); m.querySelector("summary").focus(); });
  });
  // Line links: click a line number for #L12, shift-click another for
  // #L12-20; the lines are highlighted and the page scrolls to them.
  var anchor = 0;
  function markLines(scroll) {
    var m = /^#L(\d+)(?:-L?(\d+))?$/.exec(location.hash);
    document.querySelectorAll(".code tr.hl").forEach(function (tr) { tr.classList.remove("hl"); });
    if (!m) return;
    var a = +m[1], b = m[2] ? +m[2] : a;
    if (a > b) { var t = a; a = b; b = t; }
    anchor = a;
    for (var i = a; i <= b && i - a < 5000; i++) {
      var tr = document.getElementById("L" + i);
      if (tr) tr.classList.add("hl");
    }
    var first = document.getElementById("L" + a);
    if (scroll && first) first.scrollIntoView({ block: "center" });
  }
  document.addEventListener("click", function (e) {
    var a = e.target.closest && e.target.closest('.code .ln a[href^="#L"]');
    if (!a) return;
    e.preventDefault();
    var n = +a.getAttribute("href").slice(2);
    var hash = e.shiftKey && anchor && anchor !== n ? "#L" + Math.min(anchor, n) + "-" + Math.max(anchor, n) : "#L" + n;
    history.replaceState(null, "", hash);
    markLines(false);
    if (!e.shiftKey) anchor = n;
  });
  window.addEventListener("hashchange", function () { markLines(true); });
  markLines(true);

  // Confirm heavy actions (extractions, deleting caches).
  document.addEventListener("submit", function (e) {
    var msg = e.target.getAttribute("data-confirm");
    if (msg && !window.confirm(msg)) e.preventDefault();
  });
  // Copy buttons: data-copy="#input" copies a field, data-permalink a link.
  function copyText(text, b) {
    var done = function () { var t = b.textContent; b.textContent = "Copied"; setTimeout(function () { b.textContent = t; }, 1500); };
    var fallback = function () {
      var ta = document.createElement("textarea");
      ta.value = text; ta.setAttribute("readonly", ""); ta.style.position = "fixed"; ta.style.opacity = "0";
      document.body.appendChild(ta); ta.select();
      try { if (document.execCommand("copy")) done(); } catch (err) { /* nothing to do */ }
      ta.remove();
    };
    if (navigator.clipboard && window.isSecureContext) navigator.clipboard.writeText(text).then(done, fallback);
    else fallback();
  }
  document.addEventListener("click", function (e) {
    var b = e.target.closest && e.target.closest("[data-copy], [data-permalink]");
    if (!b) return;
    if (b.hasAttribute("data-copy")) {
      var el = document.querySelector(b.getAttribute("data-copy"));
      if (el) { el.select(); copyText(el.value, b); }
      return;
    }
    copyText(location.origin + b.getAttribute("data-permalink") + (/^#L\d/.test(location.hash) ? location.hash : ""), b);
  });

  var banner = document.getElementById("status");
  var steps = document.getElementById("load-steps");
  if (!steps && (!banner || banner.getAttribute("data-ready") === "true")) return;
  var text = document.getElementById("status-text");
  var phase = null;
  function poll() {
    fetch("/-/status.json", { cache: "no-store" }).then(function (r) { return r.json(); }).then(function (s) {
      if (s.ready && s.phase === "ready") { location.reload(); return; }
      if (steps) { // loading page: move the bar, reload when the step changes
        if (phase !== null && s.phase !== phase) { location.reload(); return; }
        phase = s.phase;
        var bar = document.getElementById("load-progress"), pct = document.getElementById("load-pct");
        if (bar && s.total > 0) { bar.max = s.total; bar.value = s.done; }
        if (pct && s.total > 0) pct.textContent = Math.floor(s.done * 100 / s.total) + " %";
        setTimeout(poll, 1500);
        return;
      }
      if (text) {
        var pct = s.total > 0 ? " · " + Math.floor(s.done * 100 / s.total) + "%" : "";
        text.textContent = s.phase + pct + (s.paused ? " (paused, on battery)" : "");
      }
      setTimeout(poll, 3000);
    }).catch(function () { setTimeout(poll, 10000); });
  }
  setTimeout(poll, 3000);
})();
