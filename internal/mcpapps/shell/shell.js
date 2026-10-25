// zulip-acp MCP Apps host shell.
//
// The page holds one app in a sandboxed iframe and plays the HOST side
// of the MCP Apps postMessage protocol (JSON-RPC 2.0). Requests the
// host cannot answer itself go to the relay over a WebSocket.
(function () {
  "use strict";

  var m = location.pathname.match(/^\/a\/([^/]+)$/);
  var token = new URLSearchParams(location.search).get("t");
  var store = m ? "zulip-acp-app:" + m[1] : "";
  try {
    if (token) { sessionStorage.setItem(store, token); } else if (m) { token = sessionStorage.getItem(store); }
  } catch (e) { /* storage off: the link still works once */ }
  var statusEl = document.getElementById("status");
  var titleEl = document.getElementById("title");

  function status(text, isError) {
    statusEl.textContent = text;
    statusEl.className = isError ? "error" : "";
  }
  if (!m || !token) {
    status("bad link", true);
    return;
  }
  // Keep the token out of the address bar, history and any referrer.
  history.replaceState(null, "", location.pathname);

  var wsURL = (location.protocol === "https:" ? "wss://" : "ws://") + location.host +
    "/ws/" + encodeURIComponent(m[1]) + "?t=" + encodeURIComponent(token);
  var ws = new WebSocket(wsURL);
  var nextID = 1;
  var pending = {};
  var frame = null;
  var info = null;
  var appReady = false;

  function relay(method, params) {
    return new Promise(function (resolve, reject) {
      if (ws.readyState !== WebSocket.OPEN) {
        reject(new Error("not connected to the relay"));
        return;
      }
      var id = nextID++;
      pending[id] = { resolve: resolve, reject: reject };
      ws.send(JSON.stringify({ id: id, method: method, params: params }));
    });
  }

  ws.onmessage = function (ev) {
    var msg;
    try { msg = JSON.parse(ev.data); } catch (e) { return; }
    if (msg.id && pending[msg.id]) {
      var p = pending[msg.id];
      delete pending[msg.id];
      if (msg.error) { p.reject(new Error(msg.error.message)); } else { p.resolve(msg.result); }
      return;
    }
    if (msg.method === "tool-result" && info) {
      info.result = msg.params;
      if (appReady) { notify("ui/notifications/tool-result", info.result); }
    }
  };
  ws.onclose = function () {
    status("disconnected — reload the page to reconnect", true);
    Object.keys(pending).forEach(function (id) {
      pending[id].reject(new Error("disconnected from the relay"));
      delete pending[id];
    });
  };
  ws.onopen = function () {
    relay("init", {}).then(start, function (e) { status(e.message, true); });
  };

  function start(i) {
    info = i;
    document.title = i.label;
    titleEl.textContent = i.label;
    status("ready");
    frame = document.createElement("iframe");
    // No allow-same-origin: the app gets an opaque origin and cannot
    // reach this page, its token or the relay.
    frame.setAttribute("sandbox", "allow-scripts allow-forms");
    frame.setAttribute("referrerpolicy", "no-referrer");
    frame.src = i.contentUrl;
    document.getElementById("main").appendChild(frame);
  }

  function post(msg) {
    // The app's origin is opaque ("null"), so "*" is the only target
    // that reaches it. The window is ours, so nothing else receives it.
    if (frame && frame.contentWindow) { frame.contentWindow.postMessage(msg, "*"); }
  }
  function reply(id, result) { post({ jsonrpc: "2.0", id: id, result: result }); }
  function fail(id, code, message) { post({ jsonrpc: "2.0", id: id, error: { code: code, message: message } }); }
  function notify(method, params) { post({ jsonrpc: "2.0", method: method, params: params }); }

  function theme() {
    return window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
  }

  function hostContext() {
    return {
      theme: theme(),
      displayMode: "inline",
      availableDisplayModes: ["inline"],
      platform: /Mobi|Android|iPhone|iPad/.test(navigator.userAgent) ? "mobile" : "web",
      locale: navigator.language,
      timeZone: Intl.DateTimeFormat().resolvedOptions().timeZone,
      viewport: { width: window.innerWidth, height: window.innerHeight }
    };
  }

  function sendToolData() {
    if (info.input) {
      var args = info.input;
      if (typeof args === "object" && args !== null && !Array.isArray(args)) {
        notify("ui/notifications/tool-input", { arguments: args });
      }
    }
    if (info.result) { notify("ui/notifications/tool-result", info.result); }
  }

  var handlers = {
    "ui/initialize": function (params) {
      return Promise.resolve({
        protocolVersion: (params && params.protocolVersion) || "2025-06-18",
        hostInfo: { name: "zulip-acp", version: info.hostVersion || "" },
        hostCapabilities: { openLinks: {}, serverTools: {}, logging: {} },
        hostContext: hostContext()
      });
    },
    "tools/call": function (params) { return relay("tools/call", params); },
    "ui/message": function (params) {
      return relay("ui/message", params).then(function () { status("message sent to the agent"); return {}; });
    },
    "ui/open-link": function (params) {
      var url;
      try { url = new URL(params.url); } catch (e) { return Promise.reject(new Error("bad URL")); }
      if (url.protocol !== "https:" && url.protocol !== "http:") {
        return Promise.reject(new Error("only http and https links can be opened"));
      }
      window.open(url.href, "_blank", "noopener,noreferrer");
      return Promise.resolve({});
    },
    "ping": function () { return Promise.resolve({}); }
  };

  window.addEventListener("message", function (ev) {
    if (!frame || ev.source !== frame.contentWindow) { return; }
    var msg = ev.data;
    if (!msg || msg.jsonrpc !== "2.0" || typeof msg.method !== "string") { return; }
    var params = msg.params || {};
    if (msg.id === undefined || msg.id === null) {
      switch (msg.method) {
        case "ui/notifications/initialized":
          appReady = true;
          sendToolData();
          break;
        case "ui/notifications/size-changed":
          if (typeof params.height === "number" && params.height > 0) {
            frame.style.height = Math.min(Math.ceil(params.height), 10000) + "px";
          }
          break;
      }
      return;
    }
    var h = handlers[msg.method];
    if (!h) {
      fail(msg.id, -32601, "method not found: " + msg.method);
      return;
    }
    h(params).then(function (r) { reply(msg.id, r); }, function (e) { fail(msg.id, -32000, e.message); });
  });

  window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").addEventListener("change", function () {
    if (appReady) { notify("ui/notifications/host-context-changed", { theme: theme() }); }
  });
})();
