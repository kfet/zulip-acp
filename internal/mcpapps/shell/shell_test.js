// Behaviour test for shell.js under Node.js, with the browser APIs it
// uses replaced by small fakes. Run by TestShellJSBehaviour.
"use strict";
const fs = require("fs");
const path = require("path");
const vm = require("vm");
const assert = require("assert");

const sent = [];      // frames the shell sent to the relay
const toApp = [];     // messages the shell posted to the app iframe
const opened = [];    // window.open calls
const listeners = {};
let ws = null;

class FakeWS {
  constructor(url) { this.url = url; this.readyState = 0; ws = this; }
  send(s) { sent.push(JSON.parse(s)); }
}
FakeWS.OPEN = 1;

const contentWindow = { postMessage: (m, target) => { assert.strictEqual(target, "*"); toApp.push(m); } };
const elements = {
  status: { textContent: "", className: "" },
  title: { textContent: "" },
  main: { children: [], appendChild(c) { this.children.push(c); } },
};
const document = {
  title: "",
  getElementById: (id) => elements[id],
  createElement: () => ({ attrs: {}, style: {}, contentWindow, setAttribute(k, v) { this.attrs[k] = v; } }),
};
const store = {};
const ctx = {
  location: { pathname: "/a/ID1", search: "?t=TOK", protocol: "https:", host: "apps.example.com" },
  URLSearchParams, URL, Promise, JSON, Math, Object, Error, Intl,
  sessionStorage: { setItem: (k, v) => { store[k] = v; }, getItem: (k) => store[k] },
  history: { replaceState: (a, b, p) => { ctx.location.search = ""; ctx.replaced = p; } },
  document,
  navigator: { userAgent: "Node", language: "en" },
  WebSocket: FakeWS,
  window: {
    innerWidth: 800, innerHeight: 600,
    addEventListener: (ev, fn) => { listeners[ev] = fn; },
    open: (u, t, f) => opened.push([u, t, f]),
    matchMedia: () => ({ matches: true, addEventListener() {} }),
  },
};
vm.runInNewContext(fs.readFileSync(path.join(__dirname, "shell.js"), "utf8"), ctx);

const eq = (a, b) => assert.strictEqual(JSON.stringify(a), JSON.stringify(b));
const tick = () => new Promise((r) => setImmediate(r));
const fromApp = (data, source = contentWindow) => listeners.message({ source, data });
const relayReply = (id, body) => ws.onmessage({ data: JSON.stringify(Object.assign({ id }, body)) });

(async () => {
  assert.strictEqual(ws.url, "wss://apps.example.com/ws/ID1?t=TOK");
  assert.strictEqual(store["zulip-acp-app:ID1"], "TOK");
  assert.strictEqual(ctx.replaced, "/a/ID1");

  ws.readyState = 1;
  ws.onopen();
  eq(sent[0], { id: 1, method: "init", params: {} });
  relayReply(1, { result: { label: "demo", contentUrl: "https://apps.example.com/c/ID1?t=C", hostVersion: "9",
    input: { n: 1 }, result: null } });
  await tick();
  const frame = elements.main.children[0];
  assert.strictEqual(frame.attrs.sandbox, "allow-scripts allow-forms");
  assert.strictEqual(frame.src, "https://apps.example.com/c/ID1?t=C");
  assert.strictEqual(document.title, "demo");

  // A message from anything but the iframe is ignored.
  fromApp({ jsonrpc: "2.0", id: 1, method: "ping" }, {});
  fromApp({ jsonrpc: "1.0", id: 1, method: "ping" });
  assert.strictEqual(toApp.length, 0);

  fromApp({ jsonrpc: "2.0", id: 1, method: "ui/initialize", params: { protocolVersion: "2026-01-26" } });
  await tick();
  assert.strictEqual(toApp[0].id, 1);
  assert.strictEqual(toApp[0].result.protocolVersion, "2026-01-26");
  assert.strictEqual(toApp[0].result.hostInfo.version, "9");
  assert.strictEqual(toApp[0].result.hostContext.theme, "dark");

  // The tool result arrives before the app is ready: held until then.
  ws.onmessage({ data: JSON.stringify({ method: "tool-result", params: { content: [] } }) });
  assert.strictEqual(toApp.length, 1);
  fromApp({ jsonrpc: "2.0", method: "ui/notifications/initialized" });
  eq(toApp[1], { jsonrpc: "2.0", method: "ui/notifications/tool-input", params: { arguments: { n: 1 } } });
  assert.strictEqual(toApp[2].method, "ui/notifications/tool-result");

  fromApp({ jsonrpc: "2.0", id: 2, method: "tools/call", params: { name: "t", arguments: {} } });
  eq(sent[1], { id: 2, method: "tools/call", params: { name: "t", arguments: {} } });
  relayReply(2, { result: { content: [{ type: "text", text: "ok" }] } });
  await tick();
  assert.strictEqual(toApp[3].id, 2);
  assert.strictEqual(toApp[3].result.content[0].text, "ok");

  fromApp({ jsonrpc: "2.0", id: 3, method: "ui/message", params: { role: "user", content: [] } });
  relayReply(3, { error: { message: "too many" } });
  await tick(); await tick();
  eq(toApp[4].error, { code: -32000, message: "too many" });

  fromApp({ jsonrpc: "2.0", id: 4, method: "ui/open-link", params: { url: "javascript:alert(1)" } });
  fromApp({ jsonrpc: "2.0", id: 5, method: "ui/open-link", params: { url: "https://example.com/x" } });
  fromApp({ jsonrpc: "2.0", id: 6, method: "nope" });
  await tick();
  const byID = (id) => toApp.find((m) => m.id === id);
  assert.ok(byID(4).error);
  eq(byID(5).result, {});
  eq(opened, [["https://example.com/x", "_blank", "noopener,noreferrer"]]);
  assert.strictEqual(byID(6).error.code, -32601);

  fromApp({ jsonrpc: "2.0", method: "ui/notifications/size-changed", params: { height: 321.4 } });
  assert.strictEqual(frame.style.height, "322px");

  // A disconnect fails what is pending and later requests.
  fromApp({ jsonrpc: "2.0", id: 7, method: "tools/call", params: { name: "t" } });
  ws.readyState = 3;
  ws.onclose();
  fromApp({ jsonrpc: "2.0", id: 8, method: "tools/call", params: { name: "t" } });
  await tick(); await tick();
  assert.ok(byID(7).error && byID(8).error);
  assert.strictEqual(elements.status.className, "error");
  console.log("shell.js behaviour OK");
})().catch((e) => { console.error(e); process.exit(1); });
