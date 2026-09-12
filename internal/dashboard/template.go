package dashboard

const loginPage = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Devin API - 管理面板</title>
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{font-family:system-ui,-apple-system,sans-serif;background:#0f1117;color:#e0e0e0;display:flex;justify-content:center;align-items:center;min-height:100vh}
.login-box{background:#1a1d27;border-radius:12px;padding:40px;width:360px;box-shadow:0 4px 24px rgba(0,0,0,.4)}
.login-box h1{font-size:20px;margin-bottom:24px;text-align:center;color:#7c8aff}
.login-box input{width:100%;padding:12px 16px;border:1px solid #333;border-radius:8px;background:#0f1117;color:#e0e0e0;font-size:14px;margin-bottom:16px}
.login-box button{width:100%;padding:12px;border:none;border-radius:8px;background:#7c8aff;color:#fff;font-size:14px;cursor:pointer}
.error{color:#ff6b6b;font-size:13px;text-align:center;margin-top:8px;display:none}
</style>
</head>
<body>
<div class="login-box">
<h1>Devin API 管理面板</h1>
<form id="loginForm">
<input type="password" id="password" placeholder="请输入密码" autofocus>
<button type="submit">登录</button>
</form>
<div class="error" id="err">密码错误</div>
</div>
<script>
document.getElementById('loginForm').addEventListener('submit',async e=>{
e.preventDefault();
const pwd=document.getElementById('password').value;
const res=await fetch('/panel/login',{method:'POST',headers:{'Content-Type':'application/x-www-form-urlencoded'},body:'password='+encodeURIComponent(pwd)});
if(res.ok){location.reload()}else{const el=document.getElementById('err');el.style.display='block';setTimeout(()=>el.style.display='none',2000)}
});
</script>
</body>
</html>`

// dashboardPage is intentionally a single HTML document with client-side filters.
const dashboardPage = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Devin API - 管理面板</title>
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{font-family:system-ui,-apple-system,sans-serif;background:#0f1117;color:#e0e0e0;padding:16px 20px 40px}
.container{max-width:1400px;margin:0 auto}
h1{font-size:22px;margin-bottom:16px;color:#7c8aff}
.section{background:#1a1d27;border-radius:12px;padding:20px;margin-bottom:16px}
.section h2{font-size:15px;margin-bottom:12px;color:#7c8aff}
.grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(160px,1fr));gap:10px}
.card{background:#222632;border-radius:8px;padding:12px}
.card .label{font-size:11px;color:#888;margin-bottom:4px}
.card .value{font-size:16px;font-weight:600;word-break:break-all}
.progress-bar{width:100%;height:8px;background:#333;border-radius:4px;margin-top:8px;overflow:hidden}
.progress-fill{height:100%;border-radius:4px;transition:width .5s}
.filters{display:flex;flex-wrap:wrap;gap:8px;margin-bottom:12px;align-items:center}
.filters select,.filters input[type=search]{padding:8px 12px;border:1px solid #333;border-radius:8px;background:#0f1117;color:#e0e0e0;font-size:13px}
.filters input[type=search]{flex:1;min-width:180px}
.chip-row{display:flex;flex-wrap:wrap;gap:6px;margin-bottom:10px}
.chip{padding:4px 10px;border-radius:999px;border:1px solid #333;background:#222632;color:#bbb;font-size:12px;cursor:pointer;user-select:none}
.chip:hover{border-color:#7c8aff;color:#fff}
.chip.on{background:#7c8aff22;border-color:#7c8aff;color:#aab4ff}
.stats{font-size:12px;color:#888;margin-bottom:8px}
.model-scroll{max-height:70vh;overflow:auto;border:1px solid #222;border-radius:8px}
table{width:100%;border-collapse:collapse;font-size:12px}
th{text-align:left;padding:8px 10px;border-bottom:2px solid #333;color:#888;font-weight:600;position:sticky;top:0;background:#1a1d27;z-index:1;white-space:nowrap}
td{padding:7px 10px;border-bottom:1px solid #222;vertical-align:top}
tr:hover{background:#222632}
.mono{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:11px}
.badge{display:inline-block;padding:1px 7px;border-radius:4px;font-size:10px;font-weight:600;margin:1px 2px 1px 0;white-space:nowrap}
.badge-free{background:#1a4a2e;color:#4ade80}
.badge-low{background:#1a3a4a;color:#60a5fa}
.badge-medium{background:#4a3a1a;color:#fbbf24}
.badge-high{background:#4a1a1a;color:#f87171}
.badge-promo{background:#4a1a4a;color:#c084fc}
.badge-beta{background:#3a3a1a;color:#fde047}
.badge-fast{background:#1a4a4a;color:#22d3ee}
.badge-img{background:#2a2a3a;color:#a78bfa}
.badge-new{background:#1a3a2a;color:#34d399}
.badge-premium{background:#3a2a1a;color:#f59e0b}
.badge-rec{background:#1a2a4a;color:#93c5fd}
.badge-cap{background:#3a1a2a;color:#f472b6}
.badge-off{background:#333;color:#999}
.price{font-variant-numeric:tabular-nums;white-space:nowrap}
.muted{color:#666}
.loading{text-align:center;padding:32px;color:#888}
.note{font-size:12px;color:#888;line-height:1.55;margin-top:10px}
.note code{background:#222;padding:1px 5px;border-radius:3px}
.spark{display:block;width:100%;height:56px;margin-top:8px}
.req-table td{cursor:pointer}
.status-ok{color:#4ade80}.status-err{color:#f87171}.status-warn{color:#fbbf24}
.detail-row td{cursor:default;background:#151823;padding:12px}
.file-list{display:flex;flex-wrap:wrap;gap:6px;margin:8px 0}
.file-link{padding:3px 10px;border:1px solid #333;border-radius:6px;background:#222632;color:#aab4ff;font-size:11px;cursor:pointer;font-family:ui-monospace,Menlo,monospace}
.file-link:hover{border-color:#7c8aff}
.file-view{margin-top:8px;max-height:50vh;overflow:auto;background:#0d0f16;border:1px solid #222;border-radius:8px;padding:10px;font-family:ui-monospace,Menlo,monospace;font-size:11px;white-space:pre-wrap;word-break:break-all}
.meta-grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(200px,1fr));gap:6px 14px;font-size:12px;margin-bottom:8px}
.meta-grid .k{color:#888}.meta-grid .v{color:#e0e0e0;font-family:ui-monospace,Menlo,monospace;word-break:break-all}
</style>
</head>
<body>
<div class="container">
<h1>Devin API 管理面板</h1>

<div class="section" id="statsSection">
<h2>代理运行指标</h2>
<div class="loading">加载中...</div>
</div>

<div class="section" id="statusSection">
<h2>账户 / 容量状态</h2>
<div class="loading">加载中...</div>
</div>

<div class="section" id="activeSection" style="display:none">
<h2>进行中请求</h2>
<div id="activeBody"></div>
</div>

<div class="section">
<h2>最近请求 <span class="muted" style="font-weight:400;font-size:12px">点击行展开调试细节 · 每 10 秒刷新</span></h2>
<div class="filters">
<input type="search" id="reqSearch" placeholder="过滤：模型 / 路径 / 上游ID / IP / key哈希 / 结果..." oninput="loadRequests()">
</div>
<div class="model-scroll" style="max-height:50vh">
<table class="req-table">
<thead><tr>
<th>时间</th><th>API</th><th>状态</th><th>模型</th><th>耗时</th><th>上游TTFB</th><th>Tokens</th><th>客户端</th>
</tr></thead>
<tbody id="reqBody"><tr><td colspan="8" class="loading">加载中...</td></tr></tbody>
</table>
</div>
</div>

<div class="section">
<h2>模型列表 · 价格 · 筛选</h2>
<div class="filters">
<input type="search" id="search" placeholder="搜索 UID / 标签 / 描述 / 系列..." oninput="applyFilters()">
<select id="fProvider" onchange="applyFilters()"><option value="">全部渠道</option></select>
<select id="fApi" onchange="applyFilters()"><option value="">全部 API Provider</option></select>
<select id="fTier" onchange="applyFilters()">
<option value="">全部定价等级</option>
<option value="free">FREE</option>
<option value="low">LOW</option>
<option value="medium">MEDIUM</option>
<option value="high">HIGH</option>
</select>
<select id="fPricing" onchange="applyFilters()"><option value="">全部计费类型</option></select>
<select id="fSort" onchange="applyFilters()">
<option value="default">默认排序</option>
<option value="mult_asc">倍率 升序</option>
<option value="mult_desc">倍率 降序</option>
<option value="in_asc">Input $/1M 升序</option>
<option value="in_desc">Input $/1M 降序</option>
<option value="out_asc">Output $/1M 升序</option>
<option value="out_desc">Output $/1M 降序</option>
<option value="name">名称 A-Z</option>
</select>
</div>
<div class="chip-row" id="chips">
<span class="chip" data-tag="free" onclick="toggleChip(this)">FREE</span>
<span class="chip" data-tag="promo" onclick="toggleChip(this)">PROMO</span>
<span class="chip" data-tag="img" onclick="toggleChip(this)">支持图片</span>
<span class="chip" data-tag="beta" onclick="toggleChip(this)">BETA</span>
<span class="chip" data-tag="new" onclick="toggleChip(this)">NEW</span>
<span class="chip" data-tag="fast" onclick="toggleChip(this)">FAST</span>
<span class="chip" data-tag="premium" onclick="toggleChip(this)">Premium</span>
<span class="chip" data-tag="rec" onclick="toggleChip(this)">推荐</span>
<span class="chip" data-tag="empty_mult" onclick="toggleChip(this)">无倍率(FREE/基准)</span>
<span class="chip" data-tag="disabled" onclick="toggleChip(this)">已禁用</span>
</div>
<div class="stats" id="stats">加载中...</div>
<div class="model-scroll">
<table id="modelTable">
<thead>
<tr>
<th>#</th>
<th>Model</th>
<th>渠道</th>
<th>等级</th>
<th>倍率</th>
<th>Input $/1M</th>
<th>Cached $/1M</th>
<th>Output $/1M</th>
<th>计费</th>
<th>标签</th>
</tr>
</thead>
<tbody><tr><td colspan="10" class="loading">加载中...</td></tr></tbody>
</table>
</div>
<div class="note">
<strong>价格说明：</strong>
<code>credit_multiplier</code> 是 Windsurf credit 消耗倍率。FREE 模型倍率为 0 表示不扣分；其它模型若上游未单独下发倍率，通常按基准 1.0 理解。
<code>Input / Cached / Output</code> 来自 <code>model_dimensions</code>，单位通常是 <strong>$ / 1M tokens</strong>；上游还可能提供 min~max（不同 effort 区间）。
<code>PROMO</code> 表示促销中。计费类型：STATIC_CREDIT=固定 credit、API=按 API、BYOK=自带 Key、ACU_*=ACU 计费。
</div>
</div>
</div>

<script>
let allModels=[];
let activeTags=new Set();

function esc(s){
return String(s==null?'':s)
.replace(/&/g,'&amp;')
.replace(/</g,'&lt;')
.replace(/>/g,'&gt;')
.replace(/"/g,'&quot;');
}
function card(label,value){
return '<div class="card"><div class="label">'+esc(label)+'</div><div class="value">'+esc(String(value))+'</div></div>';
}

function fmtQuota(v){
if(v==null||v===''||v===undefined) return '-';
if(Number(v)===-1) return '不限 / 配额制';
return String(v);
}
function fmtUnix(v){
if(v==null||v===''||Number(v)===0) return '-';
const n=Number(v);
if(!Number.isFinite(n)||n<=0) return String(v);
try{return new Date(n*1000).toLocaleString()}catch(e){return String(v)}
}
function bar(label,percent){
const p=Number(percent);
if(!Number.isFinite(p)) return '';
if(p<=0){
return '<div style="margin-top:12px"><div class="label" style="font-size:12px;color:#888;margin-bottom:4px">'+esc(label)+': 已用尽</div><div class="progress-bar"><div class="progress-fill" style="width:100%;background:#f87171"></div></div></div>';
}
const color=p>50?'#4ade80':p>20?'#fbbf24':'#f87171';
return '<div style="margin-top:12px"><div class="label" style="font-size:12px;color:#888;margin-bottom:4px">'+esc(label)+': '+p+'%</div><div class="progress-bar"><div class="progress-fill" style="width:'+Math.min(100,p)+'%;background:'+color+'"></div></div></div>';
}

async function loadStatus(){
const res=await fetch('/panel/api/status');
const data=await res.json();
const el=document.getElementById('statusSection');
let html='<h2>账户 / 容量状态</h2>';
if(data.user){
const u=data.user;
html+='<div class="grid">';
html+=card('用户名',u.name||'-');
html+=card('邮箱',u.email||'-');
html+=card('Pro',u.pro?'是':'否');
html+=card('Tier',u.teams_tier||'-');
html+=card('User ID',u.user_id||'-');
html+='</div>';
}
if(data.plan_status||data.plan_info){
const ps=data.plan_status||{};
const pi=data.plan_info||{};
html+='<h2 style="margin-top:16px">套餐与用量</h2><div class="grid">';
html+=card('套餐',ps.plan_name||pi.plan_name||'-');
html+=card('计费',ps.billing_strategy||pi.billing_strategy||'-');
html+=card('月 Prompt',fmtQuota(ps.monthly_prompt_credits??pi.monthly_prompt_credits));
html+=card('可用 Prompt',fmtQuota(ps.available_prompt_credits));
html+=card('可用 Flow',fmtQuota(ps.available_flow_credits));
html+=card('可用 Flex',fmtQuota(ps.available_flex_credits));
html+=card('日配额剩余',ps.daily_quota_remaining!=null?(ps.daily_quota_remaining+'%'):'-');
html+=card('周配额剩余',ps.weekly_quota_remaining!=null?(ps.weekly_quota_remaining+'%'):'-');
html+=card('日重置',fmtUnix(ps.daily_quota_reset));
html+=card('周重置',fmtUnix(ps.weekly_quota_reset));
html+=card('周期开始',ps.plan_start||'-');
html+=card('周期结束',ps.plan_end||'-');
html+=card('超额 micros',ps.overage_balance_micros??'-');
html+=card('ACU', (ps.acu_consumed??'-')+' / '+(ps.acu_limit??'-'));
html+='</div>';
if(ps.daily_quota_remaining!=null) html+=bar('每日配额剩余',ps.daily_quota_remaining);
if(ps.weekly_quota_remaining!=null) html+=bar('每周配额剩余',ps.weekly_quota_remaining);
html+='<div class="note" style="margin-top:10px">Pro 多为 <strong>配额制 (QUOTA)</strong>：优先看日/周剩余百分比。月 Prompt 为 -1 表示不按固定 monthly credit 计。</div>';
}
if(data.capacity){
html+='<h2 style="margin-top:16px">容量</h2><div class="grid">';
html+=card('有容量',data.capacity.has_capacity?'是':'否');
html+=card('活跃会话',data.capacity.active_sessions??'-');
html+=card('容量消息',data.capacity.message||'-');
html+='</div>';
}
if(data.ide_status){
html+='<div class="grid" style="margin-top:10px">';
html+=card('IDE 状态',data.ide_status.level||'-');
html+=card('IDE 消息',data.ide_status.message||'-');
html+='</div>';
}
if(data.providers&&data.providers.length){
html+='<h2 style="margin-top:16px">渠道</h2><div class="grid">';
data.providers.forEach(p=>{html+=card(p.display_name||p.provider,p.provider||'-')});
html+='</div>';
}
if(data.model_statuses&&data.model_statuses.length){
html+='<h2 style="margin-top:16px">模型状态告警</h2><div class="grid">';
data.model_statuses.forEach(s=>{
const st=String(s.status||'-');
const bad=/WARN|ERROR|FATAL|DOWN/i.test(st);
html+='<div class="card"><div class="label">'+esc(s.model||'-')+'</div><div class="value" style="color:'+(bad?'#f87171':'#4ade80')+'">'+esc(st)+'</div></div>';
});
html+='</div>';
}
if(!data.user && data.user_status_error){
html+='<div style="color:#ff6b6b;margin-top:12px;font-size:13px">账户用量拉取失败: '+esc(data.user_status_error)+'</div>';
} else if(!data.user){
html+='<div class="note" style="margin-top:12px">暂无账户用量数据。</div>';
}
if(data.capacity_error){html+='<div style="color:#ff6b6b;margin-top:8px;font-size:12px">'+esc(data.capacity_error)+'</div>'}
el.innerHTML=html;
}

function toggleChip(el){
const tag=el.dataset.tag;
if(activeTags.has(tag)){activeTags.delete(tag);el.classList.remove('on')}
else{activeTags.add(tag);el.classList.add('on')}
applyFilters();
}

function fillSelect(id,values){
const el=document.getElementById(id);
const cur=el.value;
const keep=el.options[0].outerHTML;
const opts=[...values].filter(Boolean).sort().map(v=>'<option value="'+esc(v)+'">'+esc(v)+'</option>').join('');
el.innerHTML=keep+opts;
if([...el.options].some(o=>o.value===cur)) el.value=cur;
}

function multDisplay(m){
if(m.cost_tier==='free' && (!m.credit_multiplier || m.credit_multiplier===0)){
return '<span class="badge badge-free">0 (FREE)</span>';
}
if(!m.multiplier_known || m.credit_multiplier===0){
return '<span class="muted" title="上游未单独下发倍率，通常按基准 1.0">— / ≈1.0</span>';
}
const n=Number(m.credit_multiplier);
return 'x'+(n%1?n.toFixed(1):String(n));
}

function money(v){
if(v==null||v===undefined||v==='') return '<span class="muted">—</span>';
const n=Number(v);
if(Number.isNaN(n)) return '<span class="muted">—</span>';
return '<span class="price">$'+n.toFixed(n>=10?1:n>=1?2:3)+'</span>';
}

function badges(m){
let b='';
if(m.cost_tier==='free') b+='<span class="badge badge-free">FREE</span>';
else if(m.cost_tier==='low') b+='<span class="badge badge-low">LOW</span>';
else if(m.cost_tier==='medium') b+='<span class="badge badge-medium">MEDIUM</span>';
else if(m.cost_tier==='high') b+='<span class="badge badge-high">HIGH</span>';
if(m.promo&&m.promo.active) b+='<span class="badge badge-promo">PROMO'+(m.promo.label?(' · '+esc(m.promo.label)):'')+'</span>';
if(m.is_beta) b+='<span class="badge badge-beta">BETA</span>';
if(m.is_new) b+='<span class="badge badge-new">NEW</span>';
if(m.fast&&m.fast.active) b+='<span class="badge badge-fast">FAST</span>';
if(m.supports_images) b+='<span class="badge badge-img">img</span>';
if(m.is_premium) b+='<span class="badge badge-premium">Premium</span>';
if(m.is_recommended) b+='<span class="badge badge-rec">推荐</span>';
if(m.is_capacity_limited) b+='<span class="badge badge-cap">限容</span>';
if(m.disabled) b+='<span class="badge badge-off">禁用</span>';
return b;
}

function matchTags(m){
for(const t of activeTags){
if(t==='free' && m.cost_tier!=='free') return false;
if(t==='promo' && !(m.promo&&m.promo.active)) return false;
if(t==='img' && !m.supports_images) return false;
if(t==='beta' && !m.is_beta) return false;
if(t==='new' && !m.is_new) return false;
if(t==='fast' && !(m.fast&&m.fast.active)) return false;
if(t==='premium' && !m.is_premium) return false;
if(t==='rec' && !m.is_recommended) return false;
if(t==='empty_mult'){
const empty=!m.multiplier_known || m.credit_multiplier===0;
if(!empty) return false;
}
if(t==='disabled' && !m.disabled) return false;
}
return true;
}

function multOf(m){
if(m.cost_tier==='free') return 0;
if(!m.multiplier_known || !m.credit_multiplier) return 1;
return Number(m.credit_multiplier);
}

function applyFilters(){
const q=document.getElementById('search').value.trim().toLowerCase();
const provider=document.getElementById('fProvider').value;
const api=document.getElementById('fApi').value;
const tier=document.getElementById('fTier').value;
const pricing=document.getElementById('fPricing').value;
const sort=document.getElementById('fSort').value;

let list=allModels.filter(m=>{
if(provider && m.provider!==provider) return false;
if(api && m.api_provider!==api) return false;
if(tier && m.cost_tier!==tier) return false;
if(pricing && m.pricing_type!==pricing) return false;
if(!matchTags(m)) return false;
if(q){
const hay=[m.uid,m.label,m.description,m.family,m.provider,m.api_provider].join(' ').toLowerCase();
if(!hay.includes(q)) return false;
}
return true;
});

list=list.slice().sort((a,b)=>{
switch(sort){
case 'mult_asc': return multOf(a)-multOf(b);
case 'mult_desc': return multOf(b)-multOf(a);
case 'in_asc': return (a.price_input??1e9)-(b.price_input??1e9);
case 'in_desc': return (b.price_input??-1)-(a.price_input??-1);
case 'out_asc': return (a.price_output??1e9)-(b.price_output??1e9);
case 'out_desc': return (b.price_output??-1)-(a.price_output??-1);
case 'name': return String(a.label||a.uid).localeCompare(String(b.label||b.uid));
default: return 0;
}
});
renderModels(list);
}

function renderModels(models){
const tbody=document.querySelector('#modelTable tbody');
document.getElementById('stats').textContent='显示 '+models.length+' / 共 '+allModels.length+' 个模型';
if(!models.length){
tbody.innerHTML='<tr><td colspan="10" class="loading">无匹配模型</td></tr>';
return;
}
let html='';
models.forEach((m,i)=>{
const dimTip=(m.dimensions||[]).map(d=>d.label+': '+d.value+(d.min||d.max?(' (min '+d.min+' ~ max '+d.max+')'):'')+' / '+(d.denominator||'')).join(' | ');
const title=[m.description,m.family?('系列: '+m.family):'',dimTip,m.beta_warning||''].filter(Boolean).join(' | ');
html+='<tr title="'+esc(title)+'">';
html+='<td>'+(i+1)+'</td>';
html+='<td><div><strong>'+esc(m.label||'-')+'</strong></div><div class="mono muted">'+esc(m.uid)+'</div></td>';
html+='<td>'+esc(m.provider||'-');
if(m.api_provider && m.api_provider!==m.provider){
html+='<div class="muted mono">'+esc(m.api_provider)+'</div>';
}
html+='</td>';
html+='<td>'+esc(m.cost_tier||'-')+'</td>';
html+='<td>'+multDisplay(m)+'</td>';
html+='<td>'+money(m.price_input)+'</td>';
html+='<td>'+money(m.price_cached)+'</td>';
html+='<td>'+money(m.price_output)+'</td>';
html+='<td class="mono">'+esc(m.pricing_type||'-')+'</td>';
html+='<td>'+badges(m)+'</td>';
html+='</tr>';
});
tbody.innerHTML=html;
}

async function loadModels(){
const res=await fetch('/panel/api/models');
const data=await res.json();
allModels=data.models||[];
const providers=new Set(), apis=new Set(), pricings=new Set();
allModels.forEach(m=>{
if(m.provider) providers.add(m.provider);
if(m.api_provider) apis.add(m.api_provider);
if(m.pricing_type) pricings.add(m.pricing_type);
});
fillSelect('fProvider',providers);
fillSelect('fApi',apis);
fillSelect('fPricing',pricings);
applyFilters();
}

async function loadStats(){
try{
const res=await fetch('/panel/api/stats');
const data=await res.json();
const el=document.getElementById('statsSection');
let html='<h2>代理运行指标</h2><div class="grid">';
const h=data.http||{};
html+=card('活跃请求',h.active_requests??0);
html+=card('完成请求',h.completed_requests??0);
html+=card('被拒请求',h.rejected_requests??0);
html+=card('2xx',h.ok_responses??0);
html+=card('4xx',h.client_error_responses??0);
html+=card('5xx',h.server_error_responses??0);
html+=card('流式/非流式',(h.streaming_requests??0)+' / '+(h.non_streaming_requests??0));
html+=card('运行时长',fmtDuration(h.uptime_seconds));
html+='</div>';
if(data.debuglog){
const d=data.debuglog;
html+='<h2 style="margin-top:16px">日志管道</h2><div class="grid">';
html+=card('活跃日志目录',d.active_request_dirs??0);
html+=card('丢弃日志事件',d.dropped_log_events??0);
html+=card('保留天数',d.retention_days??'-');
html+=card('容量上限',(d.max_total_mb??'-')+' MB');
html+='</div>';
}
if(h.trend_minutes&&h.trend_minutes.length){
html+='<h2 style="margin-top:16px">最近 60 分钟</h2>'+sparkline(h.trend_minutes);
}
html+='<div class="note" style="margin-top:8px">指标为进程内存计数，重启清零；每 10 秒自动刷新。</div>';
el.innerHTML=html;
}catch(e){
document.getElementById('statsSection').innerHTML='<h2>代理运行指标</h2><div class="note">指标拉取失败: '+esc(String(e))+'</div>';
}
}

function sparkline(points){
const w=600,h=56,max=Math.max(1,...points.map(p=>p.requests||0));
const step=w/Math.max(1,points.length-1);
const xy=(p,i)=>((i*step).toFixed(1))+','+(h-4-((p.requests||0)/max)*(h-10)).toFixed(1);
const errxy=(p,i)=>((i*step).toFixed(1))+','+(h-4-((p.errors||0)/max)*(h-10)).toFixed(1);
const line=points.map(xy).join(' ');
const errs=points.map(errxy).join(' ');
const total=points.reduce((s,p)=>s+(p.requests||0),0);
const errTotal=points.reduce((s,p)=>s+(p.errors||0),0);
return '<svg class="spark" viewBox="0 0 '+w+' '+h+'" preserveAspectRatio="none">'
+'<polyline points="'+line+'" fill="none" stroke="#7c8aff" stroke-width="1.5"/>'
+(errTotal?'<polyline points="'+errs+'" fill="none" stroke="#f87171" stroke-width="1.5"/>':'')
+'</svg><div class="note" style="margin-top:4px">蓝=请求/分钟 · 红=错误/分钟 · 本小时共 '+total+' 请求 / '+errTotal+' 错误</div>';
}

function fmtTime(iso){
try{const d=new Date(iso);return d.toLocaleTimeString('zh-CN',{hour12:false})}catch(e){return iso||'-'}
}
function fmtMs(v){return v==null?'-':(v>=1000?(v/1000).toFixed(1)+'s':v+'ms')}
function statusClass(code){
if(code>=500)return 'status-err';
if(code>=400)return 'status-warn';
return 'status-ok';
}

let expandedDir=null;
let openFile=null;
function tickRequests(){if(openFile)return;loadRequests()}
async function loadRequests(){
try{
const q=encodeURIComponent(document.getElementById('reqSearch').value.trim());
const res=await fetch('/panel/api/requests?limit=100'+(q?'&q='+q:''));
const data=await res.json();
const tbody=document.getElementById('reqBody');
const list=data.requests||[];
if(data.disabled){tbody.innerHTML='<tr><td colspan="8" class="loading">调试日志未启用（config: debug.enabled）</td></tr>';return}
if(!list.length){tbody.innerHTML='<tr><td colspan="8" class="loading">暂无请求记录</td></tr>';return}
let html='';
list.forEach(e=>{
const models=esc(e.requested_model||'-');
const resolved=(e.model&&e.model!==e.requested_model)?' → '+esc(e.model):'';
const mismatch=e.model_mismatch?' <span class="badge badge-high">错配</span>':'';
html+='<tr onclick="toggleDetail(\''+esc(e.dir)+'\',this)">'
+'<td class="mono">'+fmtTime(e.started_at)+'</td>'
+'<td>'+esc(e.api||'-')+'</td>'
+'<td class="'+statusClass(e.status_code)+'">'+e.status_code+' '+esc(e.result||'')+'</td>'
+'<td>'+models+resolved+mismatch+'</td>'
+'<td class="mono">'+fmtMs(e.duration_ms)+'</td>'
+'<td class="mono">'+fmtMs(e.first_upstream_ms)+'</td>'
+'<td class="mono">'+(e.input_tokens??0)+'/'+(e.output_tokens??0)+'</td>'
+'<td class="mono muted">'+esc(e.client_ip||'')+'</td></tr>';
if(expandedDir===e.dir){
html+='<tr class="detail-row"><td colspan="8"><div class="loading" style="padding:8px">加载中...</div></td></tr>';
}
});
tbody.innerHTML=html;
if(expandedDir){fillDetail(expandedDir)}
}catch(e){/* 刷新失败保留下次重试 */}
}

async function toggleDetail(dir,row){
if(expandedDir===dir){expandedDir=null;openFile=null;loadRequests();return}
expandedDir=dir;
loadRequests();
}

async function fillDetail(dir){
const row=document.querySelector('.detail-row td');
if(!row)return;
try{
const res=await fetch('/panel/api/requests/'+encodeURIComponent(dir));
const d=await res.json();
const m=d.meta||{};
let html='<div class="meta-grid">';
[['目录',d.dir],['API',m.api],['路径',(m.method||'')+' '+(m.path||'')],['状态',(m.status_code||'-')+' '+(m.result||'')],
['请求模型',m.requested_model],['实际模型',m.model],['响应模型',m.response_model],
['开始',m.started_at],['耗时',fmtMs(m.duration_ms)],['上游TTFB',fmtMs(m.first_upstream_ms)],['客户端TTFB',fmtMs(m.first_client_ms)],
['上游请求ID',m.upstream_request_id],['客户端IP',m.client&&m.client.ip],['UA',m.client&&m.client.user_agent],['Key哈希',m.client&&m.client.key_hash],
['Tokens',m.usage?(m.usage.input+' in / '+m.usage.output+' out / '+m.usage.cache_read+' cached'):null],
['丢弃事件',m.dropped_events]].forEach(kv=>{
if(kv[1]==null||kv[1]==='')return;
html+='<div><span class="k">'+esc(kv[0])+'</span> <span class="v">'+esc(String(kv[1]))+'</span></div>';
});
html+='</div><div class="file-list">';
(d.files||[]).forEach(f=>{
html+='<span class="file-link" onclick="loadFile(\''+esc(d.dir)+'\',\''+esc(f.name)+'\')">'+esc(f.name)+' <span class="muted">'+f.size+'B</span></span>';
});
html+='</div><div class="file-view" id="fileView" style="display:none"></div>';
if(!d.meta){html='<div class="note">meta.json 缺失或已损坏</div>'+html}
row.innerHTML=html;
if(openFile&&openFile.dir===d.dir){loadFile(d.dir,openFile.name)}
}catch(e){row.innerHTML='<div class="note">详情拉取失败: '+esc(String(e))+'</div>'}
}

async function loadFile(dir,name){
const view=document.getElementById('fileView');
if(!view)return;
openFile={dir:dir,name:name};
view.style.display='block';
view.textContent='加载 '+name+' ...';
try{
const res=await fetch('/panel/api/requests/'+encodeURIComponent(dir)+'/file/'+name.split('/').map(encodeURIComponent).join('/'));
const d=await res.json();
let text=d.text||'';
if(name.endsWith('.json')){
try{text=JSON.stringify(JSON.parse(text),null,2)}catch(e){}
}else if(name.endsWith('.jsonl')){
text=text.split('\n').filter(Boolean).map(line=>{
try{const o=JSON.parse(line);
const head=(o.seq?'#'+o.seq+' ':'')+(o.elapsed_ms!=null?'+'+o.elapsed_ms+'ms ':'')+(o.event||'');
return head+'  '+JSON.stringify(o.data!==undefined?o.data:o,null,0).slice(0,2000);
}catch(e){return line}
}).join('\n\n');
}
view.textContent=text+(d.truncated?'\n\n... 已截断（原始 '+d.size+' 字节）':'');
}catch(e){view.textContent='读取失败: '+String(e)}
}

async function loadActive(){
try{
const res=await fetch('/panel/api/requests/active');
const data=await res.json();
const list=data.active||[];
const section=document.getElementById('activeSection');
if(!list.length){section.style.display='none';return}
section.style.display='';
let html='<table><thead><tr><th>目录</th><th>API</th><th>路径</th><th>已耗时</th><th>已写文件</th><th>丢弃事件</th></tr></thead><tbody>';
list.forEach(a=>{
html+='<tr><td class="mono">'+esc(a.dir)+'</td><td>'+esc(a.meta&&a.meta.api||'-')+'</td>'
+'<td class="mono">'+esc((a.meta&&a.meta.method||'')+' '+(a.meta&&a.meta.path||''))+'</td>'
+'<td class="mono">'+fmtMs(a.elapsed_ms)+'</td>'
+'<td class="mono">'+(a.files||[]).map(f=>esc(f.name)).join(', ')+'</td>'
+'<td>'+(a.dropped_events||0)+'</td></tr>';
});
html+='</tbody></table>';
document.getElementById('activeBody').innerHTML=html;
}catch(e){/* 静默 */}
}
function fmtDuration(sec){
sec=Number(sec)||0;
if(sec<60) return sec+'s';
if(sec<3600) return Math.floor(sec/60)+'m '+sec%60+'s';
return Math.floor(sec/3600)+'h '+Math.floor(sec%3600/60)+'m';
}

loadStatus();
loadModels();
loadStats();
loadRequests();
loadActive();
setInterval(loadStats,10000);
setInterval(loadActive,10000);
setInterval(tickRequests,10000);
</script>
</body>
</html>`
