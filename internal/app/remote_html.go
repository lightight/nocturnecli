package app

// remoteHTML is the single-page browser client served by the remote bridge. It
// derives the AES-GCM key from the pairing code with Web Crypto (PBKDF2), so
// nothing readable crosses the wire without the code. Themed after the main
// site (docs/index.html): night surfaces, clay accent, serif display type.
const remoteHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8"/>
<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover"/>
<meta name="theme-color" content="#0A0A0C"/>
<title>Nocturne — remote</title>
<link rel="icon" href="data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 100 100'%3E%3Cpath d='M64 12a40 40 0 1 0 24 72A44 44 0 0 1 64 12z' fill='%23D97757'/%3E%3C/svg%3E"/>
<link rel="preconnect" href="https://fonts.googleapis.com"/>
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin/>
<link href="https://fonts.googleapis.com/css2?family=JetBrains+Mono:wght@400;500;700&display=swap" rel="stylesheet"/>
<style>
  :root{
    --bg:#0A0A0C; --bg-2:#131316; --panel:#141417;
    --line:rgba(255,255,255,.10); --line-2:rgba(255,255,255,.19);
    --ink:#EDEBE7; --muted:#A8A59E; --faint:#8B8880;
    --clay:#E08A68; --clay-bright:#D97757; --clay-deep:#A14A2F; --clay-tint:rgba(217,119,87,.14);
    --night:#101014; --night-2:#1A1A1F;
    --serif:"Tiempos Headline","Iowan Old Style","Palatino Linotype",Palatino,Georgia,"Times New Roman",serif;
    --sans:"Styrene A","Styrene B",-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,Helvetica,Arial,sans-serif;
    --mono:"GT America Mono","JetBrains Mono",ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;
    --ease:cubic-bezier(.22,1,.36,1);
  }
  *{box-sizing:border-box}
  html,body{height:100%;margin:0}
  body{background:var(--bg);color:var(--ink);font-family:var(--sans);
    display:flex;flex-direction:column;font-size:16px;line-height:1.55;-webkit-font-smoothing:antialiased;
    background:radial-gradient(720px 400px at 82% -12%,rgba(217,119,87,.13),transparent 60%),
               radial-gradient(620px 340px at 4% -4%,rgba(224,138,104,.08),transparent 55%),var(--bg);}
  :focus-visible{outline:2px solid var(--clay-bright);outline-offset:2px;border-radius:6px}

  header{display:flex;align-items:center;gap:10px;padding:14px 18px;border-bottom:1px solid var(--line);
    background:rgba(10,10,12,.72);backdrop-filter:blur(12px)}
  .moon{width:20px;height:20px;animation:brandGlow 7.5s ease-in-out infinite}
  @keyframes brandGlow{
    0%,100%{filter:drop-shadow(0 0 3px rgba(217,119,87,.30))}
    50%    {filter:drop-shadow(0 0 8px rgba(217,119,87,.58))}
  }
  header b{font-family:var(--serif);font-weight:600;font-size:19px;letter-spacing:-.01em}
  header .tag{font-family:var(--serif);font-style:italic;font-size:16px;color:var(--clay);
    animation:litGlow 7.5s ease-in-out infinite}
  @keyframes litGlow{               /* breathes on the same clock as the moon */
    0%,100%{text-shadow:0 0 0 rgba(217,119,87,0)}
    50%    {text-shadow:0 0 18px rgba(217,119,87,.32)}
  }
  header .st{margin-left:auto;font:500 12.5px var(--sans);color:var(--muted);display:flex;align-items:center;gap:7px}
  .dot{width:8px;height:8px;border-radius:50%;background:#f2777a;box-shadow:0 0 8px #f2777a}
  .dot.ok{background:#7bd88f;box-shadow:0 0 8px #7bd88f}

  #log{flex:1;overflow-y:auto;padding:20px 18px 10px;display:flex;flex-direction:column;gap:14px;
    scrollbar-width:thin;scrollbar-color:rgba(255,255,255,.18) transparent}
  #log::-webkit-scrollbar{width:10px}
  #log::-webkit-scrollbar-track{background:transparent}
  #log::-webkit-scrollbar-thumb{background:rgba(255,255,255,.14);border-radius:8px;border:2px solid var(--bg)}
  #log::-webkit-scrollbar-thumb:hover{background:rgba(217,119,87,.45)}
  .msg{max-width:88%;white-space:pre-wrap;overflow-wrap:anywhere;line-height:1.55;font-size:15.5px}
  .msg.user{align-self:flex-end;background:var(--clay-tint);
    border:1px solid rgba(217,119,87,.30);border-radius:14px 14px 4px 14px;padding:10px 13px}
  .msg.user .who{color:var(--clay)}
  .msg.assistant{align-self:flex-start;background:linear-gradient(180deg,var(--night-2),var(--night));
    border:1px solid var(--line);border-radius:14px 14px 14px 4px;padding:10px 13px}
  .who{font:600 12px var(--sans);letter-spacing:.02em;margin-bottom:3px;opacity:.9}
  .msg.assistant .who{color:var(--clay)}
  .tool{align-self:flex-start;font:13px var(--mono);color:var(--muted)}
  .status{align-self:center;font:12.5px var(--sans);color:var(--faint);white-space:pre-wrap;text-align:center}

  /* input bar + slash menu */
  #dock{position:relative;border-top:1px solid var(--line);background:rgba(10,10,12,.72);backdrop-filter:blur(12px)}
  .bar{display:flex;gap:9px;padding:12px 14px}
  textarea{flex:1;resize:none;background:var(--panel);color:var(--ink);border:1px solid var(--line);
    border-radius:12px;padding:11px 13px;font:15.5px var(--sans);min-height:46px;max-height:160px;outline:none;
    transition:border-color .15s}
  textarea::placeholder{color:var(--faint)}
  textarea:focus{border-color:var(--clay-bright)}
  button{border:0;border-radius:9px;padding:0 20px;font:500 15px var(--sans);cursor:pointer;
    background:var(--ink);color:var(--bg);transition:background .15s,transform .15s var(--ease)}
  button:hover:not(:disabled){background:#fff;transform:translateY(-1px)}
  button.stop{background:var(--clay-bright);color:#1d0f08}
  button.stop:hover{background:var(--clay)}
  button:disabled{opacity:.45;cursor:default}
  #menu{position:absolute;left:14px;right:14px;bottom:calc(100% + 6px);z-index:5;
    background:var(--bg-2);border:1px solid var(--line-2);border-radius:12px;overflow:hidden;
    box-shadow:0 -8px 40px rgba(0,0,0,.5);display:none;max-height:280px;overflow-y:auto}
  #menu .row{display:flex;gap:12px;align-items:baseline;padding:9px 13px;cursor:pointer}
  #menu .row .cmd{font:600 13.5px var(--mono);color:var(--clay);flex-shrink:0}
  #menu .row .dsc{font:13px var(--sans);color:var(--muted);overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
  #menu .row.sel{background:var(--clay-tint)}
  #menu .hint{padding:7px 13px;font:11.5px var(--sans);color:var(--faint);border-top:1px solid var(--line)}

  /* pairing overlay */
  #pair{position:fixed;inset:0;background:rgba(10,10,12,.94);backdrop-filter:blur(8px);
    display:flex;align-items:center;justify-content:center;padding:24px;z-index:10}
  .card{background:linear-gradient(180deg,var(--panel),var(--bg-2));border:1px solid var(--line);
    border-radius:18px;padding:30px 26px;max-width:370px;width:100%;text-align:center;
    box-shadow:0 24px 80px rgba(0,0,0,.6),0 0 60px -20px rgba(217,119,87,.28)}
  .card .moon{width:34px;height:34px;margin-bottom:10px}
  .card h1{font-family:var(--serif);font-weight:600;font-size:22px;letter-spacing:-.01em;margin:0 0 6px}
  .card p{color:var(--muted);font-size:14px;margin:0 0 18px}
  #code{width:100%;text-transform:uppercase;letter-spacing:.32em;text-align:center;font:700 22px var(--mono);
    background:var(--bg);border:1px solid var(--line-2);border-radius:12px;padding:14px;color:var(--ink);outline:none}
  #code:focus{border-color:var(--clay-bright)}
  #pair button{width:100%;margin-top:16px;padding:13px}
  #perr{color:#f2777a;font-size:13px;margin-top:12px;min-height:16px}
