"use strict";
const { spawn } = require("child_process");
const fs=require("fs"),os=require("os"),path=require("path"),http=require("http");
const SCENE=process.argv[2]||"11xygtvd",REGION=process.argv[3]||"cn",PREFIX=process.argv[4]||"no8xfe";
const CHROME=process.env.CHROME_PATH||findChrome();
const PORT=9200+Math.floor(Math.random()*500);
const PROFILE=fs.mkdtempSync(path.join(os.tmpdir(),"zc-cap-"));
const LOG=path.join(os.tmpdir(),"zc-solver.log");
function log(s){try{fs.appendFileSync(LOG,s+"\n");}catch{}}
function findChrome(){const c=["C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe","C:\\Program Files (x86)\\Google\\Chrome\\Application\\chrome.exe",process.env.LOCALAPPDATA+"\\Google\\Chrome\\Application\\chrome.exe","C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe","/usr/bin/google-chrome","/usr/bin/chromium"];for(const p of c){try{if(fs.existsSync(p))return p;}catch{}}return "chrome";}
const args=["--disable-gpu","--no-sandbox","--no-first-run","--disable-blink-features=AutomationControlled","--disable-dev-shm-usage","--window-size=1280,900","--remote-debugging-port="+PORT,"--user-data-dir="+PROFILE,"--no-default-browser-check","about:blank"];
log("start chrome="+CHROME);
const child=spawn(CHROME,args,{stdio:["ignore","ignore","ignore"]});
let done=false;
function finish(code,out){if(done)return;done=true;if(out)process.stdout.write(out+"\n");try{child.kill("SIGKILL");}catch{}setTimeout(()=>{try{fs.rmSync(PROFILE,{recursive:true,force:true});}catch{}process.exit(code);},150);}
function httpJSON(p,method){return new Promise((res,rej)=>{const req=http.request({host:"127.0.0.1",port:PORT,path:p,method:method||"GET"},r=>{let b="";r.on("data",d=>b+=d);r.on("end",()=>{try{res(JSON.parse(b));}catch(e){rej(new Error("badjson"));}});});req.on("error",rej);req.end();});}
async function waitPort(){for(let i=0;i<120;i++){try{return await httpJSON("/json/version");}catch{await new Promise(r=>setTimeout(r,100));}}throw new Error("chrome not up");}
(async()=>{
  await waitPort();log("port up");
  const tgt=await httpJSON("/json/new?about:blank","PUT");
  const ws=new WebSocket(tgt.webSocketDebuggerUrl);
  let id=0;const pending=new Map();
  const send=(method,params)=>new Promise((res,rej)=>{const mid=++id;pending.set(mid,{res,rej});ws.send(JSON.stringify({id:mid,method,params:params||{}}));});
  ws.onmessage=ev=>{const m=JSON.parse(ev.data);
    if(m.id&&pending.has(m.id)){const p=pending.get(m.id);pending.delete(m.id);m.error?p.rej(new Error(JSON.stringify(m.error))):p.res(m.result);}
    if(m.method==="Runtime.consoleAPICalled"){const txt=(m.params.args||[]).map(a=>a.value===undefined?JSON.stringify(a.preview||""):String(a.value)).join(" ");log("con:"+txt.slice(0,300));
      if(txt.startsWith("VERIFY_PARAM="))finish(0,txt);
      if(txt.startsWith("FAIL=")||txt.startsWith("BOOT_ERR=")||txt.startsWith("TTV_ERR="))finish(4,"FAIL");}};
  await new Promise((res,rej)=>{ws.onopen=res;ws.onerror=()=>rej(new Error("ws error"));});
  await send("Page.enable");await send("Runtime.enable");
  await send("Page.navigate",{url:"https://zcode.z.ai/cn/rewards"}).catch(()=>{});
  await new Promise(r=>setTimeout(r,2000));
  const page="<!DOCTYPE html><html><head><meta charset=utf-8></head><body><div id=cap></div><button id=btn>go</button><script>\n"+
  "window.AliyunCaptchaConfig={region:'"+REGION+"',prefix:'"+PREFIX+"'};\n"+
  "function loadSdk(){return new Promise(function(res,rej){var s=document.createElement('script');s.src='https://o.alicdn.com/captcha-frontend/aliyunCaptcha/AliyunCaptcha.js';s.onload=res;s.onerror=rej;document.head.appendChild(s);});}\n"+
  "(async function(){try{await loadSdk();console.log('SDK_READY');\n"+
  "window.initAliyunCaptcha({SceneId:'"+SCENE+"',mode:'popup',region:'"+REGION+"',prefix:'"+PREFIX+"',element:'#cap',button:'#btn',showErrorTip:false,\n"+
  "getInstance:function(inst){try{console.log('GOT_INSTANCE');inst.startTracelessVerification();}catch(e){console.log('TTV_ERR='+e.message);}},\n"+
  "success:function(p){console.log('VERIFY_PARAM='+p);},\n"+
  "fail:function(e){console.log('FAIL='+JSON.stringify(e));},\n"+
  "onError:function(e){console.log('ONERROR='+(e&&e.message));}});\n"+
  "}catch(e){console.log('BOOT_ERR='+e.message);}})();\n<\/script></body></html>";
  await send("Runtime.evaluate",{expression:"document.open();document.write("+JSON.stringify(page)+");document.close();",awaitPromise:false});
  log("injected");
  setTimeout(()=>{if(!done){log("timeout");finish(2,"");}},26000);
})().catch(e=>{log("ERR "+e.message);finish(3,"");});

