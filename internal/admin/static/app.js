// 管理后台各页共用的一段脚本，做两件事：
//   - 让带 data-async 的按钮在后台提交表单，结果显示在按钮所在的表单里，页面不刷新——
//     点「连接测试」之后，表单里已经填的内容都还在。
//   - 让「全选」勾选框一次勾上表单里的全部条目。
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
})();
