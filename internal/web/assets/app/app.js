(function () {
  const state = {
    panel: null,
    loadingRows: false,
    flashTimer: null
  };

  function pageSize() {
    return Math.max(30, Math.ceil((window.innerHeight / 36) * 1.5));
  }

  function makeTableURL(table, options) {
    const params = new URLSearchParams();
    params.set("limit", String(pageSize()));
    if (options && options.sort) {
      params.set("sort", options.sort);
    }
    if (options && options.desc) {
      params.set("desc", "true");
    }
    return "/tables/" + table + "?" + params.toString();
  }

  function updateNav(table) {
    document.querySelectorAll(".stockit-nav").forEach(function (button) {
      const active = button.dataset.table === table;
      button.classList.toggle("border-sky-600", active);
      button.classList.toggle("text-sky-700", active);
      button.classList.toggle("bg-sky-50", active);
    });
  }

  function currentPanel() {
    return state.panel || document.querySelector("[data-stockit-panel]");
  }

  function currentTable() {
    const panel = currentPanel();
    return panel ? panel.dataset.table : "";
  }

  function loadTable(table, options) {
    updateNav(table);
    htmx.ajax("GET", makeTableURL(table, options || {}), { target: "#table-panel", swap: "innerHTML" });
  }

  function initPanel() {
    const panel = document.querySelector("[data-stockit-panel]");
    state.panel = panel;
    state.loadingRows = false;
    if (!panel) {
      return;
    }

    updateNav(panel.dataset.table);
    const wrap = panel.querySelector(".stockit-table-wrap");
    if (wrap) {
      wrap.addEventListener("scroll", maybeLoadMore, { passive: true });
    }
  }

  function maybeLoadMore() {
    const panel = currentPanel();
    if (!panel || state.loadingRows || panel.dataset.hasMore !== "true") {
      return;
    }

    const wrap = panel.querySelector(".stockit-table-wrap");
    if (!wrap) {
      return;
    }
    if ((wrap.scrollTop + wrap.clientHeight) < (wrap.scrollHeight - 96)) {
      return;
    }

    state.loadingRows = true;

    const params = new URLSearchParams({
      offset: panel.dataset.offset || "0",
      limit: panel.dataset.limit || String(pageSize()),
      sort: panel.dataset.sort || "",
      desc: panel.dataset.desc || "false"
    });

    fetch("/tables/" + panel.dataset.table + "/rows?" + params.toString(), {
      credentials: "same-origin",
      headers: { "HX-Request": "true" }
    }).then(function (response) {
      const redirect = response.headers.get("HX-Redirect");
      if (redirect) {
        window.location.href = redirect;
        return Promise.reject(new Error("redirect"));
      }
      if (!response.ok) {
        return Promise.reject(new Error("load failed"));
      }

      panel.dataset.hasMore = response.headers.get("X-Has-More") || "false";
      return response.text();
    }).then(function (html) {
      if (!html) {
        return;
      }

      const tbody = panel.querySelector("#table-body");
      const placeholder = tbody.querySelector("td[colspan]");
      if (placeholder) {
        placeholder.parentElement.remove();
      }

      const template = document.createElement("template");
      template.innerHTML = html.trim();
      const newRows = Array.from(template.content.querySelectorAll("tr"));
      newRows.forEach(function (row) {
        tbody.appendChild(row);
      });

      const loaded = newRows.length;
      panel.dataset.offset = String((Number(panel.dataset.offset || "0")) + loaded);
    }).catch(function (error) {
      if (error.message !== "redirect") {
        showFlash("Unable to load more rows.", "error");
      }
    }).finally(function () {
      state.loadingRows = false;
    });
  }

  function sortTable(column) {
    const panel = currentPanel();
    if (!panel) {
      return;
    }

    const nextDesc = panel.dataset.sort === column ? panel.dataset.desc !== "true" : false;
    loadTable(panel.dataset.table, { sort: column, desc: nextDesc });
  }

  function openForm(table, id) {
    let url = "/tables/" + table + "/form";
    if (id) {
      url += "?id=" + encodeURIComponent(id);
    }
    htmx.ajax("GET", url, { target: "#modal-body", swap: "innerHTML" });
  }

  function rowSelected(row) {
    const panel = currentPanel();
    if (!panel || panel.dataset.canWrite !== "true") {
      return;
    }
    openForm(panel.dataset.table, row.dataset.rowId);
  }

  function openModal() {
    const modal = document.getElementById("stockit-modal");
    if (!modal) {
      return;
    }
    modal.classList.remove("hidden");
    modal.classList.add("flex");
  }

  function closeModal() {
    const modal = document.getElementById("stockit-modal");
    const body = document.getElementById("modal-body");
    if (!modal || !body) {
      return;
    }
    body.innerHTML = "";
    modal.classList.add("hidden");
    modal.classList.remove("flex");
  }

  function maybeCloseModal(event) {
    if (event.target && event.target.id === "stockit-modal") {
      closeModal();
    }
  }

  function reloadCurrentTable() {
    const panel = currentPanel();
    if (!panel) {
      return;
    }
    loadTable(panel.dataset.table, {
      sort: panel.dataset.sort,
      desc: panel.dataset.desc === "true"
    });
  }

  function showFlash(message, kind) {
    const flash = document.getElementById("stockit-flash");
    if (!flash) {
      return;
    }

    flash.textContent = message;
    flash.classList.remove("hidden", "border-red-200", "bg-red-50", "text-red-700", "border-sky-200", "bg-sky-50", "text-sky-700");
    if (kind === "error") {
      flash.classList.add("border-red-200", "bg-red-50", "text-red-700");
    } else {
      flash.classList.add("border-sky-200", "bg-sky-50", "text-sky-700");
    }

    if (state.flashTimer) {
      clearTimeout(state.flashTimer);
    }
    state.flashTimer = window.setTimeout(function () {
      flash.classList.add("hidden");
    }, 2500);
  }

  function boot(defaultTable) {
    document.body.addEventListener("htmx:afterSwap", function (event) {
      const target = event.detail.target;
      if (target && target.id === "table-panel") {
        initPanel();
      }
      if (target && target.id === "modal-body") {
        openModal();
      }
    });

    document.body.addEventListener("stockit:refresh-table", reloadCurrentTable);
    document.body.addEventListener("stockit:close-modal", closeModal);
    document.body.addEventListener("stockit:toast", function (event) {
      const detail = event.detail || {};
      if (detail.message) {
        showFlash(detail.message, "info");
      }
    });

    window.addEventListener("resize", function () {
      const panel = currentPanel();
      if (panel) {
        panel.dataset.limit = String(pageSize());
      }
    });

    loadTable(defaultTable, {});
  }

  window.StockIt = {
    boot: boot,
    closeModal: closeModal,
    loadTable: loadTable,
    maybeCloseModal: maybeCloseModal,
    openForm: openForm,
    rowSelected: rowSelected,
    sortTable: sortTable
  };
})();
