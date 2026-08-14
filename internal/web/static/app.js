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

  function startPolling() {
    var region = document.getElementById("status");
    if (!region || region.dataset.poll !== "on") {
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
          // the page rendered from. The client never guesses at terminal.
          if (region.dataset.poll !== "on") {
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
  wireCopy();
  wireNav();
  wireConfirm();
})();
