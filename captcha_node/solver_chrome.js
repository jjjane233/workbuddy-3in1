"use strict";
/* ZCode 阿里云无痕验证求解器（CDP 驱动真实 Chrome）。
 *
 *   node solver_chrome.js <sceneId> <region> <prefix>
 *   成功输出一行：VERIFY_PARAM=<param>
 *   失败：退出码非 0（stderr 输出 FAIL/BOE_ERR/TTV_ERR 关键字）。
 *
 * 环境变量：
 *   CHROME_PATH     指定 Chrome/Chromium 可执行文件
 *   CHROME_HEADLESS 设为 1 时启用 --headless=new（默认有头；本机实测 headless 会被判 F001）
 *   CAPTCHA_TIMEOUT 求解超时毫秒（默认 30000）
 */
const { spawn } = require("child_process");
// Node < 22 无全局 WebSocket，退回 ws 包（npm i ws）。
let WSImpl = typeof WebSocket !== "undefined" ? WebSocket : null;
if (!WSImpl) { try { WSImpl = require("ws"); } catch (e) {} }
if (!WSImpl) { try { WSImpl = require("undici").WebSocket; } catch (e) {} }
const fs = require("fs"), os = require("os"), path = require("path"), http = require("http");

const SCENE = process.argv[2] || process.env.CAPTCHA_SCENE || "11xygtvd";
const REGION = process.argv[3] || process.env.CAPTCHA_REGION || "cn";
const PREFIX = process.argv[4] || process.env.CAPTCHA_PREFIX || "no8xfe";
const CHROME = process.env.CHROME_PATH || findChrome();
const HEADLESS = process.env.CHROME_HEADLESS === "1";
const TIMEOUT = parseInt(process.env.CAPTCHA_TIMEOUT || "30000", 10);
const PORT = 9200 + Math.floor(Math.random() * 500);
const PROFILE = fs.mkdtempSync(path.join(os.tmpdir(), "zc-cap-"));

function log(s) { try { process.stderr.write("[solver] " + s + "\n"); } catch (e) {} }

function findChrome() {
  const cands = [
    process.env.LOCALAPPDATA + "\\Google\\Chrome\\Application\\chrome.exe",
    "C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe",
    "C:\\Program Files (x86)\\Google\\Chrome\\Application\\chrome.exe",
    "C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe",
    "C:\\Program Files\\Microsoft\\Edge\\Application\\msedge.exe",
    "/usr/bin/google-chrome", "/usr/bin/google-chrome-stable", "/usr/bin/chromium",
    "/usr/bin/chromium-browser", "/snap/bin/chromium",
  ];
  for (const p of cands) { try { if (p && fs.existsSync(p)) return p; } catch (e) {} }
  return "google-chrome";
}

const args = [
  "--disable-gpu", "--no-sandbox", "--no-first-run", "--no-default-browser-check",
  "--disable-blink-features=AutomationControlled",
  "--disable-features=IsolateOrigins,site-per-process",
  "--disable-dev-shm-usage", "--window-size=1280,900",
  "--remote-debugging-port=" + PORT, "--user-data-dir=" + PROFILE,
  "--lang=zh-CN", "about:blank",
];
if (HEADLESS) args.unshift("--headless=new");
log("chrome=" + CHROME + " headless=" + HEADLESS + " scene=" + SCENE + " region=" + REGION);

const child = spawn(CHROME, args, { stdio: ["ignore", "ignore", "ignore"] });
let done = false;
function finish(code, out) {
  if (done) return; done = true;
  if (out) process.stdout.write(out + "\n");
  try { child.kill("SIGKILL"); } catch (e) {}
  setTimeout(() => { try { fs.rmSync(PROFILE, { recursive: true, force: true }); } catch (e) {} process.exit(code); }, 150);
}
function httpJSON(p, method) {
  return new Promise((res, rej) => {
    const req = http.request({ host: "127.0.0.1", port: PORT, path: p, method: method || "GET" }, r => {
      let b = ""; r.on("data", d => b += d); r.on("end", () => { try { res(JSON.parse(b)); } catch (e) { rej(new Error("badjson")); } });
    });
    req.on("error", rej); req.end();
  });
}
async function waitPort() {
  for (let i = 0; i < 150; i++) { try { return await httpJSON("/json/version"); } catch (e) { await new Promise(r => setTimeout(r, 100)); } }
  throw new Error("chrome not up");
}