</style>
</head>
<body>
  <header>
    <svg class="moon" viewBox="0 0 100 100"><path d="M64 12a40 40 0 1 0 24 72A44 44 0 0 1 64 12z" fill="#D97757"/></svg>
    <b>Nocturne</b> <span class="tag">remote</span>
    <span class="st"><span id="dot" class="dot"></span><span id="ststr">connecting…</span></span>
  </header>
  <div id="log"></div>
  <div id="dock">
    <div id="menu"></div>
    <div class="bar">
      <textarea id="in" placeholder="Message your terminal session…  (/ for commands)" rows="1" disabled></textarea>
      <button id="send" disabled>Send</button>
    </div>
  </div>

  <div id="pair">
    <form class="card" id="pairform">
      <svg class="moon" viewBox="0 0 100 100"><path d="M64 12a40 40 0 1 0 24 72A44 44 0 0 1 64 12z" fill="#D97757"/></svg>
      <h1>Pair this device</h1>
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
const inp = document.getElementById('in');
const sendBtn = document.getElementById('send');
const menu = document.getElementById('menu');
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

let busy = false;
	let msgQueue = [];
	function setBusy(b){
	  busy = b;
	  sendBtn.textContent = b ? 'Stop' : 'Send';
	  sendBtn.classList.toggle('stop', b);
	  if(b){
	    inp.disabled = true;
	    inp.placeholder = 'Nocturne is thinking…  (your messages will be queued)';
	  } else {
	    inp.disabled = false;
	    inp.placeholder = 'Message your terminal session…  (/ for commands)';
	    // flush any queued messages now that we're idle
	    if(msgQueue.length){
	      const pending = msgQueue.slice();
	      msgQueue = [];
	      pending.forEach(t => post(JSON.stringify({text: t})));
	    }
	  }
	}

