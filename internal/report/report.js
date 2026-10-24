// Draws the report from the JSON beside it. Every piece of logged text goes on the page as text, never as HTML.
(function () {
  "use strict";
  var data = JSON.parse(document.getElementById("data").textContent);
  var root = document.documentElement;
  var light = window.matchMedia && window.matchMedia("(prefers-color-scheme: light)").matches;
  root.dataset.theme = light ? "light" : "dark";
  document.getElementById("theme").onclick = function () {
    root.dataset.theme = root.dataset.theme === "dark" ? "light" : "dark";
  };
  document.getElementById("print").onclick = function () { window.print(); };

  function el(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined && text !== null) e.textContent = String(text);
    return e;
  }
  function add(parent) {
    for (var i = 1; i < arguments.length; i++) if (arguments[i]) parent.appendChild(arguments[i]);
    return parent;
  }
  // Times arrive already in the log's own zone; show them as written rather than in the reader's.
  function when(s) { return s ? s.slice(0, 10) + " " + s.slice(11, 19) : ""; }
  function badge(status) { return el("span", "badge s-" + status, status); }
  function n(v) { return (v || 0).toLocaleString(); }
  function section(title, body) {
    var s = el("section");
    add(s, el("h2", "", title), body);
    return s;
  }
  function table(headers, rows) {
    var wrap = el("div", "panel"), t = el("table"), head = el("tr");
    headers.forEach(function (h) {
      var th = el("th", h.num ? "num" : "", h.title || h);
      head.appendChild(th);
    });
    add(t, add(el("thead"), head));
    var body = el("tbody");
    rows.forEach(function (cells) {
      var tr = el("tr");
      cells.forEach(function (c, i) {
        var td = el("td", headers[i].num ? "num" : headers[i].cls || "");
        if (c instanceof Node) td.appendChild(c); else td.textContent = c === undefined || c === null ? "" : String(c);
        tr.appendChild(td);
      });
      body.appendChild(tr);
    });
    add(wrap, add(t, body));
    return wrap;
  }

  var app = document.getElementById("app");
  app.textContent = "";
  document.getElementById("period").textContent = when(data.since) + " to " + when(data.until) + " (" + data.timezone + ")";

  var meta = el("div", "meta");
  (data.sources || []).forEach(function (s) {
    add(meta, add(el("span"), el("b", "", s.name), document.createTextNode(", " + n(s.lines) + " lines" + (s.unparsed ? " (" + n(s.unparsed) + " not syslog)" : ""))));
  });
  if (data.search) add(meta, add(el("span"), document.createTextNode("Search "), el("code", "", data.search)));
  add(meta, el("span", "", "Made " + when(data.generated_at) + " by Mailinois " + data.version));
  app.appendChild(meta);

  var d = data.summary || {};
  var stats = el("div", "stats");
  [["Received", d.received], ["Delivered", d.sent], ["Deferrals", d.deferrals], ["Bounced", d.bounced],
    ["Expired", d.expired], ["Rejected", d.rejected], ["Still queued", d.queue]].forEach(function (s) {
    add(stats, add(el("div", "stat"), el("span", "", s[0]), el("strong", "", n(s[1]))));
  });
  if (d.p50 != null) add(stats, add(el("div", "stat"), el("span", "", "Delivery delay"), el("strong", "", d.p50.toFixed(1) + "s"), el("span", "sub", "median; p95 " + (d.p95 || 0).toFixed(1) + "s")));
  app.appendChild(section("Summary", stats));

  var buckets = d.buckets || [];
  if (buckets.length) {
    var ns = "http://www.w3.org/2000/svg", w = 1000, h = 120, bw = w / buckets.length;
    var max = Math.max.apply(null, buckets.map(function (b) { return b.sent + b.deferred + b.bounced + b.rejected; }).concat([1]));
    var svg = document.createElementNS(ns, "svg");
    svg.setAttribute("viewBox", "0 0 " + w + " " + h);
    svg.setAttribute("preserveAspectRatio", "none");
    svg.setAttribute("role", "img");
    svg.setAttribute("aria-label", "Outcomes over the period");
    var colours = { sent: "var(--sent)", deferred: "var(--deferred)", bounced: "var(--bounced)", rejected: "var(--rejected)" };
    buckets.forEach(function (b, i) {
      var y = h;
      ["sent", "deferred", "bounced", "rejected"].forEach(function (k) {
        if (!b[k]) return;
        var bh = b[k] / max * (h - 4), r = document.createElementNS(ns, "rect");
        y -= bh;
        r.setAttribute("x", i * bw + 1); r.setAttribute("y", y); r.setAttribute("width", Math.max(bw - 2, 1)); r.setAttribute("height", bh);
        r.setAttribute("fill", colours[k]);
        var title = document.createElementNS(ns, "title");
        title.textContent = when(b.at) + ": " + b[k] + " " + k;
        r.appendChild(title);
        svg.appendChild(r);
      });
    });
    var legend = el("div", "legend");
    ["sent", "deferred", "bounced", "rejected"].forEach(function (k) { add(legend, add(el("span"), el("i", "bg-" + k), document.createTextNode(k))); });
    app.appendChild(section("Outcomes over time", add(el("div", "panel chart"), svg, legend)));
  }

  if ((data.deferral_reasons || []).length) {
    app.appendChild(section("Why mail waited", table(
      ["Provider", "Code", { title: "Deferrals", num: true }, "Latest", { title: "Latest answer", cls: "wrap" }],
      data.deferral_reasons.map(function (r) { return [r.provider, el("code", "", r.dsn), n(r.count), when(r.last), r.example]; }))));
  }
  var providers = (data.providers || []).filter(function (p) { return p.attempts > 0; });
  if (providers.length) {
    app.appendChild(section("Providers", table(
      ["Provider", { title: "Attempts", num: true }, { title: "Delivered", num: true }, { title: "Deferred", num: true }, { title: "Failed", num: true }],
      providers.map(function (p) { return [p.provider, n(p.attempts), n(p.sent), n(p.deferred), n(p.failed)]; }))));
  }
  if ((data.still_queued || []).length) {
    app.appendChild(section("Still queued when the log ends", table(
      ["Received", "Server", "Queue ID", "Sender", { title: "Attempts", num: true }, { title: "Latest answer", cls: "wrap" }],
      data.still_queued.map(function (q) { return [when(q.received_at), q.server, el("code", "", q.queue_id), q.sender, n(q.attempts), q.response]; }))));
  }

  // Messages: a filter box and status chips over the list; each row opens to its recipients and its log lines.
  var messages = data.messages || [];
  var list = el("div", "panel");
  var counts = {}, rows = messages.map(function (m) {
    counts[m.status] = (counts[m.status] || 0) + 1;
    var rcpts = m.recipients || [], first = rcpts[0] || {};
    var text = [m.queue_id, m.sender, m.message_id, m.server, m.client_host, m.client_ip, m.status].concat(
      rcpts.map(function (r) { return r.address + " " + r.response + " " + r.dsn; }),
      (m.events || []).map(function (e) { return e.raw; })).join("\n").toLowerCase();
    var det = el("details", "msg"), sum = el("summary");
    var to = (first.address || "(no recipient)") + (rcpts.length > 1 ? " +" + (rcpts.length - 1) : "");
    add(sum, el("span", "cell mono", when(m.received_at)), el("span", "cell hide-narrow", m.server), el("span", "cell mono hide-narrow", m.queue_id),
      el("span", "cell hide-narrow", m.sender || "(no sender)"), el("span", "cell", to), add(el("span", "cell"), badge(m.status)));
    det.appendChild(sum);
    det.addEventListener("toggle", function () { if (det.open && det.childNodes.length === 1) det.appendChild(body(m)); });
    list.appendChild(det);
    return { m: m, el: det, text: text };
  });

  function body(m) {
    var b = el("div", "body"), facts = el("dl", "facts");
    [["Server", m.server], ["Queue ID", m.queue_id], ["From", m.sender], ["Message-ID", m.message_id],
      ["Client", [m.client_host, m.client_ip].filter(Boolean).join(" ")], ["SASL user", m.sasl_user],
      ["Size", m.size ? n(m.size) + " bytes" : ""], ["Removed", when(m.removed_at)]].forEach(function (f) {
      if (f[1]) add(facts, el("dt", "", f[0]), el("dd", "", f[1]));
    });
    b.appendChild(facts);
    if ((m.recipients || []).length) {
      b.appendChild(table(["Recipient", "Status", "Code", "Relay", { title: "Attempts", num: true }, { title: "Answer", cls: "wrap" }],
        m.recipients.map(function (r) { return [r.address, badge(r.status), el("code", "", r.dsn), r.relay, n(r.attempts), r.response]; })));
    }
    var log = (m.events || []).map(function (e) { return when(e.at) + "  " + e.program + ": " + e.raw; }).join("\n");
    if (log) b.appendChild(el("pre", "log", log));
    return b;
  }

  var controls = el("div", "controls"), input = el("input");
  input.type = "search";
  input.placeholder = "Filter: an address, queue ID, Message-ID, code or any text in the log";
  input.setAttribute("aria-label", "Filter messages");
  controls.appendChild(input);
  var chosen = "";
  var chips = [["", "All", messages.length]].concat(Object.keys(counts).sort().map(function (s) { return [s, s, counts[s]]; })).map(function (c) {
    var bt = el("button", "chip", c[1] + " " + c[2]);
    bt.type = "button";
    bt.setAttribute("aria-pressed", c[0] === chosen ? "true" : "false");
    bt.onclick = function () {
      chosen = c[0];
      chips.forEach(function (x) { x.setAttribute("aria-pressed", x === bt ? "true" : "false"); });
      apply();
    };
    controls.appendChild(bt);
    return bt;
  });
  var shown = el("p", "note");
  function apply() {
    var q = input.value.trim().toLowerCase(), k = 0;
    rows.forEach(function (r) {
      var ok = (!chosen || r.m.status === chosen) && (!q || r.text.indexOf(q) >= 0);
      r.el.hidden = !ok;
      if (ok) k++;
    });
    shown.textContent = k + " of " + messages.length + " messages" + (data.more ? ", the newest that matched; narrow the search or raise -limit for more" : "");
  }
  input.addEventListener("input", apply);
  var initial = decodeURIComponent((location.hash.match(/[#&]q=([^&]*)/) || [])[1] || "");
  if (initial) input.value = initial;
  var msgSection = section("Messages", controls);
  msgSection.appendChild(messages.length ? list : el("p", "empty", "No messages matched."));
  msgSection.appendChild(shown);
  app.appendChild(msgSection);
  apply();
  // A narrow report (one customer's mail) opens straight onto the stories.
  if (rows.length <= 5) rows.forEach(function (r) { r.el.appendChild(body(r.m)); r.el.open = true; });

  // Printing opens every message, so the PDF has the whole story; closing again afterwards.
  var opened = [];
  window.addEventListener("beforeprint", function () {
    rows.forEach(function (r) {
      if (r.el.hidden || r.el.open) return;
      if (r.el.childNodes.length === 1) r.el.appendChild(body(r.m)); // toggle fires too late for the print
      r.el.open = true;
      opened.push(r.el);
    });
  });
  window.addEventListener("afterprint", function () { opened.forEach(function (e) { e.open = false; }); opened = []; });

  var foot = el("footer", "", "Times are as the log wrote them, read as " + data.timezone + ". Made offline by Mailinois " + data.version + "; this file loads nothing from anywhere.");
  document.body.appendChild(foot);
})();
