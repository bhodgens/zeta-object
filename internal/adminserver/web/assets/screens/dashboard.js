/*
 * Dashboard screen (admin-server-2026-10, leaf 04 Task 1).
 *
 * Reads GET /api/status and renders exactly what the gateway reports:
 * version, uptime (already formatted by the gateway), listeners, frontends
 * (the admin frontend marked), backends, the restart-required banner, and the
 * metadata provider {available, reason}. Nothing is invented: a field the
 * gateway does not send renders as "unknown"; backend capabilities are not in
 * the payload and are shown as "unknown (not reported)".
 *
 * Plain module, no framework. Attaches window.Console.screens.dashboard with a
 * mount(view) function. Every failure renders the gateway's own message with a
 * retry control.
 */
(function () {
  "use strict";

  function Console() {
    return window.Console;
  }

  function el(tag, cls, text) {
    var node = document.createElement(tag);
    if (cls) {
      node.className = cls;
    }
    if (text !== undefined && text !== null) {
      node.textContent = String(text);
    }
    return node;
  }

  function unknown(value) {
    if (value === undefined || value === null || value === "") {
      return "unknown";
    }
    return String(value);
  }

  function gatewayMessage(err) {
    if (err && err.data) {
      var d = err.data;
      if (d.error) {
        if (typeof d.error === "string") {
          return d.error;
        }
        if (d.error.message) {
          return d.error.message;
        }
        return JSON.stringify(d.error);
      }
      if (typeof d === "string" && d) {
        return d;
      }
    }
    if (err && err.message) {
      return err.message;
    }
    return "Unknown error";
  }

  function loadingState(message) {
    var box = el("div", "state");
    box.appendChild(el("div", "spinner"));
    box.appendChild(el("div", null, message));
    return box;
  }

  function errorState(title, message, onRetry) {
    var box = el("div", "state state-error");
    box.appendChild(el("div", "state-title", title));
    box.appendChild(el("div", null, message));
    if (typeof onRetry === "function") {
      var retry = el("button", "btn", "Retry");
      retry.type = "button";
      retry.addEventListener("click", onRetry);
      box.appendChild(retry);
    }
    return box;
  }

  function metric(label, valueNode) {
    var m = el("div", "metric");
    m.appendChild(el("div", "metric-label", label));
    if (valueNode instanceof Node) {
      m.appendChild(valueNode);
    } else {
      m.appendChild(el("div", "metric-value", unknown(valueNode)));
    }
    return m;
  }

  function panel(title, headExtra) {
    var p = el("div", "panel");
    var head = el("div", "panel-head");
    head.appendChild(el("h2", "panel-title", title));
    if (headExtra) {
      head.appendChild(headExtra);
    }
    p.appendChild(head);
    return p;
  }

  function stringList(items) {
    var ul = el("ul");
    items.forEach(function (item) {
      ul.appendChild(el("li", null, item));
    });
    return ul;
  }

  function refreshButton(onClick) {
    var b = el("button", "btn", "Refresh");
    b.type = "button";
    b.addEventListener("click", onClick);
    return b;
  }

  function renderStatus(view, data, reload) {
    view.textContent = "";

    var restart = Array.isArray(data.restartRequired) ? data.restartRequired : [];
    if (restart.length) {
      var banner = el("div", "panel");
      banner.setAttribute("role", "status");
      var bHead = el("div", "panel-head");
      bHead.appendChild(el("h2", "panel-title", "Restart required"));
      bHead.appendChild(el("span", "badge badge-warn", "restart pending"));
      banner.appendChild(bHead);
      banner.appendChild(
        el("div", null, "These keys have changed and require a server restart: " + restart.join(", "))
      );
      view.appendChild(banner);
    }

    var overview = panel("Overview", refreshButton(reload));
    var grid = el("div", "grid");
    grid.appendChild(metric("Version", unknown(data.version)));
    grid.appendChild(metric("Uptime", unknown(data.uptime)));

    var mp = data.metadataProvider;
    if (mp && typeof mp === "object") {
      var meta = el("div", "metric");
      meta.appendChild(el("div", "metric-label", "Metadata provider"));
      var status = el(
        "span",
        mp.available ? "badge badge-ok" : "badge badge-danger",
        mp.available ? "available" : "unavailable"
      );
      meta.appendChild(status);
      meta.appendChild(el("div", "metric-value", unknown(mp.reason)));
      grid.appendChild(meta);
    } else {
      grid.appendChild(metric("Metadata provider", "unknown"));
    }
    overview.appendChild(grid);
    view.appendChild(overview);

    var listeners = panel("Listeners");
    var listenerList = Array.isArray(data.listeners) ? data.listeners : null;
    if (!listenerList || !listenerList.length) {
      listeners.appendChild(el("div", "state", "None reported."));
    } else {
      listeners.appendChild(stringList(listenerList.map(String)));
    }
    view.appendChild(listeners);

    var frontends = panel("Frontends");
    var frontendList = Array.isArray(data.frontends) ? data.frontends : null;
    if (!frontendList || !frontendList.length) {
      frontends.appendChild(el("div", "state", "None reported."));
    } else {
      var feUl = el("ul");
      frontendList.forEach(function (name) {
        var li = el("li");
        li.appendChild(el("span", "mono", String(name)));
        if (name === "admin") {
          li.appendChild(document.createTextNode(" "));
          li.appendChild(el("span", "badge badge-warn", "this console"));
        }
        feUl.appendChild(li);
      });
      frontends.appendChild(feUl);
    }
    view.appendChild(frontends);

    var backends = panel("Backends");
    var backendList = Array.isArray(data.backends) ? data.backends : null;
    if (!backendList || !backendList.length) {
      backends.appendChild(el("div", "state", "None reported."));
    } else {
      var table = el("table");
      var thead = el("thead");
      var hrow = el("tr");
      hrow.appendChild(el("th", null, "Backend"));
      hrow.appendChild(el("th", null, "Capabilities"));
      thead.appendChild(hrow);
      table.appendChild(thead);
      var tbody = el("tbody");
      backendList.forEach(function (name) {
        var row = el("tr");
        row.appendChild(el("td", "mono", String(name)));
        row.appendChild(el("td", null, "unknown (not reported)"));
        tbody.appendChild(row);
      });
      table.appendChild(tbody);
      backends.appendChild(table);
    }
    view.appendChild(backends);
  }

  function mount(view) {
    view.textContent = "";
    var loading = loadingState("Loading server status\u2026");
    view.appendChild(loading);

    function reload() {
      view.textContent = "";
      view.appendChild(loadingState("Loading server status\u2026"));
      Console()
        .api("GET", "/api/status")
        .then(function (data) {
          renderStatus(view, data || {}, reload);
        })
        .catch(function (err) {
          view.textContent = "";
          view.appendChild(errorState("Status request failed", gatewayMessage(err), reload));
        });
    }

    reload();
  }

  if (window.Console && window.Console.screens) {
    window.Console.screens.dashboard = mount;
  } else {
    document.addEventListener("DOMContentLoaded", function () {
      if (window.Console && window.Console.screens) {
        window.Console.screens.dashboard = mount;
      }
    });
  }
})();