function handleOne(ev){
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
    case 'busy': setBusy(ev.text === '1'); break;
    case 'input':
      lastDraft = ev.text;
      // Don't clobber while the user is actively typing here.
      if(document.activeElement !== inp && inp.value !== ev.text){
        inp.value = ev.text; autoresize(); refreshMenu();
      }
      break;
    case 'status': default: live=null; line('status', ev.text); break;
  }
}
function handle(ev){ Array.isArray(ev) ? ev.forEach(handleOne) : handleOne(ev); }

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
      inp.disabled=false; sendBtn.disabled=false;
      setStatus(true,'connected'); inp.focus(); }
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

// --- slash command menu (mirrors the terminal's command list) --------------
const COMMANDS = [
	  {name:'/help',       desc:'show commands'},
	  {name:'/model',      desc:'pick a model from a list (or /model <id>)'},
	  {name:'/level',      desc:'thinking: off · normal · extended'},
	  {name:'/permissions',desc:'how tool actions are approved (ask · auto · bypass)'},
	  {name:'/btw',        desc:'by the way — add a note without breaking flow'},
	  {name:'/key',        desc:'save your API key (remembered everywhere)'},
	  {name:'/image',      desc:'attach an image file — /image <path>'},
	  {name:'/mouse',      desc:'capture the mouse for wheel scrolling (off = native text selection)'},
	  {name:'/cd',         desc:'change the working directory'},
	  {name:'/usage',      desc:'usage & quota — sent as a text summary'},
	  {name:'/cowork',     desc:'computer use — see & control this computer'},
	  {name:'/plan',       desc:'plan mode — read-only exploration, approve to execute'},
	  {name:'/compact',    desc:'summarize the conversation to free up context'},
	  {name:'/resume',     desc:'list saved chats — /resume <n> to open one'},
	  {name:'/new',        desc:'start a new chat'},
	  {name:'/remote',     desc:'show the pairing link & code (/remote off to stop)'},
	  {name:'/clear',      desc:'clear the conversation'},
	  {name:'/init',       desc:'generate a NOCTURNE.md for the project'},
	  {name:'/update',     desc:'update Nocturne to the latest release'},
	  {name:'/exit',       desc:'quit the terminal session'},
	];