// 反自动化检测脚本，在每个新文档注入。
const STEALTH = `
Object.defineProperty(navigator,'webdriver',{get:()=>undefined});
Object.defineProperty(navigator,'languages',{get:()=>['zh-CN','zh','en']});
Object.defineProperty(navigator,'plugins',{get:()=>[1,2,3,4,5]});
window.chrome=window.chrome||{runtime:{},app:{isInstalled:false}};
const origQuery=window.navigator.permissions&&window.navigator.permissions.query;
if(origQuery){window.navigator.permissions.query=(p)=>p&&p.name==='notifications'?Promise.resolve({state:Notification.permission}):origQuery(p);}
Object.defineProperty(navigator,'hardwareConcurrency',{get:()=>8});
Object.defineProperty(navigator,'deviceMemory',{get:()=>8});
`;

(async () => {
  await waitPort(); log("port up");
  const tgt = await httpJSON("/json/new?about:blank", "PUT");
  const ws = new WSImpl(tgt.webSocketDebuggerUrl);
  let id = 0; const pending = new Map();
  const send = (method, params) => new Promise((res, rej) => {
    const mid = ++id; pending.set(mid, { res, rej });
    ws.send(JSON.stringify({ id: mid, method, params: params || {} }));
  });
  ws.onmessage = ev => {
    const m = JSON.parse(ev.data);
    if (m.id && pending.has(m.id)) { const p = pending.get(m.id); pending.delete(m.id); m.error ? p.rej(new Error(JSON.stringify(m.error))) : p.res(m.result); }
    if (m.method === "Runtime.consoleAPICalled") {
      const txt = (m.params.args || []).map(a => a.value === undefined ? JSON.stringify(a.preview || "") : String(a.value)).join(" ");
      log("con:" + txt.slice(0, 220));
      if (txt.startsWith("VERIFY_PARAM=")) finish(0, txt);
      if (txt.startsWith("FAIL=") || txt.startsWith("BOOT_ERR=") || txt.startsWith("TTV_ERR=")) finish(4, "");
    }
  };
  await new Promise((res, rej) => {
    if (typeof ws.onopen !== "undefined" || "onopen" in ws) { ws.onopen = res; ws.onerror = () => rej(new Error("ws error")); }
    else { ws.addEventListener("open", res); ws.addEventListener("error", () => rej(new Error("ws error"))); }
  });
  await send("Page.enable"); await send("Runtime.enable");
  await send("Page.addScriptToEvaluateOnNewDocument", { source: STEALTH }).catch(() => {});
  await send("Page.navigate", { url: "https://zcode.z.ai/cn/rewards" }).catch(() => {});
  await new Promise(r => setTimeout(r, 2500));
  const page =
    "<!DOCTYPE html><html><head><meta charset=utf-8></head><body><div id=cap></div><button id=btn>go</button><script>\n" +
    "window.AliyunCaptchaConfig={region:'" + REGION + "',prefix:'" + PREFIX + "'};\n" +
    "function loadSdk(){return new Promise(function(res,rej){var s=document.createElement('script');s.src='https://o.alicdn.com/captcha-frontend/aliyunCaptcha/AliyunCaptcha.js';s.onload=res;s.onerror=rej;document.head.appendChild(s);});}\n" +
    "(async function(){try{await loadSdk();console.log('SDK_READY');\n" +
    "window.initAliyunCaptcha({SceneId:'" + SCENE + "',mode:'popup',region:'" + REGION + "',prefix:'" + PREFIX + "',element:'#cap',button:'#btn',showErrorTip:false,\n" +
    "getInstance:function(inst){try{console.log('GOT_INSTANCE');inst.startTracelessVerification();}catch(e){console.log('TTV_ERR='+e.message);}},\n" +
    "success:function(p){console.log('VERIFY_PARAM='+p);},\n" +
    "fail:function(e){console.log('FAIL='+JSON.stringify(e));},\n" +
    "onError:function(e){console.log('ONERROR='+(e&&e.message));}});\n" +
    "}catch(e){console.log('BOOT_ERR='+e.message);}})();\n<\/script></body></html>";
  await send("Runtime.evaluate", { expression: "document.open();document.write(" + JSON.stringify(page) + ");document.close();", awaitPromise: false });
  log("injected");
  setTimeout(() => { if (!done) { log("timeout"); finish(2, ""); } }, TIMEOUT);
})().catch(e => { log("ERR " + e.message); finish(3, ""); });
