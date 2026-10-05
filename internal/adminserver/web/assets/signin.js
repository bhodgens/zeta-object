/*
 * Sign-in behaviour (admin-server-2026-10, leaf 04 Task 4).
 *
 * POSTs the operator token to /login and, on success, loads the shell. It also
 * installs a session guard: a 401 from any later /api/ call shows a toast and
 * lets the shell's own api() helper return the operator to /login (the shell
 * redirects; this module only supplies the toast, because it wraps window.fetch
 * before app.js runs).
 *
 * If the sign-in form is present (the /login page), the form is wired to
 * signIn() and the gateway's message is shown verbatim on failure. On the shell
 * page there is no form and the wiring is a no-op.
 *
 * Plain module, no framework. Must be loaded before app.js.
 */
(function () {
  "use strict";

  function envelopeMessage(data, fallback) {
    if (data && data.error) {
      if (typeof data.error === "string" && data.error) {
        return data.error;
      }
      if (data.error.message) {
        return data.error.message;
      }
    }
    if (typeof data === "string" && data) {
      return data;
    }
    return fallback;
  }

  function signIn(token) {
    return fetch("/login", {
      method: "POST",
      headers: { "Content-Type": "application/json", Accept: "application/json" },
      credentials: "same-origin",
      body: JSON.stringify({ token: token })
    }).then(function (response) {
      return response.text().then(function (text) {
        var data = null;
        if (text) {
          try {
            data = JSON.parse(text);
          } catch (err) {
            data = text;
          }
        }
        if (!response.ok) {
          var message = envelopeMessage(data, "Sign in failed (" + response.status + ")");
          var failure = new Error(message);
          failure.status = response.status;
          failure.data = data;
          throw failure;
        }
        return data || {};
      });
    });
  }

  function loadShell() {
    window.location.assign("/");
  }

  function doSignIn(token) {
    return signIn(token).then(function () {
      loadShell();
    });
  }

  function installSessionGuard() {
    if (window.__zetaSessionGuard || typeof window.fetch !== "function") {
      return;
    }
    window.__zetaSessionGuard = true;
    var nativeFetch = window.fetch.bind(window);
    window.fetch = function (input, init) {
      return nativeFetch(input, init).then(function (response) {
        var url = typeof input === "string" ? input : (input && input.url) || "";
        if (response.status === 401 && url.indexOf("/api/") !== -1) {
          var C = window.Console;
          if (C && typeof C.toast === "function") {
            C.toast("warn", "Session expired \u2014 sign in again.");
          }
        }
        return response;
      });
    };
  }

  function wireLoginForm() {
    var form = document.getElementById("login-form");
    var field = document.getElementById("token");
    var message = document.getElementById("login-message");
    if (!form || !field || !message) {
      return;
    }
    function setMessage(text, kind) {
      message.textContent = text || "";
      message.className = "login-message" + (kind ? " is-" + kind : "");
    }
    form.addEventListener("submit", function (event) {
      event.preventDefault();
      var token = field.value;
      if (!token) {
        setMessage("Enter the operator token.", "error");
        field.focus();
        return;
      }
      setMessage("Signing in\u2026", "");
      var submitBtn = form.querySelector("button[type=submit]");
      if (submitBtn) {
        submitBtn.disabled = true;
      }
      doSignIn(token)
        .catch(function (err) {
          setMessage((err && err.message) || "Sign in failed.", "error");
        })
        .then(function () {
          if (submitBtn) {
            submitBtn.disabled = false;
          }
        });
    });
  }

  function expose() {
    if (window.Console) {
      window.Console.signin = { signIn: doSignIn, login: doSignIn };
    }
  }

  wireLoginForm();
  installSessionGuard();

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", expose);
  } else {
    expose();
  }
})();
