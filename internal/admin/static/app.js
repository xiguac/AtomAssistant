// 管理后台各页共用的一段脚本，做两件事：
//   - 让带 data-async 的按钮在后台提交表单，结果显示在按钮所在的表单里，页面不刷新——
//     点「连接测试」之后，表单里已经填的内容都还在。
//   - 让「全选」勾选框一次勾上表单里的全部条目。
//   - 让带 data-poll 的表单自动在后台反复提交，直到有了结果（微信扫码连接等用户扫码时用）。
//   - 弹窗表单：带 data-dialog 的按钮打开对应的 <dialog>；按钮上的 data-v-字段名 把该行的现有值填进表单（修改），
//     没有就是空白表单（添加）。data-lock 指定的字段在修改时只读，免得改名变成新增。
//   - 所有 method="post" 的表单默认在后台提交：成功后弹出提示、关闭弹窗，并只重新载入内容区，
//     滚动位置、展开的折叠区和别处没提交的输入都保持原样；失败时错误显示在表单里，已填的内容不丢。
//     应答不是 JSON（登录失效、创建任务后跳转到任务页等）时按普通跳转处理。
//     上传文件、登录、退出、重启与带 data-poll 的表单不走这条路。
//   - 给表格的单元格写上列名（data-label），手机上表格按一行一张卡片显示时用它标注每一项。
//   - 带 data-show-if="字段=值1,值2" 的元素只在表单里那个字段取这几个值时显示（比如消息源选了微信，
//     就只显示扫码需要的那几项）。
// 脚本不可用时按钮照常提交表单并整页跳转，条目也可以逐个勾选，功能不受影响。
(function () {
  "use strict";

  function show(form, ok, text) {
    var box = form.querySelector(".async-result");
    if (!box) {
      box = document.createElement("div");
      box.className = "async-result";
      form.appendChild(box);
    }
    box.className = "async-result " + (ok ? "ok" : "bad");
    box.textContent = text;
  }

  // 「全选」：带 data-check-all 的勾选框控制它所在表单里的全部勾选框。
  document.addEventListener("change", function (ev) {
    var all = ev.target;
    if (!all.matches || !all.matches("input[data-check-all]") || !all.form) {
      return;
    }
    var boxes = all.form.querySelectorAll('input[type="checkbox"][name]');
    for (var i = 0; i < boxes.length; i++) {
      boxes[i].checked = all.checked;
    }
  });

  // 自动查询：带 data-poll 的表单会一遍遍在后台提交，把结果显示出来，直到应答里说不用再问了（done）。
  // 微信扫码连接用它等用户扫码：每一次提交服务端都会挂住一会儿，状态有变化才回话。
  function poll(form) {
    fetch(form.getAttribute("action"), {
      method: "POST",
      body: new URLSearchParams(new FormData(form)),
      credentials: "same-origin",
      headers: { "X-Requested-With": "fetch" }
    }).then(function (resp) {
      if (resp.status === 403) {
        throw new Error("会话已失效，请刷新页面后重试。");
      }
      return resp.json();
    }).then(function (data) {
      show(form, !!data.ok, data.message || "");
      if (!data.done) {
        setTimeout(function () { poll(form); }, 1000);
      }
    }).catch(function (err) {
      show(form, false, "请求失败：" + err.message + " 稍后自动重试。");
      setTimeout(function () { poll(form); }, 5000);
    });
  }

  document.addEventListener("click", function (ev) {
    var btn = ev.target.closest ? ev.target.closest("button[data-async]") : null;
    if (!btn || !btn.form) {
      return;
    }
    ev.preventDefault();
    var form = btn.form;
    var body = new URLSearchParams(new FormData(form));
    if (btn.name) {
      body.set(btn.name, btn.value);
    }
    var label = btn.textContent;
    btn.disabled = true;
    btn.textContent = "测试中…";
    show(form, true, "正在测试，请稍候…");
    fetch(btn.getAttribute("formaction") || form.getAttribute("action"), {
      method: "POST",
      body: body,
      credentials: "same-origin",
      headers: { "X-Requested-With": "fetch" }
    }).then(function (resp) {
      if (resp.status === 403) {
        throw new Error("会话已失效，请刷新页面后重试。");
      }
      return resp.json();
    }).then(function (data) {
      show(form, !!data.ok, data.message || (data.ok ? "成功" : "失败"));
    }).catch(function (err) {
      show(form, false, "请求失败：" + err.message);
    }).then(function () {
      btn.disabled = false;
      btn.textContent = label;
    });
  });

  // ---- 按字段的值显示或隐藏 ----

  function showIf(form) {
    if (!form || !form.querySelectorAll) {
      return;
    }
    var els = form.querySelectorAll("[data-show-if]");
    for (var i = 0; i < els.length; i++) {
      var rule = els[i].getAttribute("data-show-if");
      var eq = rule.indexOf("=");
      var field = form.elements[rule.slice(0, eq)];
      var allowed = rule.slice(eq + 1).split(",");
      els[i].hidden = !field || allowed.indexOf(field.value) < 0;
    }
  }

  document.addEventListener("change", function (ev) {
    if (ev.target && ev.target.form) {
      showIf(ev.target.form);
    }
  });

  // ---- 弹窗表单 ----

  function openDialog(btn) {
    var dlg = document.getElementById(btn.getAttribute("data-dialog"));
    if (!dlg || !dlg.showModal) {
      return;
    }
    var form = dlg.querySelector("form");
    form.reset();
    var old = form.querySelectorAll(".async-result");
    for (var i = 0; i < old.length; i++) {
      old[i].parentNode.removeChild(old[i]);
    }
    var locked = form.querySelectorAll("[data-locked]");
    for (i = 0; i < locked.length; i++) {
      locked[i].readOnly = false;
      locked[i].removeAttribute("data-locked");
    }
    // 把这一行的现有值填进表单。多选框与复选框组的值用逗号分隔。
    var attrs = btn.attributes;
    for (i = 0; i < attrs.length; i++) {
      if (attrs[i].name.indexOf("data-v-") !== 0) {
        continue;
      }
      var fields = form.querySelectorAll('[name="' + attrs[i].name.slice(7) + '"]');
      var vals = attrs[i].value.split(",");
      for (var j = 0; j < fields.length; j++) {
        var f = fields[j];
        if (f.type === "checkbox" || f.type === "radio") {
          f.checked = vals.indexOf(f.value) >= 0;
        } else {
          f.value = attrs[i].value;
        }
      }
    }
    var lock = btn.getAttribute("data-lock");
    if (lock && form.elements[lock]) {
      form.elements[lock].readOnly = true;
      form.elements[lock].setAttribute("data-locked", "");
    }
    showIf(form);
    var title = dlg.querySelector(".modal-title");
    if (title && btn.getAttribute("data-title")) {
      title.textContent = btn.getAttribute("data-title");
    }
    dlg.showModal();
    var first = form.querySelector("input:not([type=hidden]):not([readonly]):not([type=checkbox]),select,textarea");
    if (first) {
      first.focus();
    }
  }

  // 在弹窗里按下鼠标、拖选到外面松开，也会在弹窗本身上触发 click，所以要两头都落在背景上才算点了背景。
  var downOn = null;
  document.addEventListener("mousedown", function (ev) { downOn = ev.target; });
  document.addEventListener("click", function (ev) {
    var t = ev.target;
    if (!t.closest) {
      return;
    }
    var opener = t.closest("[data-dialog]");
    if (opener) {
      ev.preventDefault();
      openDialog(opener);
      return;
    }
    var closer = t.closest("[data-close]");
    if (closer) {
      closer.closest("dialog").close();
      return;
    }
    if (t.tagName === "DIALOG" && t.classList.contains("modal") && downOn === t) {
      t.close();
    }
  });

  // ---- 页面载入与内容区刷新后都要做的初始化 ----

  function init(root) {
    var forms = root.querySelectorAll("form");
    for (var f = 0; f < forms.length; f++) {
      showIf(forms[f]);
    }
    var polled = root.querySelectorAll("form[data-poll]");
    for (var i = 0; i < polled.length; i++) {
      poll(polled[i]);
    }
    // 给表格单元格写上列名：手机上表格按一行一张卡片显示时，用它标注每一项。
    var tables = root.querySelectorAll("table");
    for (i = 0; i < tables.length; i++) {
      var heads = [];
      var ths = tables[i].querySelectorAll("thead th");
      for (var h = 0; h < ths.length; h++) {
        heads.push(ths[h].textContent.trim());
      }
      var rows = tables[i].querySelectorAll("tbody tr");
      for (var r = 0; r < rows.length; r++) {
        var cells = rows[r].children;
        for (var c = 0; c < cells.length; c++) {
          var name = heads[c] || "";
          if (name === "操作") {
            cells[c].classList.add("act");
            name = "";
          }
          cells[c].setAttribute("data-label", name);
        }
      }
    }
  }
  init(document);

  // ---- 提示 ----

  function toast(ok, text) {
    var box = document.getElementById("toasts");
    if (!box) {
      box = document.createElement("div");
      box.id = "toasts";
      box.setAttribute("role", "status");
      document.body.appendChild(box);
    }
    var t = document.createElement("div");
    t.className = "toast " + (ok ? "ok" : "bad");
    t.textContent = text;
    box.appendChild(t);
    setTimeout(function () { t.classList.add("gone"); }, ok ? 4000 : 9000);
    setTimeout(function () { if (t.parentNode) { t.parentNode.removeChild(t); } }, ok ? 4400 : 9400);
  }

  // ---- 后台提交与局部刷新 ----

  // 刷新内容区：重新取当前页，只换 .wrap 里的内容。
  // 换之前记下展开的折叠区、别处已改过但没提交的输入，换完放回去；滚动位置原样保留。
  function refresh() {
    var wrap = document.querySelector(".wrap");
    var url = new URL(location.href);
    url.searchParams.delete("ok");
    url.searchParams.delete("e");
    return fetch(url.toString(), { credentials: "same-origin" }).then(function (resp) {
      if (resp.redirected && resp.url.indexOf(url.pathname) < 0) {
        location.href = resp.url;
        return null;
      }
      return resp.text();
    }).then(function (html) {
      if (html === null) {
        return;
      }
      var doc = new DOMParser().parseFromString(html, "text/html");
      var fresh = doc.querySelector(".wrap");
      if (!wrap || !fresh) {
        location.reload();
        return;
      }
      var y = window.scrollY;
      var opened = [];
      var details = wrap.querySelectorAll("details");
      for (var i = 0; i < details.length; i++) {
        if (details[i].open) {
          opened.push(i);
        }
      }
      var typed = {};
      var fields = wrap.querySelectorAll("input[id],textarea[id],select[id]");
      for (i = 0; i < fields.length; i++) {
        var f = fields[i];
        if (f.type === "hidden" || f.type === "file") {
          continue;
        }
        if (f.type === "checkbox" || f.type === "radio") {
          if (f.checked !== f.defaultChecked) {
            typed[f.id] = f.checked;
          }
        } else if (f.value !== f.defaultValue) {
          typed[f.id] = f.value;
        }
      }
      wrap.innerHTML = fresh.innerHTML;
      details = wrap.querySelectorAll("details");
      for (i = 0; i < opened.length; i++) {
        if (details[opened[i]]) {
          details[opened[i]].open = true;
        }
      }
      for (var id in typed) {
        var el = document.getElementById(id);
        if (el) {
          if (el.type === "checkbox" || el.type === "radio") {
            el.checked = typed[id];
          } else {
            el.value = typed[id];
          }
        }
      }
      init(wrap);
      window.scrollTo(0, y);
    });
  }

  document.addEventListener("submit", function (ev) {
    var form = ev.target;
    if (ev.defaultPrevented || !form.getAttribute || (form.getAttribute("method") || "").toLowerCase() !== "post") {
      return;
    }
    var btn = ev.submitter || null;
    var action = (btn && btn.getAttribute("formaction")) || form.getAttribute("action") || "";
    if (!action || form.hasAttribute("data-poll") || (form.enctype || "").indexOf("multipart") >= 0 ||
        /^\/(login|logout|restart)/.test(action)) {
      return;
    }
    ev.preventDefault();
    var body = new URLSearchParams(new FormData(form));
    if (btn && btn.name) {
      body.set(btn.name, btn.value);
    }
    var label = btn ? btn.textContent : "";
    if (btn) {
      btn.disabled = true;
      btn.textContent = "处理中…";
    }
    var dlg = form.closest ? form.closest("dialog") : null;
    var old = form.querySelector(".async-result");
    if (old) {
      old.parentNode.removeChild(old);
    }
    fetch(action, {
      method: "POST",
      body: body,
      credentials: "same-origin",
      headers: { "X-Requested-With": "fetch" }
    }).then(function (resp) {
      if (resp.status === 403) {
        throw new Error("会话已失效，请刷新页面后重试。");
      }
      if ((resp.headers.get("Content-Type") || "").indexOf("json") < 0) {
        // 不是后台应答（登录失效、跳转到别的页面等）：照普通提交处理。
        location.href = resp.url;
        return null;
      }
      return resp.json();
    }).then(function (data) {
      if (!data) {
        return;
      }
      var text = data.message || (data.ok ? "已完成" : "操作失败");
      if (!data.ok) {
        if (dlg) {
          show(form, false, text);
        } else {
          toast(false, text);
        }
        return;
      }
      toast(true, text);
      if (dlg) {
        dlg.close();
      }
      return refresh();
    }).catch(function (err) {
      var text = "请求失败：" + err.message;
      if (dlg) {
        show(form, false, text);
      } else {
        toast(false, text);
      }
    }).then(function () {
      if (btn && document.contains(btn)) {
        btn.disabled = false;
        btn.textContent = label;
      }
    });
  });
})();
