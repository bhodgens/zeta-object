/*
 * Danger-zone screen (admin-server-2026-10, leaf 04 Task 4).
 *
 * Explains plainly that a purge destroys the event history AND the permanent
 * gap/loss record, and requires typing the dataset name into the confirmation
 * modal before POST /api/purge is sent. The success and failure results both
 * render what the gateway returned.
 *
 * Plain module, no framework. Attaches window.Console.screens.danger.
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

  function renderShell(view, state) {
    view.textContent = "";

    var explained = el("div", "panel");
    var head = el("div", "panel-head");
    head.appendChild(el("h2", "panel-title", "Purge metadata history"));
    head.appendChild(el("span", "badge badge-danger", "destructive"));
    explained.appendChild(head);
    explained.appendChild(
      el(
        "p",
        null,
        "Purge permanently destroys BOTH the event history AND the permanent gap/loss record for a dataset. " +
          "After a purge there is no record of the events that were removed, and the tracking of missing or lost " +
          "events is cleared as well. This cannot be undone."
      )
    );

    var label = el("label", "field-label", "Dataset name");
    label.setAttribute("for", "danger-dataset");
    var input = el("input", "field-input");
    input.type = "text";
    input.id = "danger-dataset";
    input.autocomplete = "off";
    input.spellcheck = false;
    var purge = el("button", "btn danger-btn", "Purge");
    purge.type = "button";
    purge.addEventListener("click", function () {
      startPurge(state, input);
    });

    explained.appendChild(label);
    explained.appendChild(input);
    explained.appendChild(purge);
    view.appendChild(explained);

    state.resultHost = el("div");
    view.appendChild(state.resultHost);
  }

  function confirmByName(dataset) {
    var body = el("div");
    body.appendChild(
      el(
        "p",
        null,
        "This destroys the event history and the gap/loss record for the dataset and cannot be undone. " +
          "Type the dataset name exactly to confirm:"
      )
    );
    body.appendChild(el("div", "mono", dataset));
    var label = el("label", "field-label", "Dataset name");
    label.setAttribute("for", "danger-confirm-input");
    var input = el("input", "field-input");
    input.type = "text";
    input.id = "danger-confirm-input";
    input.autocomplete = "off";
    input.spellcheck = false;
    body.appendChild(label);
    body.appendChild(input);

    var promise = Console().modal("Confirm purge", body);
    var confirmBtn = document.getElementById("modal-confirm");
    if (confirmBtn) {
      confirmBtn.disabled = true;
      input.addEventListener("input", function () {
        confirmBtn.disabled = input.value !== dataset;
      });
      input.focus();
    }
    return promise.then(function (ok) {
      if (confirmBtn) {
        confirmBtn.disabled = false;
      }
      return ok === true && input.value === dataset;
    });
  }

  function startPurge(state, input) {
    var dataset = input.value.trim();
    if (!dataset) {
      Console().toast("error", "Enter the dataset name.");
      input.focus();
      return;
    }
    confirmByName(dataset).then(function (confirmed) {
      if (!confirmed) {
        return;
      }
      sendPurge(state, dataset);
    });
  }

  function showResult(state, text, isError) {
    state.resultHost.textContent = "";
    var box = el("div", "panel");
    box.setAttribute("role", isError ? "alert" : "status");
    var head = el("div", "panel-head");
    head.appendChild(el("h2", "panel-title", isError ? "Purge failed" : "Purge result"));
    head.appendChild(el("span", isError ? "badge badge-danger" : "badge badge-ok", isError ? "error" : "ok"));
    box.appendChild(head);
    box.appendChild(el("div", null, text));
    state.resultHost.appendChild(box);
  }

  function sendPurge(state, dataset) {
    Console()
      .api("POST", "/api/purge", { dataset: dataset })
      .then(function (result) {
        result = result || {};
        var purged = result.purged === true;
        var named = result.dataset ? String(result.dataset) : dataset;
        showResult(
          state,
          purged
            ? 'Event history and the gap/loss record were purged for "' + named + '".'
            : 'Purge requested for "' + named + '".',
          false
        );
        Console().toast("ok", 'Purged metadata history for "' + named + '".');
      })
      .catch(function (err) {
        var message = gatewayMessage(err);
        showResult(state, message, true);
        Console().toast("error", message);
      });
  }

  function mount(view) {
    var state = {};
    renderShell(view, state);
  }

  if (window.Console && window.Console.screens) {
    window.Console.screens.danger = mount;
  } else {
    document.addEventListener("DOMContentLoaded", function () {
      if (window.Console && window.Console.screens) {
        window.Console.screens.danger = mount;
      }
    });
  }
})();
