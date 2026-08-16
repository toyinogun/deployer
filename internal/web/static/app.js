// The one script. Everything it does is an improvement on a page that already
// works without it: with JavaScript off, every page renders and every form
// submits, and only live polling and copy to clipboard are lost (AC-30).

(function () {
  "use strict";

  // How often the status region is refetched, and the ceiling after which it
  // stops regardless. The ceiling is a client behaviour only, stated knowingly:
  // the fragment is a session gated read, so a client ignoring it costs one
  // indexed query (AC-16).
  var POLL_MS = 3000;
  var CEILING_MS = 15 * 60 * 1000;

  // live reads the marker the status fragment carries. It is inside the swapped
  // content on purpose, so each poll brings a fresh answer with it.
  function live(region) {
    var marker = region.querySelector("[data-live]");
    return !!marker && marker.dataset.live === "on";
  }

  function startPolling() {
    var region = document.getElementById("status");
    if (!region || !live(region)) {
      return;
    }
    var slug = region.dataset.slug;
    if (!slug) {
      return;
    }
    var url = "/apps/" + encodeURIComponent(slug) + "?partial=status";
    var stopAt = Date.now() + CEILING_MS;

    var timer = setInterval(function () {
      if (Date.now() > stopAt) {
        clearInterval(timer);
        return;
      }
      fetch(url, {
        headers: { "Accept": "text/html" },
        credentials: "same-origin",
        // A redirect is the session having gone underneath the poll. Following
        // it would swap the sign in page into the status region; this makes it
        // an ordinary non ok response instead, which stops polling quietly.
        redirect: "manual",
      })
        .then(function (res) {
          if (!res.ok) {
            // The session expired, or the app is gone. Stop and leave the page
            // exactly as it stands, so the next navigation lands on sign in
            // rather than the page breaking visibly.
            clearInterval(timer);
            return null;
          }
          return res.text();
        })
        .then(function (html) {
          if (html === null || html === undefined) {
            return;
          }
          region.innerHTML = html;
          // The server decides when this is over, from the same state machine
          // the page rendered from. The client never guesses at terminal: it
          // reads the marker the fragment just brought with it.
          if (!live(region)) {
            clearInterval(timer);
          }
        })
        .catch(function () {
          clearInterval(timer);
        });
    }, POLL_MS);
  }

  // Copy to clipboard for the one time token panel. The value is read from the
  // element already on the page, so the script never holds it anywhere else.
  function wireCopy() {
    document.addEventListener("click", function (event) {
      var button = event.target.closest("[data-copy]");
      if (!button || !navigator.clipboard) {
        return;
      }
      var source = document.getElementById(button.dataset.copy);
      if (!source) {
        return;
      }
      navigator.clipboard.writeText(source.textContent.trim()).then(
        function () {
          var was = button.textContent;
          button.textContent = "Copied";
          setTimeout(function () {
            button.textContent = was;
          }, 2000);
        },
        function () {
          button.textContent = "Press Ctrl or Cmd and C";
        }
      );
    });
  }

  // Load more appends the next page onto the table rather than replacing it
  // (AC-14). The control stays an ordinary link to the same cursor URL, so with
  // this script off it still works, it just takes you to the next page instead
  // of growing this one (AC-30). One handler serves both paged tables, the app
  // list and the releases list, because they share this markup.
  function wireLoadMore() {
    document.addEventListener("click", function (event) {
      var link = event.target.closest(".load-more a");
      // A modified click is a request to open the page elsewhere. Let it.
      if (!link || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) {
        return;
      }
      var body = document.querySelector("table.rows tbody");
      if (!body) {
        return;
      }
      event.preventDefault();
      if (link.dataset.loading === "on") {
        return;
      }
      link.dataset.loading = "on";
      link.setAttribute("aria-busy", "true");

      fetch(link.href, {
        headers: { "Accept": "text/html" },
        credentials: "same-origin",
        // Same reasoning as the status poll: a redirect is the session having
        // gone underneath this, and following it would append the sign in page
        // into the table.
        redirect: "manual",
      })
        .then(function (res) {
          return res.ok ? res.text() : null;
        })
        .then(function (html) {
          link.dataset.loading = "";
          link.removeAttribute("aria-busy");
          if (!html) {
            // Leave the page exactly as it stands and leave the link alone, so
            // clicking again navigates and lands on sign in.
            return;
          }
          var doc = new DOMParser().parseFromString(html, "text/html");
          var rows = doc.querySelectorAll("table.rows tbody tr");
          if (!rows.length) {
            return;
          }
          var arrived = null;
          rows.forEach(function (row) {
            var added = body.appendChild(document.importNode(row, true));
            if (!arrived) {
              arrived = added;
            }
          });
          var next = doc.querySelector(".load-more a");
          if (next) {
            // Updating the href in place keeps focus where the person left it.
            link.href = next.getAttribute("href");
            return;
          }
          // That was the last page. The control goes, so focus moves to the
          // first row that just arrived rather than to a removed button.
          var landing = arrived.querySelector("a");
          link.parentNode.remove();
          if (landing) {
            landing.focus();
          }
        })
        .catch(function () {
          link.dataset.loading = "";
          link.removeAttribute("aria-busy");
        });
    });
  }

  // The client tabs on the connect page. Without this script every panel is
  // shown and the strip is hidden by the page's own noscript rule, so the page
  // is four stacked blocks rather than a blank one (AC-14).
  function wireConnectTabs() {
    var tabs = document.querySelectorAll("[data-connect-tab]");
    if (!tabs.length) {
      return;
    }
    tabs.forEach(function (tab) {
      tab.addEventListener("click", function () {
        tabs.forEach(function (other) {
          var chosen = other === tab;
          other.setAttribute("aria-selected", chosen ? "true" : "false");
          var panel = document.getElementById("panel-" + other.dataset.connectTab);
          if (panel) {
            panel.hidden = !chosen;
          }
        });
      });
    });
  }

  // The sidebar toggle below the breakpoint. Without this script the sidebar is
  // simply always open, which is why the markup starts unhidden.
  function wireNav() {
    var toggle = document.querySelector(".nav-toggle");
    var sidebar = document.getElementById("sidebar");
    if (!toggle || !sidebar) {
      return;
    }
    var narrow = window.matchMedia("(max-width: 860px)");

    function apply() {
      if (narrow.matches) {
        sidebar.hidden = toggle.getAttribute("aria-expanded") !== "true";
      } else {
        sidebar.hidden = false;
      }
    }
    toggle.addEventListener("click", function () {
      toggle.setAttribute("aria-expanded", toggle.getAttribute("aria-expanded") === "true" ? "false" : "true");
      apply();
    });
    narrow.addEventListener("change", apply);
    apply();
  }

  // The disable confirmation: the button stays disabled until the typed address
  // matches. The server checks it too, and that check is the one that holds;
  // this only saves a person the round trip.
  function wireConfirm() {
    document.querySelectorAll("[data-confirm]").forEach(function (input) {
      var form = input.form;
      if (!form) {
        return;
      }
      var button = form.querySelector("button[type=submit]");
      if (!button) {
        return;
      }
      function check() {
        button.disabled = input.value.trim().toLowerCase() !== input.dataset.confirm.toLowerCase();
      }
      input.addEventListener("input", check);
      check();
    });
  }

  startPolling();
  wireLoadMore();
  wireCopy();
  wireConnectTabs();
  wireNav();
  wireConfirm();
})();