let matches = [], msel = 0;
function hideMenu(){ menu.style.display='none'; matches=[]; }
function refreshMenu(){
  const v = inp.value;
  if(v.startsWith('/') && !/[\s\n]/.test(v)){
    matches = COMMANDS.filter(c => c.name.startsWith(v.toLowerCase()));
    if(matches.length){
      if(msel >= matches.length) msel = 0;
      menu.innerHTML = matches.map((c,i) =>
        '<div class="row'+(i===msel?' sel':'')+'" data-i="'+i+'"><span class="cmd">'+c.name+'</span><span class="dsc">'+c.desc+'</span></div>'
      ).join('') + '<div class="hint">↑↓ choose · tab complete · enter run</div>';
      menu.style.display='block';
      menu.querySelectorAll('.row').forEach(r => r.addEventListener('mousedown', (e) => {
        e.preventDefault(); completeCmd(matches[+r.dataset.i]);
      }));
      return;
    }
  }
  hideMenu();
}
function completeCmd(c){
  inp.value = c.name + ' ';
  hideMenu(); inp.focus(); autoresize(); syncDraftNow();
}

// --- shared draft: keep this box and the terminal's input in sync ----------
let lastDraft = null, draftTimer = null;
function syncDraftNow(){
  if(!verified || inp.value === lastDraft) return;
  lastDraft = inp.value;
  post(JSON.stringify({kind:'input', text: inp.value}));
}
function autoresize(){ inp.style.height='auto'; inp.style.height=Math.min(inp.scrollHeight,160)+'px'; }

async function sendMsg(){
  const text = inp.value.trim();
  if(!text || !KEY) return;
  inp.value=''; autoresize(); hideMenu(); syncDraftNow();
  post(JSON.stringify({text}));
}
sendBtn.addEventListener('click', () => {
  if(busy){ post(JSON.stringify({kind:'interrupt'})); return; }
  sendMsg();
});
inp.addEventListener('input', () => {
  autoresize(); refreshMenu();
  clearTimeout(draftTimer);
  draftTimer = setTimeout(syncDraftNow, 150);
});
inp.addEventListener('keydown', (e) => {
  if(menu.style.display === 'block' && matches.length){
    if(e.key === 'ArrowDown'){ e.preventDefault(); msel=(msel+1)%matches.length; refreshMenu(); return; }
    if(e.key === 'ArrowUp'){ e.preventDefault(); msel=(msel-1+matches.length)%matches.length; refreshMenu(); return; }
    if(e.key === 'Tab'){ e.preventDefault(); completeCmd(matches[msel]); return; }
    if(e.key === 'Escape'){ e.preventDefault(); hideMenu(); return; }
    if(e.key === 'Enter' && !e.shiftKey){
      e.preventDefault();
      if(matches.length === 1 && inp.value === matches[0].name) sendMsg();
      else completeCmd(matches[msel]);
      return;
    }
  }
  if(e.key==='Enter' && !e.shiftKey){ e.preventDefault(); sendMsg(); }
});
document.getElementById('code').focus();
</script>
</body>
</html>`
