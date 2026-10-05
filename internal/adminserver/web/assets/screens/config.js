/*
 * Configuration screen (admin-server-2026-10, leaf 04 Task 2).
 *
 * Loads GET /api/config, renders the effective configuration as grouped
 * fields. Every secret is read-only and visibly masked. The hot-apply keys
 * (region, zfs_versioning, zfs_versioning_reflink_retention) are editable;
 * every other key is read-only with an explanation (changing it needs a
 * restart). Apply sends a partial patch to PUT /api/config and shows the
 * returned `applied` list as a success toast and the `restartRequired` list as
 * a persistent notice. Save to file -> POST /api/config/save. Reload
 * identities -> POST /api/auth/reload. An invalid patch surfaces the gateway's
 * validator message and never looks applied.
 *
 * Plain module, no framework. Attaches window.Console.screens.config.
 */
(function () {
  "use strict";

  var EDITABLE = {
    region: "text",
    zfs_versioning: "select",
    zfs_versioning_reflink_retention: "number"
  };

  var ZFS_VERSIONING_OPTIONS = ["snapshots", "sidecar", "reflink", "both"];

  var GROUPS = [
    { title: "Server", keys: ["dataDir", "listenAddr", "certFile", "keyFile"] },
    { title: "Storage", keys: ["buckets"] },
    { title: "Backends", keys: ["backends"] },
    { title: "Frontends", keys: ["frontends"] },
    { title: "Identity", keys: ["identities", "auth"] },
    {
      title: "ZFS metadata",
      keys: [
        "region",
        "zfs_versioning",
        "zfs_versioning_reflink_retention",
        "zfs_bucket_datasets",
        "zfs_binary",
        "zmetad_db_path",
        "zmetad_binary"
      ]
    },
    { title: "Audit", keys: ["auditLog"] }
  ];

  var MASK = "********";

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

  function isSecretName(name) {
    if (typeof name !== "string") {
      return false;
    }
    return /secret|password|passphrase|token|credential/i.test(name);
  }

  // maskForDisplay walks a value and replaces every secret-named leaf with the
  // mask literal, so a secret the gateway sends unmasked is never shown.
  function maskForDisplay(value, keyName) {
    if (isSecretName(keyName)) {
      return MASK;
    }
    if (Array.isArray(value)) {
      return value.map(function (item) {
        return maskForDisplay(item, null);
      });
    }
    if (value && typeof value === "object") {
      var out = {};
      Object.keys(value).forEach(function (k) {
        out[k] = maskForDisplay(value[k], k);
      });
      return out;
    }
    return value;
  }

  function displayValue(value, keyName) {
    if (isSecretName(keyName) || value === MASK) {
      return MASK;
    }
    if (value && typeof value === "object") {
      return JSON.stringify(maskForDisplay(value, keyName), null, 2);
    }
    if (value === null || value === undefined) {
      return "unknown";
    }
    return String(value);
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

  function makeInput(key, value) {
    if (key === "zfs_versioning") {
      var select = el("select", "field-input");
      var seen = ZFS_VERSIONING_OPTIONS.slice();
      if (value !== undefined && value !== null && seen.indexOf(String(value)) === -1) {
        seen.push(String(value));
      }
      seen.forEach(function (opt) {
        var o = el("option", null, opt);
        o.value = opt;
        select.appendChild(o);
      });
      select.value = value === undefined || value === null ? "" : String(value);
      return select;
    }
    var input = el("input", "field-input");
    input.type = key === "zfs_versioning_reflink_retention" ? "number" : "text";
    if (key === "zfs_versioning_reflink_retention") {
      input.min = "0";
      input.step = "1";
    }
    input.spellcheck = false;
    input.autocomplete = "off";
    input.value = value === undefined || value === null ? "" : String(value);
    return input;
  }

  function fieldRow(key, value, draft, original) {
    var row = el("div", "config-field");
    var label = el("label", "field-label", key);
    var control;
    if (Object.prototype.hasOwnProperty.call(EDITABLE, key)) {
      control = makeInput(key, value);
      control.setAttribute("aria-label", key);
      control.id = "config-" + key;
      label.setAttribute("for", control.id);
      var keyName = key;
      control.addEventListener("input", function () {
        draft[keyName] = control.value;
      });
      control.addEventListener("change", function () {
        draft[keyName] = control.value;
      });
      row.appendChild(label);
      row.appendChild(control);
      row.appendChild(el("div", "config-help", "Hot-apply: takes effect immediately."));
      return row;
    }

    label.removeAttribute("for");
    row.appendChild(label);
    var val = el("pre", "mono config-value", displayValue(value, key));
    row.appendChild(val);
    var help;
    if (isSecretName(key) || value === MASK) {
      help = "Read-only: masked secret.";
    } else {
      help = "Read-only: changing this key requires a restart.";
    }
    row.appendChild(el("div", "config-help", help));
    return row;
  }

  function renderConfig(view, data, state) {
    view.textContent = "";
    var config = data && typeof data === "object" ? data : {};
    var restartKeys = Array.isArray(data.restartRequired) ? data.restartRequired : [];

    var toolbar = el("div", "panel");
    var tHead = el("div", "panel-head");
    tHead.appendChild(el("h2", "panel-title", "Configuration"));
    var actions = el("div");
    var apply = el("button", "btn primary-btn", "Apply");
    apply.type = "button";
    apply.addEventListener("click", function () {
      applyPatch(state, view);
    });
    var save = el("button", "btn", "Save to file");
    save.type = "button";
    save.addEventListener("click", function () {
      saveConfig(view);
    });
    var reload = el("button", "btn", "Reload identities");
    reload.type = "button";
    reload.addEventListener("click", function () {
      reloadIdentities(view);
    });
    actions.appendChild(apply);
    actions.appendChild(save);
    actions.appendChild(reload);
    tHead.appendChild(actions);
    toolbar.appendChild(tHead);

    state.noticeHost = el("div");
    if (state.persistentNotice) {
      state.noticeHost.appendChild(noticeBlock(state.persistentNotice, "badge badge-warn"));
    }
    if (restartKeys.length) {
      var list = "Restart-required keys currently recorded: " + restartKeys.join(", ");
      state.noticeHost.appendChild(noticeBlock(list, "badge badge-warn"));
    }
    toolbar.appendChild(state.noticeHost);
    view.appendChild(toolbar);

    state.draft = {};
    state.original = {};

    var known = {};
    GROUPS.forEach(function (group) {
      group.keys.forEach(function (k) {
        known[k] = true;
      });
    });

    function appendGroup(title, keys) {
      var presentKeys = keys.filter(function (k) {
        return Object.prototype.hasOwnProperty.call(config, k);
      });
      if (!presentKeys.length) {
        return;
      }
      var panel = el("div", "panel");
      panel.appendChild(el("h2", "panel-title", title));
      presentKeys.forEach(function (key) {
        var value = config[key];
        state.original[key] = value === undefined || value === null ? "" : String(value);
        panel.appendChild(fieldRow(key, value, state.draft, state.original));
      });
      view.appendChild(panel);
    }

    GROUPS.forEach(function (group) {
      appendGroup(group.title, group.keys);
    });

    var leftovers = Object.keys(config).filter(function (k) {
      return k !== "restartRequired" && !known[k];
    });
    if (leftovers.length) {
      appendGroup("Other", leftovers);
    }
  }

  function noticeBlock(text, badgeClass) {
    var box = el("div", "panel");
    box.setAttribute("role", "status");
    var head = el("div", "panel-head");
    head.appendChild(el("h2", "panel-title", "Notice"));
    head.appendChild(el("span", badgeClass, "restart"));
    box.appendChild(head);
    box.appendChild(el("div", null, text));
    return box;
  }

  function showError(view, message) {
    if (!view) {
      return;
    }
    if (!view._configError) {
      view._configError = el("div", "state state-error");
      view.insertBefore(view._configError, view.firstChild);
    }
    view._configError.textContent = "";
    view._configError.appendChild(el("div", "state-title", "Request failed"));
    view._configError.appendChild(el("div", null, message));
  }

  function clearError(view) {
    if (view && view._configError && view._configError.parentNode) {
      view._configError.parentNode.removeChild(view._configError);
      view._configError = null;
    }
  }

  function buildPatch(state) {
    var patch = {};
    var changed = false;
    Object.keys(EDITABLE).forEach(function (key) {
      var next = state.draft[key];
      if (next === undefined) {
        return;
      }
      var prev = state.original[key];
      if (key === "zfs_versioning_reflink_retention") {
        if (next === "" || next === prev) {
          return;
        }
        if (!/^-?\d+$/.test(next)) {
          patch.__invalid__ = "reflink retention must be an integer";
          changed = true;
          return;
        }
        patch[key] = parseInt(next, 10);
        changed = true;
        return;
      }
      if (next === prev) {
        return;
      }
      patch[key] = next;
      changed = true;
    });
    return changed ? patch : null;
  }

  function reload(view, state) {
    view.textContent = "";
    view.appendChild(loadingState("Loading configuration\u2026"));
    Console()
      .api("GET", "/api/config")
      .then(function (data) {
        renderConfig(view, data || {}, state);
      })
      .catch(function (err) {
        view.textContent = "";
        view.appendChild(
          errorState("Configuration request failed", gatewayMessage(err), function () {
            reload(view, state);
          })
        );
      });
  }

  function applyPatch(state, view) {
    clearError(view);
    var patch = buildPatch(state);
    if (patch && patch.__invalid__) {
      showError(view, patch.__invalid__);
      Console().toast("error", patch.__invalid__);
      return;
    }
    if (!patch) {
      Console().toast("info", "No hot-apply changes to submit.");
      return;
    }
    Console()
      .api("PUT", "/api/config", patch)
      .then(function (result) {
        result = result || {};
        var applied = Array.isArray(result.applied) ? result.applied : [];
        var restart = Array.isArray(result.restartRequired) ? result.restartRequired : [];
        if (applied.length) {
          Console().toast("ok", "Applied: " + applied.join(", "));
        } else {
          Console().toast("ok", "Configuration accepted.");
        }
        state.persistentNotice = restart.length
          ? "Restart required for: " + restart.join(", ") + ". The change will not take effect until the server restarts."
          : "";
        reload(view, state);
      })
      .catch(function (err) {
        // Invalid patch: surface the gateway's validator message and do NOT
        // render anything as applied.
        var message = gatewayMessage(err);
        showError(view, message);
        Console().toast("error", message);
      });
  }

  function saveConfig(view) {
    clearError(view);
    Console()
      .api("POST", "/api/config/save")
      .then(function (result) {
        var saved = result && result.saved === true;
        Console().toast("ok", saved ? "Configuration saved to file." : "Save requested.");
      })
      .catch(function (err) {
        var message = gatewayMessage(err);
        showError(view, message);
        Console().toast("error", message);
      });
  }

  function reloadIdentities(view) {
    clearError(view);
    Console()
      .api("POST", "/api/auth/reload")
      .then(function (result) {
        var reloaded = result && result.reloaded === true;
        Console().toast("ok", reloaded ? "Identities reloaded." : "Reload requested.");
      })
      .catch(function (err) {
        var message = gatewayMessage(err);
        showError(view, message);
        Console().toast("error", message);
      });
  }

  function mount(view) {
    var state = { draft: {}, original: {}, persistentNotice: "", noticeHost: null };
    reload(view, state);
  }

  if (window.Console && window.Console.screens) {
    window.Console.screens.config = mount;
  } else {
    document.addEventListener("DOMContentLoaded", function () {
      if (window.Console && window.Console.screens) {
        window.Console.screens.config = mount;
      }
    });
  }
})();
