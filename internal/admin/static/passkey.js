// 通行密钥：登记与登录。只在登录页和「登录与安全」页加载。
//
// 通行密钥必须由浏览器里的脚本向设备（指纹、面容、硬件密钥）发起，这一步没法靠纯表单完成。
// 脚本只和后台自己通信；设备返回的结果原样交给后台校验，这里不做任何判断。
(function () {
  "use strict";

  function toBytes(text) {
    var b64 = text.replace(/-/g, "+").replace(/_/g, "/");
    while (b64.length % 4) { b64 += "="; }
    var raw = atob(b64);
    var out = new Uint8Array(raw.length);
    for (var i = 0; i < raw.length; i++) { out[i] = raw.charCodeAt(i); }
    return out.buffer;
  }

  function toText(buffer) {
    var bytes = new Uint8Array(buffer);
    var raw = "";
    for (var i = 0; i < bytes.length; i++) { raw += String.fromCharCode(bytes[i]); }
    return btoa(raw).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  }

  function show(box, ok, text) {
    if (!box) { return; }
    box.hidden = false;
    box.className = "async-result " + (ok ? "ok" : "bad");
    box.textContent = text;
  }

  function post(url, body, json) {
    var headers = { "X-Requested-With": "fetch" };
    if (json) { headers["Content-Type"] = "application/json"; }
    return fetch(url, { method: "POST", body: body, credentials: "same-origin", headers: headers })
      .then(function (resp) {
        return resp.json().catch(function () {
          throw new Error(resp.status === 403 ? "会话已失效，请刷新页面后重试。" : "服务器返回了无法识别的内容。");
        });
      })
      .then(function (data) {
        if (!data.ok) { throw new Error(data.message || "操作失败"); }
        return data;
      });
  }

  // 设备那边的失败（用户取消、超时、没有可用的密钥）给一句看得懂的话。
  function explain(err) {
    if (err && err.name === "NotAllowedError") { return "操作已取消，或设备没有在限定时间内完成验证。"; }
    if (err && err.name === "InvalidStateError") { return "这台设备已经登记过了。"; }
    if (err && err.name === "SecurityError") { return "当前访问地址不能使用通行密钥，请使用 HTTPS 或 localhost。"; }
    return (err && err.message) || "操作失败";
  }

  function supported(box) {
    if (window.PublicKeyCredential && navigator.credentials) { return true; }
    show(box, false, "这个浏览器不支持通行密钥，或者当前访问地址不是 HTTPS 或 localhost。");
    return false;
  }

  function register(btn) {
    var form = btn.form;
    var box = form.querySelector(".async-result");
    if (!supported(box)) { return; }
    var csrf = form.querySelector("input[name=csrf]").value;
    btn.disabled = true;
    show(box, true, "请按照浏览器或设备的提示完成验证…");
    post("/security/passkey/begin", new URLSearchParams(new FormData(form)))
      .then(function (begin) {
        var pk = begin.options.publicKey;
        pk.challenge = toBytes(pk.challenge);
        pk.user.id = toBytes(pk.user.id);
        (pk.excludeCredentials || []).forEach(function (c) { c.id = toBytes(c.id); });
        return navigator.credentials.create({ publicKey: pk }).then(function (cred) {
          var body = {
            id: cred.id, rawId: toText(cred.rawId), type: cred.type,
            response: {
              attestationObject: toText(cred.response.attestationObject),
              clientDataJSON: toText(cred.response.clientDataJSON),
              transports: cred.response.getTransports ? cred.response.getTransports() : []
            },
            clientExtensionResults: cred.getClientExtensionResults ? cred.getClientExtensionResults() : {}
          };
          if (cred.authenticatorAttachment) { body.authenticatorAttachment = cred.authenticatorAttachment; }
          return post("/security/passkey/finish?sid=" + encodeURIComponent(begin.sid) + "&csrf=" + encodeURIComponent(csrf),
            JSON.stringify(body), true);
        });
      })
      .then(function () { window.location.assign("/security?ok=" + encodeURIComponent("通行密钥已登记。")); })
      .catch(function (err) { show(box, false, explain(err)); btn.disabled = false; });
  }

  function login(btn) {
    var box = document.querySelector(".passkey-result");
    if (!supported(box)) { return; }
    btn.disabled = true;
    show(box, true, "请按照浏览器或设备的提示完成验证…");
    post("/login/passkey/begin", new URLSearchParams())
      .then(function (begin) {
        var pk = begin.options.publicKey;
        pk.challenge = toBytes(pk.challenge);
        (pk.allowCredentials || []).forEach(function (c) { c.id = toBytes(c.id); });
        return navigator.credentials.get({ publicKey: pk }).then(function (cred) {
          var body = {
            id: cred.id, rawId: toText(cred.rawId), type: cred.type,
            response: {
              authenticatorData: toText(cred.response.authenticatorData),
              clientDataJSON: toText(cred.response.clientDataJSON),
              signature: toText(cred.response.signature),
              userHandle: cred.response.userHandle ? toText(cred.response.userHandle) : null
            },
            clientExtensionResults: cred.getClientExtensionResults ? cred.getClientExtensionResults() : {}
          };
          return post("/login/passkey/finish?sid=" + encodeURIComponent(begin.sid), JSON.stringify(body), true);
        });
      })
      .then(function (done) { window.location.assign(done.to || "/"); })
      .catch(function (err) { show(box, false, explain(err)); btn.disabled = false; });
  }

  document.addEventListener("click", function (ev) {
    var btn = ev.target.closest ? ev.target.closest("button[data-passkey]") : null;
    if (!btn) { return; }
    ev.preventDefault();
    if (btn.getAttribute("data-passkey") === "register") { register(btn); } else { login(btn); }
  });
})();
