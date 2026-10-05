/*
 * Buckets screen (admin-server-2026-10, leaf 04 Task 3).
 *
 * Lists GET /api/buckets with backend, tunables and a dataset marker; a create
 * control calls POST /api/buckets; a row action opens a detail panel from
 * GET /api/buckets/{name}; the detail edits auditReads and reflinkRetention
 * through PUT /api/buckets/{name}/settings; deleting a plain bucket goes
 * through the confirmation modal, and a bucket the detail reports as a dataset
 * is never offered a delete that the gateway would refuse (the UI explains
 * that dataset destruction is a host action). A 409 DatasetBucketNotDeletable
 * that still occurs is shown verbatim.
 *
 * Plain module, no framework. Attaches window.Console.screens.buckets.
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

  function boolLabel(value) {
    if (value === true) {
      return "true";
    }
    if (value === false) {
      return "false";
    }
    return "unknown";
  }

  function datasetLabel(value) {
    if (value === true) {
      return "dataset";
    }
    if (value === false) {
      return "directory";
    }
    return "unknown";
  }

  function retentionLabel(value) {
    if (value === undefined || value === null || value === "") {
      return "default";
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

  function notice(text, badgeCls) {
    var box = el("div", "panel");
    box.setAttribute("role", "status");
    var head = el("div", "panel-head");
    head.appendChild(el("h2", "panel-title", "Notice"));
    if (badgeCls) {
      head.appendChild(el("span", badgeCls, "notice"));
    }
    box.appendChild(head);
    box.appendChild(el("div", null, text));
    return box;
  }

  function renderShell(view, state) {
    view.textContent = "";

    var create = el("div", "panel");
    var cHead = el("div", "panel-head");
    cHead.appendChild(el("h2", "panel-title", "Create bucket"));
    create.appendChild(cHead);
    var form = el("div");
    var label = el("label", "field-label", "New bucket name");
    label.setAttribute("for", "bucket-create-name");
    var input = el("input", "field-input");
    input.type = "text";
    input.id = "bucket-create-name";
    input.autocomplete = "off";
    input.spellcheck = false;
    var button = el("button", "btn primary-btn", "Create");
    button.type = "button";
    button.addEventListener("click", function () {
      createBucket(state, input, view);
    });
    form.appendChild(label);
    form.appendChild(input);
    form.appendChild(button);
    create.appendChild(form);
    view.appendChild(create);

    state.noticeHost = el("div");
    view.appendChild(state.noticeHost);

    state.listHost = el("div");
    view.appendChild(state.listHost);

    state.detailHost = el("div");
    view.appendChild(state.detailHost);
  }

  function setNotice(state, text, badgeCls) {
    state.noticeHost.textContent = "";
    if (text) {
      state.noticeHost.appendChild(notice(text, badgeCls));
    }
  }

  function loadList(view, state) {
    state.listHost.textContent = "";
    state.listHost.appendChild(loadingState("Loading buckets\u2026"));
    Console()
      .api("GET", "/api/buckets")
      .then(function (data) {
        var buckets = data && Array.isArray(data.buckets) ? data.buckets : [];
        state.buckets = buckets;
        renderList(view, state);
      })
      .catch(function (err) {
        state.listHost.textContent = "";
        state.listHost.appendChild(
          errorState("Bucket list failed", gatewayMessage(err), function () {
            loadList(view, state);
          })
        );
      });
  }

  function renderList(view, state) {
    state.listHost.textContent = "";
    var buckets = state.buckets || [];

    var panel = el("div", "panel");
    var head = el("div", "panel-head");
    head.appendChild(el("h2", "panel-title", "Buckets"));
    var refresh = el("button", "btn", "Refresh");
    refresh.type = "button";
    refresh.addEventListener("click", function () {
      loadList(view, state);
    });
    head.appendChild(refresh);
    panel.appendChild(head);

    if (!buckets.length) {
      panel.appendChild(el("div", "state", "No buckets reported."));
      state.listHost.appendChild(panel);
      return;
    }

    var table = el("table");
    var thead = el("thead");
    var hrow = el("tr");
    ["Name", "Backend", "auditReads", "reflinkRetention", "Dataset", "Created", "Actions"].forEach(function (h) {
      hrow.appendChild(el("th", null, h));
    });
    thead.appendChild(hrow);
    table.appendChild(thead);

    var tbody = el("tbody");
    buckets.forEach(function (bucket) {
      var name = bucket && bucket.name !== undefined ? String(bucket.name) : "";
      var row = el("tr");
      row.appendChild(el("td", "mono", unknown(bucket.name)));
      row.appendChild(el("td", null, unknown(bucket.backend)));
      row.appendChild(el("td", null, boolLabel(bucket.auditReads)));
      row.appendChild(el("td", null, retentionLabel(bucket.reflinkRetention)));
      var datasetCell = el("td");
      datasetCell.appendChild(
        el(
          "span",
          bucket.isDataset === true ? "badge badge-warn" : "badge badge-neutral",
          datasetLabel(bucket.isDataset)
        )
      );
      row.appendChild(datasetCell);
      row.appendChild(el("td", "mono", bucket.createdAt ? String(bucket.createdAt) : "unknown"));

      var actions = el("td");
      var detail = el("button", "btn", "Detail");
      detail.type = "button";
      detail.addEventListener("click", function () {
        selectBucket(view, state, name);
      });
      actions.appendChild(detail);
      if (name) {
        var del = el("button", "btn danger-btn", "Delete");
        del.type = "button";
        del.addEventListener("click", function () {
          confirmDelete(view, state, name);
        });
        actions.appendChild(del);
      }
      row.appendChild(actions);
      tbody.appendChild(row);
    });
    table.appendChild(tbody);
    panel.appendChild(table);
    state.listHost.appendChild(panel);
  }

  function createBucket(state, input, view) {
    var name = input.value.trim();
    if (!name) {
      Console().toast("error", "Enter a bucket name.");
      input.focus();
      return;
    }
    Console()
      .api("POST", "/api/buckets", { name: name })
      .then(function (result) {
        var created = result && result.created === true;
        Console().toast("ok", created ? 'Created bucket "' + name + '".' : 'Create requested for "' + name + '".');
        input.value = "";
        loadList(view, state);
      })
      .catch(function (err) {
        var message = gatewayMessage(err);
        setNotice(state, message, "badge badge-danger");
        Console().toast("error", message);
      });
  }

  function selectBucket(view, state, name) {
    state.detailHost.textContent = "";
    state.detailHost.appendChild(loadingState("Loading bucket detail\u2026"));
    Console()
      .api("GET", "/api/buckets/" + encodeURIComponent(name))
      .then(function (data) {
        state.detail = data || {};
        state.detailName = name;
        renderDetail(view, state);
      })
      .catch(function (err) {
        state.detailHost.textContent = "";
        state.detailHost.appendChild(errorState("Bucket detail failed", gatewayMessage(err), null));
      });
  }

  function renderDetail(view, state) {
    var data = state.detail || {};
    var name = state.detailName || data.name || "";
    state.detailHost.textContent = "";

    var panel = el("div", "panel");
    var head = el("div", "panel-head");
    head.appendChild(el("h2", "panel-title", "Bucket: " + unknown(name)));
    head.appendChild(
      el(
        "span",
        data.isDataset === true ? "badge badge-warn" : "badge badge-neutral",
        datasetLabel(data.isDataset)
      )
    );
    panel.appendChild(head);

    var grid = el("div", "grid");
    function field(label, value) {
      var m = el("div", "metric");
      m.appendChild(el("div", "metric-label", label));
      m.appendChild(el("div", "metric-value", value));
      return m;
    }
    grid.appendChild(field("Backend", unknown(data.backend)));
    grid.appendChild(field("auditReads", boolLabel(data.auditReads)));
    grid.appendChild(field("reflinkRetention", retentionLabel(data.reflinkRetention)));
    panel.appendChild(grid);

    if (data.isDataset === true) {
      var explain = el("div", "state state-error");
      explain.appendChild(el("div", "state-title", "Dataset-backed bucket"));
      explain.appendChild(
        el(
          "div",
          null,
          "This bucket is a ZFS dataset. The management API does not destroy datasets, so no delete is offered here. Remove it on the host with `zfs destroy " +
            name +
            "`."
        )
      );
      panel.appendChild(explain);
    }

    var settings = el("div");
    settings.appendChild(el("h2", "panel-title", "Settings"));

    var auditLabel = el("label", "field-label", "auditReads");
    auditLabel.setAttribute("for", "bucket-audit-reads");
    var audit = el("input");
    audit.type = "checkbox";
    audit.id = "bucket-audit-reads";
    audit.checked = data.auditReads === true;
    settings.appendChild(auditLabel);
    settings.appendChild(audit);

    var retLabel = el("label", "field-label", "reflinkRetention");
    retLabel.setAttribute("for", "bucket-reflink-retention");
    var retention = el("input", "field-input");
    retention.type = "number";
    retention.min = "0";
    retention.step = "1";
    retention.id = "bucket-reflink-retention";
    retention.value = data.reflinkRetention === undefined || data.reflinkRetention === null ? "" : String(data.reflinkRetention);
    settings.appendChild(retLabel);
    settings.appendChild(retention);

    var saveSettings = el("button", "btn primary-btn", "Save settings");
    saveSettings.type = "button";
    saveSettings.addEventListener("click", function () {
      saveBucketSettings(view, state, name, audit, retention);
    });
    settings.appendChild(saveSettings);

    if (data.isDataset !== true) {
      var del = el("button", "btn danger-btn", "Delete bucket");
      del.type = "button";
      del.addEventListener("click", function () {
        confirmDelete(view, state, name);
      });
      settings.appendChild(del);
    }

    panel.appendChild(settings);
    state.detailHost.appendChild(panel);
  }

  function saveBucketSettings(view, state, name, audit, retention) {
    var patch = { auditReads: audit.checked };
    var raw = retention.value.trim();
    if (raw !== "") {
      if (!/^\d+$/.test(raw)) {
        var invalid = "reflinkRetention must be a non-negative integer.";
        setNotice(state, invalid, "badge badge-danger");
        Console().toast("error", invalid);
        return;
      }
      patch.reflinkRetention = parseInt(raw, 10);
    }
    Console()
      .api("PUT", "/api/buckets/" + encodeURIComponent(name) + "/settings", patch)
      .then(function (result) {
        var updated = result && result.updated === true;
        Console().toast("ok", updated ? 'Updated settings for "' + name + '".' : "Settings update requested.");
        selectBucket(view, state, name);
      })
      .catch(function (err) {
        var message = gatewayMessage(err);
        setNotice(state, message, "badge badge-danger");
        Console().toast("error", message);
      });
  }

  function confirmDelete(view, state, name) {
    Console()
      .api("GET", "/api/buckets/" + encodeURIComponent(name))
      .then(function (detail) {
        detail = detail || {};
        if (detail.isDataset === true) {
          state.detail = detail;
          state.detailName = name;
          renderDetail(view, state);
          Console().toast("warn", 'Bucket "' + name + '" is a ZFS dataset; it is not deletable through this API.');
          return;
        }
        Console()
          .modal("Delete bucket", 'Delete bucket "' + name + '" and its contents? This cannot be undone.')
          .then(function (confirmed) {
            if (!confirmed) {
              return;
            }
            doDelete(view, state, name);
          });
      })
      .catch(function (err) {
        var message = gatewayMessage(err);
        setNotice(state, message, "badge badge-danger");
        Console().toast("error", message);
      });
  }

  function doDelete(view, state, name) {
    Console()
      .api("DELETE", "/api/buckets/" + encodeURIComponent(name))
      .then(function (result) {
        var deleted = result && result.deleted === true;
        Console().toast("ok", deleted ? 'Deleted bucket "' + name + '".' : "Delete requested.");
        state.detail = null;
        state.detailHost.textContent = "";
        loadList(view, state);
      })
      .catch(function (err) {
        // Includes 409 DatasetBucketNotDeletable: show the gateway's message
        // verbatim.
        var message = gatewayMessage(err);
        setNotice(state, message, "badge badge-danger");
        Console().toast("error", message);
      });
  }

  function mount(view) {
    var state = {};
    renderShell(view, state);
    loadList(view, state);
  }

  if (window.Console && window.Console.screens) {
    window.Console.screens.buckets = mount;
  } else {
    document.addEventListener("DOMContentLoaded", function () {
      if (window.Console && window.Console.screens) {
        window.Console.screens.buckets = mount;
      }
    });
  }
})();
