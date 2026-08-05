/* meshbbs site — progressive enhancement only.
   Every page reads fine with this file absent. */

(function () {
  "use strict";

  /* ── The hero packet diagram ──────────────────────────────
     Byte counts are Appendix B of docs/design.md, for a
     top-level post (no `parent` pointer). They sum to 233. */

  var SEGMENTS = [
    { n: 8,   c: "#3A4257", label: "L1 symbol header" },
    { n: 6,   c: "#3A4257", label: "L2 bundle header" },
    { n: 9,   c: "#48526B", label: "Origin table" },
    { n: 1,   c: "#48526B", label: "Origin index" },
    { n: 5,   c: "#48526B", label: "seq · ts · type" },
    { n: 64,  c: "#5E6B8A", label: "Ed25519 signature", note: "90% of per-record overhead" },
    { n: 140, c: "#F0A13C", label: "Compressed body", note: "≈ 490 characters", hot: true }
  ];

  function drawPacket() {
    var grid = document.getElementById("packet");
    if (!grid) return;

    var frag = document.createDocumentFragment();
    var i = 0;

    SEGMENTS.forEach(function (s) {
      for (var k = 0; k < s.n; k++) {
        var cell = document.createElement("i");
        cell.style.setProperty("--seg", s.c);
        cell.style.setProperty("--d", (i * 3.4) + "ms");
        frag.appendChild(cell);
        i++;
      }
    });
    grid.appendChild(frag);

    var legend = document.querySelector("[data-packet-legend]");
    if (!legend) return;

    legend.innerHTML = SEGMENTS.map(function (s) {
      return '<div' + (s.hot ? ' class="hot"' : '') + '>'
           +   '<b style="background:' + s.c + '"></b>'
           +   '<span>' + s.label + (s.note ? ' <em>— ' + s.note + '</em>' : '') + '</span>'
           +   '<span>' + s.n + ' B</span>'
           + '</div>';
    }).join("");
  }

  /* ── Tabbed terminal panes ─────────────────────────────── */

  function wireTabs(bar) {
    var scope = bar.closest(".term");
    if (!scope) return;

    bar.addEventListener("click", function (e) {
      var btn = e.target.closest("button");
      if (!btn) return;
      select(btn);
    });

    bar.addEventListener("keydown", function (e) {
      if (e.key !== "ArrowLeft" && e.key !== "ArrowRight") return;
      var tabs = Array.prototype.slice.call(bar.querySelectorAll("button"));
      var at = tabs.indexOf(document.activeElement);
      if (at === -1) return;
      e.preventDefault();
      var to = tabs[(at + (e.key === "ArrowRight" ? 1 : tabs.length - 1)) % tabs.length];
      to.focus();
      select(to);
    });

    function select(btn) {
      bar.querySelectorAll("button").forEach(function (b) {
        b.setAttribute("aria-selected", String(b === btn));
      });
      scope.querySelectorAll("[data-panel]").forEach(function (p) {
        p.hidden = p.dataset.panel !== btn.dataset.tab;
      });
    }
  }

  /* ── Mark the current page in the nav ──────────────────── */

  function markCurrentPage() {
    var here = location.pathname.split("/").pop() || "index.html";
    document.querySelectorAll(".nav a[href]").forEach(function (a) {
      if (a.getAttribute("href") === here) a.setAttribute("aria-current", "page");
    });
  }

  document.addEventListener("DOMContentLoaded", function () {
    drawPacket();
    document.querySelectorAll(".term-bar").forEach(wireTabs);
    markCurrentPage();
  });
})();
