// Loaded in <head> so the saved theme apply before the page paints.
(function () {
  var root = document.documentElement;
  function saved(key, fallback) {
    try { return localStorage.getItem(key) || fallback; } catch (e) { return fallback; }
  }
  function save(key, value) {
    try { localStorage.setItem(key, value); } catch (e) { /* private window: it just won't stick */ }
  }
  var light = window.matchMedia && window.matchMedia("(prefers-color-scheme: light)").matches;
  root.dataset.theme = saved("theme", light ? "light" : "dark");

  document.addEventListener("DOMContentLoaded", function () {
    document.querySelectorAll('[data-toggle="theme"]').forEach(function (button) {
      button.addEventListener("click", function () {
        var next = root.dataset.theme === "dark" ? "light" : "dark";
        root.dataset.theme = next;
        save("theme", next);
      });
    });

    var search = document.getElementById("search");
    document.addEventListener("keydown", function (e) {
      var typing = /INPUT|TEXTAREA|SELECT/.test(document.activeElement.tagName);
      if (search && ((e.key === "k" && (e.metaKey || e.ctrlKey)) || (e.key === "/" && !typing))) {
        e.preventDefault();
        search.focus();
        search.select();
      }
    });

    document.querySelectorAll("[data-copy]").forEach(function (button) {
      button.addEventListener("click", function () {
        navigator.clipboard.writeText(button.dataset.copy).then(function () {
          var label = button.textContent;
          button.textContent = "Copied";
          setTimeout(function () { button.textContent = label; }, 1500);
        });
      });
    });

    var tail = document.getElementById("tail");
    if (tail) liveTail(tail);
  });

  // liveTail polls for new outcomes and puts them on top, as text only.
  function liveTail(list) {
    var paused = false;
    var pause = document.querySelector("[data-pause]");
    if (pause) pause.addEventListener("click", function () {
      paused = !paused;
      pause.textContent = paused ? "Resume" : "Pause";
    });
    function lastID() {
      var first = list.querySelector("li[data-id]");
      return first ? first.dataset.id : 0;
    }
    function el(tag, cls, text) {
      var e = document.createElement(tag);
      if (cls) e.className = cls;
      if (text !== undefined) e.textContent = text;
      return e;
    }
    function poll() {
      if (paused || document.hidden) return;
      var src = list.dataset.src + (list.dataset.src.indexOf("?") < 0 ? "?" : "&") + "after=" + lastID();
      fetch(src, { headers: { Accept: "application/json" } }).then(function (r) { return r.ok ? r.json() : { lines: [] }; }).then(function (data) {
        var lines = (data.lines || []).slice().reverse();
        if (lines.length) {
          var empty = list.querySelector(".empty");
          if (empty) empty.remove();
        }
        lines.forEach(function (l) {
          var li = el("li", "new");
          li.dataset.id = l.id;
          li.appendChild(el("span", "mono when", new Date(l.at).toLocaleTimeString([], { hour12: false })));
          li.appendChild(el("i", "dot s-" + l.status));
          var a = el("a");
          a.href = "/messages/" + l.message_id;
          a.appendChild(el("span", "to", l.recipient || "(no recipient)"));
          a.appendChild(el("span", "mono what", l.server + " · " + l.status + (l.dsn ? " " + l.dsn : "")));
          li.appendChild(a);
          list.insertBefore(li, list.firstChild);
        });
        while (list.children.length > 12) list.removeChild(list.lastChild);
      }).catch(function () {});
    }
    setInterval(poll, 3000);
  }
})();
