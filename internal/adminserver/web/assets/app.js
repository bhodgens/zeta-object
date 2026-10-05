/*
 * zeta-object admin console - shell behaviour.
 * Hand-written, dependency-free. Defines window.Console, the shared entry
 * point for the shell and the per-screen modules attached by leaf 04.
 *
 * Contract 2/3 notes:
 *  - CSRF: the session carries a CSRF token; the UI echoes it in an
 *    X-CSRF-Token header on every mutating request. We read it from a
 *    <meta name="csrf-token"> when the server fills it, and fall back to a
 *    non-HttpOnly cookie named "zeta_csrf".
 *  - 401 from any api() call returns the operator to /login.
 */
(function () {
  "use strict";

  var CSRF_META = "csrf-token";
  var CSRF_COOKIE = "zeta_csrf";
  var THEME_KEY = "zeta-theme";
  var TOAST_MS = 5000;

  function byId(id) {
    return document.getElementById(id);
  }

  /* --- CSRF --------------------------------------------------------------- */

  function readCookie(name) {
    var parts = document.cookie ? document.cookie.split(";") : [];
    for (var i = 0; i < parts.length; i++) {
      var pair = parts[i].trim();
      if (pair.indexOf(name + "=") === 0) {
        return decodeURIComponent(pair.slice(name.length + 1));
      }
    }
    return "";
  }

  function csrfToken() {
    var meta = document.querySelector('meta[name="' + CSRF_META + '"]');
    if (meta && meta.content) {
      return meta.content;
    }
    return readCookie(CSRF_COOKIE);
  }

  /* --- escaping ----------------------------------------------------------- */

  function escapeHtml(value) {
    if (value === null || value === undefined) {
      return "";
    }
    return String(value)
      .replace(/&/g, "&amp;")
      .replace(/</g, "&lt;")
      .replace(/>/g, "&gt;")
      .replace(/"/g, "&quot;")
      .replace(/'/g, "&#39;");
  }

  /* --- theme -------------------------------------------------------------- */

  function safeStorage() {
    try {
      return window.localStorage;
    } catch (err) {
      return null;
    }
  }

  function getTheme() {
    return document.documentElement.getAttribute("data-theme") === "light"
      ? "light"
      : "dark";
  }

  function applyTheme(theme, persist) {
    var next = theme === "light" ? "light" : "dark";
    document.documentElement.setAttribute("data-theme", next);
    var toggle = byId("theme-toggle");
    if (toggle) {
      var isLight = next === "light";
      toggle.setAttribute("aria-pressed", isLight ? "true" : "false");
      var label = toggle.querySelector("[data-theme-label]");
      if (label) {
        label.textContent = isLight ? "Dark theme" : "Light theme";
      }
    }
    if (persist) {
      var store = safeStorage();
      if (store) {
        try {
          store.setItem(THEME_KEY, next);
        } catch (err) {
          /* storage full or blocked - theme still applies for this session */
        }
      }
    }
  }

  function setTheme(theme) {
    applyTheme(theme, true);
  }

  function initTheme() {
    var stored = null;
    var store = safeStorage();
    if (store) {
      try {
        stored = store.getItem(THEME_KEY);
      } catch (err) {
        stored = null;
      }
    }
    if (stored === "light" || stored === "dark") {
      applyTheme(stored, false);
    } else {
      applyTheme("dark", false);
    }
  }

  /* --- api ---------------------------------------------------------------- */

  function redirectToLogin() {
    if (window.location.pathname !== "/login") {
      window.location.assign("/login");
    }
  }

  function isMutating(method) {
    var m = (method || "GET").toUpperCase();
    return m !== "GET" && m !== "HEAD" && m !== "OPTIONS";
  }

  function api(method, path, body) {
    var headers = { Accept: "application/json" };
    var opts = {
      method: (method || "GET").toUpperCase(),
      headers: headers,
      credentials: "same-origin"
    };
    if (body !== undefined && body !== null) {
      headers["Content-Type"] = "application/json";
      opts.body = JSON.stringify(body);
    }
    if (isMutating(opts.method)) {
      headers["X-CSRF-Token"] = csrfToken();
    }

    return fetch(path, opts).then(function (response) {
      if (response.status === 401) {
        redirectToLogin();
        var unauth = new Error("Session expired");
        unauth.status = 401;
        throw unauth;
      }
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
          var message = "Request failed (" + response.status + ")";
          if (data && typeof data === "object" && data.error) {
            message = data.error;
          } else if (typeof data === "string" && data) {
            message = data;
          }
          var failure = new Error(message);
          failure.status = response.status;
          failure.data = data;
          throw failure;
        }
        return data;
      });
    });
  }

  /* --- toast -------------------------------------------------------------- */

  function toastClass(kind) {
    switch (kind) {
      case "ok":
      case "success":
        return "toast-ok";
      case "warn":
        return "toast-warn";
      case "error":
      case "danger":
        return "toast-error";
      default:
        return "toast-info";
    }
  }

  function removeToast(el) {
    if (!el || !el.parentNode) {
      return;
    }
    el.classList.add("is-leaving");
    window.setTimeout(function () {
      if (el.parentNode) {
        el.parentNode.removeChild(el);
      }
    }, 200);
  }

  function toast(kind, message) {
    var region = byId("toast");
    if (!region) {
      return;
    }
    var el = document.createElement("div");
    el.className = "toast " + toastClass(kind);
    el.setAttribute("role", kind === "error" || kind === "danger" ? "alert" : "status");
    el.textContent = message === null || message === undefined ? "" : String(message);
    el.addEventListener("click", function () {
      removeToast(el);
    });
    region.appendChild(el);
    window.setTimeout(function () {
      removeToast(el);
    }, TOAST_MS);
  }

  /* --- modal -------------------------------------------------------------- */

  var modalState = null;

  function closeModal(result) {
    if (!modalState) {
      return;
    }
    var modal = byId("modal");
    var state = modalState;
    modalState = null;
    if (modal) {
      modal.hidden = true;
    }
    state.confirm.removeEventListener("click", state.onConfirm);
    state.cancel.removeEventListener("click", state.onCancel);
    state.backdrop.forEach(function (node) {
      node.removeEventListener("click", state.onCancel);
    });
    document.removeEventListener("keydown", state.onKeydown, true);
    if (state.previousFocus && typeof state.previousFocus.focus === "function") {
      state.previousFocus.focus();
    }
    state.resolve(result);
  }

  function writeModalBody(host, body) {
    host.textContent = "";
    if (body instanceof Node) {
      host.appendChild(body);
    } else {
      host.textContent = body === null || body === undefined ? "" : String(body);
    }
  }

  function modal(title, body) {
    var modalEl = byId("modal");
    var titleEl = byId("modal-title");
    var bodyEl = byId("modal-body");
    var confirmBtn = byId("modal-confirm");
    var cancelBtn = byId("modal-cancel");
    var backdrop = modalEl ? modalEl.querySelectorAll("[data-modal-dismiss]") : [];

    if (!modalEl || !titleEl || !bodyEl || !confirmBtn || !cancelBtn) {
      return Promise.resolve(false);
    }

    if (modalState) {
      closeModal(false);
    }

    titleEl.textContent = title === null || title === undefined ? "" : String(title);
    writeModalBody(bodyEl, body);

    return new Promise(function (resolve) {
      var state = { resolve: resolve, previousFocus: document.activeElement };

      state.onConfirm = function () {
        closeModal(true);
      };
      state.onCancel = function () {
        closeModal(false);
      };
      state.onKeydown = function (event) {
        if (event.key === "Escape") {
          event.preventDefault();
          closeModal(false);
        }
      };

      state.confirm = confirmBtn;
      state.cancel = cancelBtn;
      state.backdrop = Array.prototype.slice.call(backdrop);
      modalState = state;

      confirmBtn.addEventListener("click", state.onConfirm);
      cancelBtn.addEventListener("click", state.onCancel);
      state.backdrop.forEach(function (node) {
        node.addEventListener("click", state.onCancel);
      });
      document.addEventListener("keydown", state.onKeydown, true);

      modalEl.hidden = false;
      confirmBtn.focus();
    });
  }

  /* --- render / screens --------------------------------------------------- */

  var screens = {};

  var DEFAULT_TITLE = {
    dashboard: "Dashboard",
    config: "Configuration",
    buckets: "Buckets",
    danger: "Danger zone"
  };

  function findModule(screen) {
    return Object.prototype.hasOwnProperty.call(screens, screen) ? screens[screen] : null;
  }

  function emptyState(view, screen) {
    view.innerHTML =
      '<div class="state">' +
      '<div class="state-title">' +
      escapeHtml(DEFAULT_TITLE[screen] || "Screen") +
      "</div>" +
      '<div>Nothing to show yet.</div>' +
      "</div>";
  }

  function errorState(view, screen, message) {
    view.innerHTML =
      '<div class="state state-error">' +
      '<div class="state-title">' +
      escapeHtml(DEFAULT_TITLE[screen] || "Screen") +
      " failed to load</div>" +
      "<div>" +
      escapeHtml(message || "Unknown error") +
      "</div>" +
      "</div>";
  }

  function render(screen) {
    if (!screen) {
      return;
    }
    document.documentElement.setAttribute("data-screen", screen);

    var tabs = document.querySelectorAll("#rail .tab");
    Array.prototype.forEach.call(tabs, function (tab) {
      var active = tab.getAttribute("data-screen") === screen;
      tab.classList.toggle("active", active);
      if (active) {
        tab.setAttribute("aria-current", "page");
        tab.setAttribute("aria-selected", "true");
        tab.setAttribute("tabindex", "0");
      } else {
        tab.removeAttribute("aria-current");
        tab.setAttribute("aria-selected", "false");
        tab.setAttribute("tabindex", "-1");
      }
    });

    var titleEl = byId("view-title");
    if (titleEl && DEFAULT_TITLE[screen]) {
      titleEl.textContent = DEFAULT_TITLE[screen];
    }

    var view = byId("view");
    if (!view) {
      return;
    }
    view.textContent = "";

    var module = findModule(screen);
    if (!module) {
      emptyState(view, screen);
      return;
    }
    try {
      if (typeof module === "function") {
        module(view);
      } else if (typeof module.render === "function") {
        module.render(view);
      } else {
        emptyState(view, screen);
      }
    } catch (err) {
      errorState(view, screen, err && err.message ? err.message : String(err));
    }
  }

  /* --- rail keyboard navigation ------------------------------------------ */

  function tabList() {
    return Array.prototype.slice.call(document.querySelectorAll("#rail .tab"));
  }

  function focusTabAt(index) {
    var tabs = tabList();
    if (!tabs.length) {
      return;
    }
    var i = (index + tabs.length) % tabs.length;
    tabs.forEach(function (tab, n) {
      tab.setAttribute("tabindex", n === i ? "0" : "-1");
    });
    tabs[i].focus();
  }

  function wireRail() {
    var tabs = tabList();
    tabs.forEach(function (tab, index) {
      tab.addEventListener("click", function () {
        render(tab.getAttribute("data-screen"));
      });
      tab.addEventListener("keydown", function (event) {
        switch (event.key) {
          case "ArrowDown":
          case "ArrowRight":
            event.preventDefault();
            focusTabAt(index + 1);
            break;
          case "ArrowUp":
          case "ArrowLeft":
            event.preventDefault();
            focusTabAt(index - 1);
            break;
          case "Home":
            event.preventDefault();
            focusTabAt(0);
            break;
          case "End":
            event.preventDefault();
            focusTabAt(tabs.length - 1);
            break;
          default:
            break;
        }
      });
    });
  }

  /* --- sign out ----------------------------------------------------------- */

  function signOut() {
    var headers = { Accept: "application/json" };
    headers["X-CSRF-Token"] = csrfToken();
    fetch("/logout", {
      method: "POST",
      headers: headers,
      credentials: "same-origin"
    })
      .then(
        function () {
          redirectAfterLogout();
        },
        function () {
          redirectAfterLogout();
        }
      );
  }

  function redirectAfterLogout() {
    window.location.assign("/login");
  }

  /* --- version ------------------------------------------------------------ */

  function setVersion(version) {
    Console.version = version;
    var railVersion = byId("app-version");
    if (railVersion) {
      railVersion.textContent = version;
    }
    var footVersion = byId("foot-version");
    if (footVersion) {
      footVersion.textContent = version;
    }
  }

  /* --- boot --------------------------------------------------------------- */

  function init() {
    initTheme();

    var toggle = byId("theme-toggle");
    if (toggle) {
      toggle.addEventListener("click", function () {
        setTheme(getTheme() === "light" ? "dark" : "light");
      });
    }

    var signout = byId("signout");
    if (signout) {
      signout.addEventListener("click", function () {
        modal("Sign out", "End this console session?").then(function (confirmed) {
          if (confirmed) {
            signOut();
          }
        });
      });
    }

    wireRail();

    var initial = document.documentElement.getAttribute("data-screen") || "dashboard";
    render(initial);
  }

  var Console = {
    version: "dev",
    screens: screens,
    render: render,
    toast: toast,
    modal: modal,
    api: api,
    escapeHtml: escapeHtml,
    theme: { get: getTheme, set: setTheme },
    setVersion: setVersion
  };

  window.Console = Console;

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
