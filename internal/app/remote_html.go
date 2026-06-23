package app

// remoteHTML is the single-page browser client served by the remote bridge. It
// derives the AES-GCM key from the pairing code with Web Crypto (PBKDF2), so
// nothing readable crosses the wire without the code.
const remoteHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8"/>
<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover"/>
<title>Nocturne — remote</title>
<style>
  :root{
    --bg:#0a0a0f; --panel:#13131d; --panel2:#171723; --line:rgba(255,255,255,.09);
    --ink:#ECECF1; --dim:#9a9aab; --amber:#E0A458; --amber2:#f0c089; --violet:#A78BFA;
    --user:#F0ABFC; --tool:#7AA2F7; --mono:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;
    --sans:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif;
  }
  *{box-sizing:border-box}
  html,body{height:100%;margin:0}
  body{background:var(--bg);color:var(--ink);font-family:var(--sans);
    display:flex;flex-direction:column;
    background:radial-gradient(700px 380px at 80% -10%,rgba(167,139,250,.14),transparent 60%),
               radial-gradient(640px 360px at 5% 0,rgba(224,164,88,.10),transparent 55%),var(--bg);}
  header{display:flex;align-items:center;gap:10px;padding:14px 16px;border-bottom:1px solid var(--line);
    backdrop-filter:blur(8px)}
  .moon{width:20px;height:20px}
  header b{font-weight:700;letter-spacing:-.01em}
  header .st{margin-left:auto;font:600 12px var(--sans);color:var(--dim);display:flex;align-items:center;gap:7px}
  .dot{width:8px;height:8px;border-radius:50%;background:#f2777a;box-shadow:0 0 8px #f2777a}
  .dot.ok{background:#7bd88f;box-shadow:0 0 8px #7bd88f}
  #log{flex:1;overflow-y:auto;padding:18px 16px 8px;display:flex;flex-direction:column;gap:14px}
  .msg{max-width:90%;white-space:pre-wrap;overflow-wrap:anywhere;line-height:1.5;font-size:15px}
  .msg.user{align-self:flex-end;background:linear-gradient(180deg,#1d1726,#191322);
    border:1px solid var(--line);border-radius:14px 14px 4px 14px;padding:10px 13px}
  .msg.user .who{color:var(--user)}
  .msg.assistant{align-self:flex-start}
  .who{font:700 12px var(--sans);margin-bottom:3px;opacity:.85}
  .msg.assistant .who{color:var(--amber)}
  .tool{align-self:flex-start;font:13px var(--mono);color:var(--tool);opacity:.9}
  .status{align-self:center;font:12px var(--sans);color:var(--dim)}
  .bar{display:flex;gap:8px;padding:12px 14px;border-top:1px solid var(--line);background:rgba(10,10,15,.6)}
  textarea{flex:1;resize:none;background:var(--panel);color:var(--ink);border:1px solid var(--line);
    border-radius:12px;padding:11px 13px;font:15px var(--sans);min-height:46px;max-height:160px;outline:none}
  textarea:focus{border-color:var(--amber)}
  button{border:0;border-radius:12px;padding:0 18px;font:700 15px var(--sans);cursor:pointer;
    background:linear-gradient(180deg,var(--amber2),var(--amber));color:#231405}
  button:disabled{opacity:.5;cursor:default}
  /* pairing overlay */
  #pair{position:fixed;inset:0;background:rgba(8,8,12,.92);backdrop-filter:blur(6px);
    display:flex;align-items:center;justify-content:center;padding:24px;z-index:10}
  .card{background:linear-gradient(180deg,var(--panel),var(--panel2));border:1px solid var(--line);
    border-radius:18px;padding:28px 26px;max-width:360px;width:100%;text-align:center}
  .card h1{font-size:20px;margin:0 0 6px}
  .card p{color:var(--dim);font-size:14px;margin:0 0 18px}
  #code{width:100%;text-transform:uppercase;letter-spacing:.32em;text-align:center;font:700 22px var(--mono);
    background:#0c0c13;border:1px solid var(--line);border-radius:12px;padding:14px;color:var(--ink);outline:none}
  #code:focus{border-color:var(--amber)}
  #pair button{width:100%;margin-top:16px;padding:13px}
  #perr{color:#f2777a;font-size:13px;margin-top:12px;min-height:16px}
</style>
</head>
<body>
  <header>
    <svg class="moon" viewBox="0 0 100 100"><path d="M62 8a42 42 0 1 0 26 76A46 46 0 0 1 62 8z" fill="#E0A458"/></svg>
    <b>Nocturne</b> <span style="color:var(--dim);font-size:13px">remote</span>
    <span class="st"><span id="dot" class="dot"></span><span id="ststr">connecting…</span></span>
  </header>
  <div id="log"></div>
  <div class="bar">
    <textarea id="in" placeholder="Message your terminal session…" rows="1" disabled></textarea>
    <button id="send" disabled>Send</button>
  </div>

  <div id="pair">
    <form class="card" id="pairform">
      <h1>◗ Pair this device</h1>
      <p>Enter the 6-character code shown in your terminal.</p>
      <input id="code" maxlength="6" autocomplete="off" autocapitalize="characters" placeholder="······"/>
      <button type="submit">Connect</button>
      <div id="perr"></div>
    </form>
  </div>

<script>
const enc = new TextEncoder(), dec = new TextDecoder();
function b64(b){
  const u = new Uint8Array(b);
  let s = '';
  for(let i=0; i<u.length; i+=0x8000) s += String.fromCharCode(...u.slice(i, i+0x8000));
  return btoa(s);
}
const unb64 = s => Uint8Array.from(atob(s), c => c.charCodeAt(0));
const SID = location.pathname.split('/').filter(Boolean).pop();
let KEY = null, es = null, verified = false, live = null, helloTimer = null;

async function deriveKey(code){
  const base = await crypto.subtle.importKey('raw', enc.encode(code), 'PBKDF2', false, ['deriveKey']);
  return crypto.subtle.deriveKey(
    {name:'PBKDF2', salt: enc.encode('nocturne-remote-v1'), iterations: 150000, hash:'SHA-256'},
    base, {name:'AES-GCM', length:256}, false, ['encrypt','decrypt']);
}
async function encrypt(text){
  const iv = crypto.getRandomValues(new Uint8Array(12));
  const ct = await crypto.subtle.encrypt({name:'AES-GCM', iv}, KEY, enc.encode(text));
  const out = new Uint8Array(12 + ct.byteLength); out.set(iv,0); out.set(new Uint8Array(ct),12);
  return b64(out);
}
async function decrypt(s){
  const raw = unb64(s);
  const pt = await crypto.subtle.decrypt({name:'AES-GCM', iv: raw.slice(0,12)}, KEY, raw.slice(12));
  return dec.decode(pt);
}

const log = document.getElementById('log');
function atBottom(){ return log.scrollHeight - log.scrollTop - log.clientHeight < 60; }
function scroll(){ log.scrollTop = log.scrollHeight; }
function bubble(cls, who, text){
  const wrap = document.createElement('div'); wrap.className = 'msg ' + cls;
  if(who){ const h=document.createElement('div'); h.className='who'; h.textContent=who; wrap.appendChild(h); }
  const body=document.createElement('div'); body.textContent=text; wrap.appendChild(body);
  const stick = atBottom(); log.appendChild(wrap); if(stick) scroll();
  return body;
}
function line(cls, text){ const stick=atBottom(); const d=document.createElement('div'); d.className=cls; d.textContent=text; log.appendChild(d); if(stick) scroll(); }

function handle(ev){
  switch(ev.kind){
    case 'system': break;
    case 'user': live=null; bubble('user','you', ev.text); break;
    case 'stream':
      if(!live) live = bubble('assistant','Nocturne', '');
      live.textContent = ev.text; if(atBottom()) scroll(); break;
    case 'assistant':
      if(!live) live = bubble('assistant','Nocturne','');
      live.textContent = ev.text; live=null; scroll(); break;
    case 'tool': live=null; line('tool', ev.text); break;
    case 'status': default: live=null; line('status', ev.text); break;
  }
}

function setStatus(ok, str){ document.getElementById('dot').className='dot'+(ok?' ok':''); document.getElementById('ststr').textContent=str; }
function perr(t){ document.getElementById('perr').textContent=t; }

async function post(text){
  try { await fetch('/api/remote/'+SID+'/from-browser', {method:'POST', body: await encrypt(text)}); }
  catch(e){}
}

function connect(){
  es = new EventSource('/api/remote/'+SID+'/to-browser');
  es.onopen = () => {                       // greet the terminal so it confirms the code
    post(JSON.stringify({kind:'hello'}));
    clearTimeout(helloTimer);
    helloTimer = setTimeout(() => {
      if(verified) return;
      post(JSON.stringify({kind:'hello'}));  // one retry
      setTimeout(() => { if(!verified){
        perr('No response — wrong code, or the terminal session was closed.');
        document.getElementById('pair').style.display='flex'; if(es) es.close(); KEY=null;
      }}, 3500);
    }, 2500);
  };
  es.onmessage = async (e) => {
    let ev; try { ev = JSON.parse(await decrypt(e.data)); } catch(_) { return; } // not ours / wrong code
    if(!verified){ verified=true; clearTimeout(helloTimer);
      document.getElementById('pair').style.display='none';
      document.getElementById('in').disabled=false; document.getElementById('send').disabled=false;
      setStatus(true,'connected'); document.getElementById('in').focus(); }
    handle(ev);
  };
  es.onerror = () => { if(verified) setStatus(false, 'reconnecting…'); };
}

document.getElementById('pairform').addEventListener('submit', async (e) => {
  e.preventDefault();
  const code = document.getElementById('code').value.trim().toUpperCase();
  if(code.length < 4){ perr('Enter the full code.'); return; }
  perr('connecting…');
  try { KEY = await deriveKey(code); } catch(err){ perr('Crypto unavailable — open this page over https.'); return; }
  verified=false; if(es) es.close(); connect();
});

async function sendMsg(){
  const ta = document.getElementById('in'); const text = ta.value.trim();
  if(!text || !KEY) return;
  ta.value=''; ta.style.height='auto';
  post(JSON.stringify({text}));
}
document.getElementById('send').addEventListener('click', sendMsg);
const inp = document.getElementById('in');
inp.addEventListener('input', () => { inp.style.height='auto'; inp.style.height=Math.min(inp.scrollHeight,160)+'px'; });
inp.addEventListener('keydown', (e) => { if(e.key==='Enter' && !e.shiftKey){ e.preventDefault(); sendMsg(); } });
document.getElementById('code').focus();
</script>
</body>
</html>`
