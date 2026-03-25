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
    if (options && options.parentTable && options.parentID && options.parentField) {
      params.set("parent_table", options.parentTable);
      params.set("parent_id", options.parentID);
      params.set("parent_field", options.parentField);
    }
    return "/tables/" + table + "?" + params.toString();
  }

  function updateNav(table) {
    document.querySelectorAll(".stockit-nav").forEach(function (button) {
      button.classList.toggle("is-active", button.dataset.table === table);
    });
  }

  function currentPanel() {
    return state.panel || document.querySelector("[data-stockit-panel]");
  }

  function currentTable() {
    const panel = currentPanel();
    return panel ? panel.dataset.table : "";
  }

  function clearSelectedRow() {
    const panel = currentPanel();
    if (!panel) {
      return;
    }
    panel.querySelectorAll(".stockit-row.is-selected").forEach(function (row) {
      row.classList.remove("is-selected");
    });
  }

  function currentRows(panel) {
    if (!panel) {
      return [];
    }
    return Array.from(panel.querySelectorAll("#table-body .stockit-row"));
  }

  function selectedRow(panel) {
    if (!panel) {
      return null;
    }
    return panel.querySelector("#table-body .stockit-row.is-selected");
  }

  function ensureRowVisible(panel, row) {
    if (!panel || !row) {
      return;
    }

    const wrap = panel.querySelector(".stockit-table-wrap");
    if (!wrap) {
      return;
    }

    const header = panel.querySelector(".stockit-table-head");
    const wrapRect = wrap.getBoundingClientRect();
    const rowRect = row.getBoundingClientRect();
    const headerHeight = header ? header.getBoundingClientRect().height : 0;
    const rowTop = rowRect.top - wrapRect.top + wrap.scrollTop;
    const rowBottom = rowTop + rowRect.height;
    const visibleTop = wrap.scrollTop + headerHeight;
    const visibleBottom = wrap.scrollTop + wrap.clientHeight;
    const edgePadding = 4;

    if (rowTop < visibleTop) {
      wrap.scrollTop = Math.max(0, rowTop - headerHeight - edgePadding);
      return;
    }
    if (rowBottom > visibleBottom) {
      wrap.scrollTop = rowBottom - wrap.clientHeight + edgePadding;
    }
  }

  function setActiveRow(row, options) {
    const panel = currentPanel();
    if (!panel || !row) {
      return;
    }

    clearSelectedRow();
    row.classList.add("is-selected");

    if (!options || options.scrollIntoView !== false) {
      ensureRowVisible(panel, row);
    }
    if (options && options.loadChild === false) {
      return;
    }
    if (panel.dataset.childTable && panel.dataset.childField) {
      loadTable(panel.dataset.childTable, {
        parentTable: panel.dataset.table,
        parentID: row.dataset.rowId,
        parentField: panel.dataset.childField,
        navTable: panel.dataset.navTable || panel.dataset.table
      });
    }
  }

  function pageRowStep(panel, rows) {
    const wrap = panel.querySelector(".stockit-table-wrap");
    if (!wrap || rows.length === 0) {
      return 1;
    }

    const rowHeight = rows[0].getBoundingClientRect().height;
    if (!rowHeight) {
      return 10;
    }
    return Math.max(1, Math.floor(wrap.clientHeight / rowHeight) - 1);
  }

  function moveActiveRow(direction, usePageStep) {
    const panel = currentPanel();
    const rows = currentRows(panel);
    if (!panel || rows.length === 0) {
      return;
    }

    const current = selectedRow(panel);
    const step = usePageStep ? pageRowStep(panel, rows) : 1;
    let nextIndex = direction > 0 ? 0 : rows.length - 1;
    if (current) {
      const currentIndex = rows.indexOf(current);
      nextIndex = Math.max(0, Math.min(rows.length - 1, currentIndex + (direction * step)));
    }

    setActiveRow(rows[nextIndex], { loadChild: false });
  }

  function panelParentContext(panel, table) {
    if (!panel) {
      return null;
    }
    if (table && panel.dataset.table !== table) {
      return null;
    }
    if (!panel.dataset.parentTable || !panel.dataset.parentId || !panel.dataset.parentField) {
      return null;
    }
    return {
      parentTable: panel.dataset.parentTable,
      parentID: panel.dataset.parentId,
      parentField: panel.dataset.parentField
    };
  }

  function loadTable(table, options) {
    updateNav((options && options.navTable) || table);
    htmx.ajax("GET", makeTableURL(table, options || {}), { target: "#table-panel", swap: "innerHTML" });
  }

  function initPanel() {
    const panel = document.querySelector("[data-stockit-panel]");
    state.panel = panel;
    state.loadingRows = false;
    if (!panel) {
      return;
    }

    updateNav(panel.dataset.navTable || panel.dataset.table);
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
    const parentContext = panelParentContext(panel, panel.dataset.table);
    if (parentContext) {
      params.set("parent_table", parentContext.parentTable);
      params.set("parent_id", parentContext.parentID);
      params.set("parent_field", parentContext.parentField);
    }

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
    const options = { sort: column, desc: nextDesc, navTable: panel.dataset.navTable || panel.dataset.table };
    const parentContext = panelParentContext(panel, panel.dataset.table);
    if (parentContext) {
      options.parentTable = parentContext.parentTable;
      options.parentID = parentContext.parentID;
      options.parentField = parentContext.parentField;
    }
    loadTable(panel.dataset.table, options);
  }

  function openForm(table, id) {
    if (!id) {
      clearSelectedRow();
    }

    let url = "/tables/" + table + "/form";
    const params = new URLSearchParams();
    if (id) {
      params.set("id", id);
    }

    const panel = currentPanel();
    const parentContext = panelParentContext(panel, table);
    if (parentContext) {
      params.set("parent_table", parentContext.parentTable);
      params.set("parent_id", parentContext.parentID);
      params.set("parent_field", parentContext.parentField);
    }
    const query = params.toString();
    if (query) {
      url += "?" + query;
    }
    htmx.ajax("GET", url, { target: "#modal-body", swap: "innerHTML" });
  }

  function rowSelected(row) {
    const panel = currentPanel();
    if (!panel) {
      return;
    }

    setActiveRow(row, { scrollIntoView: false });
    if (panel.dataset.childTable && panel.dataset.childField) {
      return;
    }
    if (panel.dataset.canWrite !== "true") {
      return;
    }
    openForm(panel.dataset.table, row.dataset.rowId);
  }

  function openSelectedRow(panel) {
    const row = selectedRow(panel);
    if (!panel || !row) {
      return;
    }

    if (panel.dataset.childTable && panel.dataset.childField) {
      loadTable(panel.dataset.childTable, {
        parentTable: panel.dataset.table,
        parentID: row.dataset.rowId,
        parentField: panel.dataset.childField,
        navTable: panel.dataset.navTable || panel.dataset.table
      });
      return;
    }

    if (panel.dataset.canWrite === "true") {
      openForm(panel.dataset.table, row.dataset.rowId);
    }
  }

  function returnToParentTable(panel) {
    if (!panel || !panel.dataset.parentTable) {
      return;
    }
    loadTable(panel.dataset.parentTable, {
      navTable: panel.dataset.parentTable
    });
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

  function currentModalForm() {
    return document.querySelector("#modal-body form[data-stockit-modal-form='true']");
  }

  function resizeTextarea(textarea) {
    if (!textarea) {
      return;
    }
    textarea.style.height = "auto";
    textarea.style.height = textarea.scrollHeight + "px";
  }

  function initModalForm() {
    const form = currentModalForm();
    if (!form) {
      return;
    }

    form.querySelectorAll("textarea[data-stockit-autogrow='true']").forEach(function (textarea) {
      resizeTextarea(textarea);
      if (textarea.dataset.stockitAutogrowBound === "true") {
        return;
      }
      textarea.dataset.stockitAutogrowBound = "true";
      textarea.addEventListener("input", function () {
        resizeTextarea(textarea);
      });
    });
  }

  function insertTextareaNewline(textarea) {
    const start = textarea.selectionStart;
    const end = textarea.selectionEnd;
    textarea.setRangeText("\n", start, end, "end");
    textarea.dispatchEvent(new Event("input", { bubbles: true }));
  }

  function handleModalKeydown(event) {
    const modal = document.getElementById("stockit-modal");
    if (!modal || modal.classList.contains("hidden")) {
      return;
    }

    const form = currentModalForm();
    if (!form) {
      return;
    }

    const target = event.target;
    if (!(target instanceof Element) || !modal.contains(target)) {
      return;
    }
    if (event.isComposing) {
      return;
    }

    if (event.key === "Escape") {
      event.preventDefault();
      closeModal();
      return;
    }

    if (event.key !== "Enter" || event.altKey || event.metaKey) {
      return;
    }
    if (target instanceof HTMLButtonElement) {
      return;
    }
    if (event.shiftKey || event.ctrlKey) {
      if (target instanceof HTMLTextAreaElement) {
        event.preventDefault();
        insertTextareaNewline(target);
      } else {
        event.preventDefault();
      }
      return;
    }
    if (!form.contains(target)) {
      return;
    }

    event.preventDefault();
    form.requestSubmit();
  }

  function dispatchHXTrigger(triggerHeader) {
    if (!triggerHeader) {
      return false;
    }

    let payload = null;
    try {
      payload = JSON.parse(triggerHeader);
    } catch (error) {
      return false;
    }
    if (!payload || typeof payload !== "object") {
      return false;
    }

    Object.keys(payload).forEach(function (name) {
      const detail = payload[name] && typeof payload[name] === "object" ? payload[name] : {};
      document.body.dispatchEvent(new CustomEvent(name, {
        bubbles: true,
        detail: detail
      }));
    });
    return true;
  }

  function deleteSelectedRow() {
    const panel = currentPanel();
    const row = selectedRow(panel);
    if (!panel || !row || panel.dataset.canWrite !== "true") {
      return;
    }
    const confirmMessage = row.dataset.rowDeleteConfirm || "Delete this record?";
    if (!window.confirm(confirmMessage)) {
      return;
    }

    fetch("/tables/" + panel.dataset.table + "/row/" + encodeURIComponent(row.dataset.rowId), {
      method: "DELETE",
      credentials: "same-origin",
      headers: { "HX-Request": "true" }
    }).then(function (response) {
      if (!response.ok) {
        return Promise.reject(new Error("delete failed"));
      }
      if (!dispatchHXTrigger(response.headers.get("HX-Trigger"))) {
        reloadCurrentTable();
      }
    }).catch(function () {
      showFlash("Unable to delete record.", "error");
    });
  }

  function isInteractiveTarget(target) {
    return target instanceof HTMLInputElement ||
      target instanceof HTMLTextAreaElement ||
      target instanceof HTMLSelectElement ||
      (target instanceof Element && (target.isContentEditable || target.closest("[contenteditable='true']")));
  }

  function handlePanelKeydown(event) {
    const modal = document.getElementById("stockit-modal");
    if (modal && !modal.classList.contains("hidden")) {
      return;
    }

    const panel = currentPanel();
    if (!panel || event.isComposing) {
      return;
    }

    const target = event.target;
    if (isInteractiveTarget(target)) {
      return;
    }

    if (event.altKey || event.metaKey || event.ctrlKey) {
      if (event.key !== "+" && event.code !== "NumpadAdd") {
        return;
      }
    }

    switch (event.key) {
      case "ArrowDown":
        event.preventDefault();
        moveActiveRow(1, false);
        return;
      case "ArrowUp":
        event.preventDefault();
        moveActiveRow(-1, false);
        return;
      case "PageDown":
        event.preventDefault();
        moveActiveRow(1, true);
        return;
      case "PageUp":
        event.preventDefault();
        moveActiveRow(-1, true);
        return;
      case "Enter":
        if (event.shiftKey) {
          return;
        }
        if (!selectedRow(panel)) {
          return;
        }
        event.preventDefault();
        openSelectedRow(panel);
        return;
      case "Escape":
        if (!panel.dataset.parentTable) {
          return;
        }
        event.preventDefault();
        returnToParentTable(panel);
        return;
      case "Delete":
        if (event.shiftKey) {
          return;
        }
        event.preventDefault();
        deleteSelectedRow();
        return;
      case "Insert":
        if (event.shiftKey) {
          return;
        }
        if (panel.dataset.canWrite !== "true") {
          return;
        }
        event.preventDefault();
        openForm(panel.dataset.table);
        return;
      default:
        if ((event.key === "+" || event.code === "NumpadAdd") && panel.dataset.canWrite === "true") {
          event.preventDefault();
          openForm(panel.dataset.table);
        }
    }
  }

  function reloadCurrentTable() {
    const panel = currentPanel();
    if (!panel) {
      return;
    }
    const options = {
      sort: panel.dataset.sort,
      desc: panel.dataset.desc === "true",
      navTable: panel.dataset.navTable || panel.dataset.table
    };
    const parentContext = panelParentContext(panel, panel.dataset.table);
    if (parentContext) {
      options.parentTable = parentContext.parentTable;
      options.parentID = parentContext.parentID;
      options.parentField = parentContext.parentField;
    }
    loadTable(panel.dataset.table, options);
  }

  function handleRecordDeleted(event) {
    const panel = currentPanel();
    if (!panel) {
      return;
    }

    const detail = event.detail || {};
    if (!detail.table || !detail.id) {
      reloadCurrentTable();
      return;
    }

    if (panel.dataset.parentTable === detail.table && panel.dataset.parentId === String(detail.id)) {
      loadTable(detail.table, { navTable: detail.table });
      return;
    }

    reloadCurrentTable();
  }

  function showFlash(message, kind) {
    const flash = document.getElementById("stockit-flash");
    if (!flash) {
      return;
    }

    flash.textContent = message;
    flash.classList.remove("hidden", "is-error");
    flash.classList.toggle("is-error", kind === "error");

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
        initModalForm();
      }
    });

    document.body.addEventListener("stockit:refresh-table", reloadCurrentTable);
    document.body.addEventListener("stockit:record-deleted", handleRecordDeleted);
    document.body.addEventListener("stockit:close-modal", closeModal);
    document.addEventListener("keydown", handleModalKeydown);
    document.addEventListener("keydown", handlePanelKeydown);
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
