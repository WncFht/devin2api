// 模型页：上游模型目录 + 价格/倍率/能力筛选。目录数据 5 分钟缓存（后端），
// 页面只在首次打开时拉取一次。

const Models = (() => {
  let allModels = [];
  const activeTags = new Set();

  function multDisplay(m) {
    if (m.cost_tier === 'free' && (!m.credit_multiplier || m.credit_multiplier === 0)) {
      return '<span class="badge badge-free">0 (FREE)</span>';
    }
    if (!m.multiplier_known || m.credit_multiplier === 0) {
      return '<span class="muted" title="上游未单独下发倍率，通常按基准 1.0">— / ≈1.0</span>';
    }
    const n = Number(m.credit_multiplier);
    return 'x' + (n % 1 ? n.toFixed(1) : String(n));
  }
  function multOf(m) {
    if (m.cost_tier === 'free') return 0;
    if (!m.multiplier_known || !m.credit_multiplier) return 1;
    return Number(m.credit_multiplier);
  }
  function badges(m) {
    let b = '';
    if (m.cost_tier === 'free') b += '<span class="badge badge-free">FREE</span>';
    else if (m.cost_tier === 'low') b += '<span class="badge badge-low">LOW</span>';
    else if (m.cost_tier === 'medium') b += '<span class="badge badge-medium">MEDIUM</span>';
    else if (m.cost_tier === 'high') b += '<span class="badge badge-high">HIGH</span>';
    if (m.promo && m.promo.active) b += '<span class="badge badge-promo">PROMO' + (m.promo.label ? (' · ' + esc(m.promo.label)) : '') + '</span>';
    if (m.is_beta) b += '<span class="badge badge-beta">BETA</span>';
    if (m.is_new) b += '<span class="badge badge-new">NEW</span>';
    if (m.fast && m.fast.active) b += '<span class="badge badge-fast">FAST</span>';
    if (m.supports_images) b += '<span class="badge badge-img">img</span>';
    if (m.is_premium) b += '<span class="badge badge-premium">Premium</span>';
    if (m.is_recommended) b += '<span class="badge badge-rec">推荐</span>';
    if (m.is_capacity_limited) b += '<span class="badge badge-cap">限容</span>';
    if (m.beta_warning) b += '<span class="badge badge-beta" title="' + esc(m.beta_warning) + '">警告</span>';
    if (m.disabled) b += '<span class="badge badge-off">禁用</span>';
    return b;
  }
  function matchTags(m) {
    for (const t of activeTags) {
      if (t === 'free' && m.cost_tier !== 'free') return false;
      if (t === 'promo' && !(m.promo && m.promo.active)) return false;
      if (t === 'img' && !m.supports_images) return false;
      if (t === 'beta' && !m.is_beta) return false;
      if (t === 'new' && !m.is_new) return false;
      if (t === 'fast' && !(m.fast && m.fast.active)) return false;
      if (t === 'premium' && !m.is_premium) return false;
      if (t === 'rec' && !m.is_recommended) return false;
      if (t === 'empty_mult') {
        const empty = !m.multiplier_known || m.credit_multiplier === 0;
        if (!empty) return false;
      }
      if (t === 'disabled' && !m.disabled) return false;
    }
    return true;
  }

  function apply() {
    const q = $('search').value.trim().toLowerCase();
    const provider = $('fProvider').value, api_ = $('fApi').value, tier = $('fTier').value;
    const pricing = $('fPricing').value, sort = $('fSort').value;
    let list = allModels.filter(m => {
      if (provider && m.provider !== provider) return false;
      if (api_ && m.api_provider !== api_) return false;
      if (tier && m.cost_tier !== tier) return false;
      if (pricing && m.pricing_type !== pricing) return false;
      if (!matchTags(m)) return false;
      if (q) {
        const hay = [m.uid, m.label, m.description, m.family, m.provider, m.api_provider].join(' ').toLowerCase();
        if (!hay.includes(q)) return false;
      }
      return true;
    });
    list = list.slice().sort((a, b) => {
      switch (sort) {
        case 'mult_asc': return multOf(a) - multOf(b);
        case 'mult_desc': return multOf(b) - multOf(a);
        case 'in_asc': return (a.price_input ?? 1e9) - (b.price_input ?? 1e9);
        case 'in_desc': return (b.price_input ?? -1) - (a.price_input ?? -1);
        case 'out_asc': return (a.price_output ?? 1e9) - (b.price_output ?? 1e9);
        case 'out_desc': return (b.price_output ?? -1) - (a.price_output ?? -1);
        case 'name': return String(a.label || a.uid).localeCompare(String(b.label || b.uid));
        default: return 0;
      }
    });
    render(list);
  }

  function render(models) {
    const tbody = document.querySelector('#modelTable tbody');
    $('modelCount').textContent = '显示 ' + models.length + ' / 共 ' + allModels.length + ' 个';
    if (!models.length) {
      tbody.innerHTML = '<tr><td colspan="10" class="loading">无匹配模型</td></tr>';
      return;
    }
    let html = '';
    models.forEach((m, i) => {
      const dimTip = (m.dimensions || []).map(d => d.label + ': ' + d.value + (d.min || d.max ? (' (min ' + d.min + ' ~ max ' + d.max + ')') : '') + ' / ' + (d.denominator || '')).join(' | ');
      const title = [m.description, m.family ? ('系列: ' + m.family) : '', dimTip, m.beta_warning || ''].filter(Boolean).join(' | ');
      html += '<tr title="' + esc(title) + '">' +
        '<td>' + (i + 1) + '</td>' +
        '<td><div><strong>' + esc(m.label || '-') + '</strong></div><div class="mono muted">' + esc(m.uid) + '</div></td>' +
        '<td>' + esc(m.provider || '-') + (m.api_provider && m.api_provider !== m.provider && m.api_provider !== 'UNSPECIFIED' ? '<div class="muted mono">' + esc(m.api_provider) + '</div>' : '') + '</td>' +
        '<td>' + esc(m.cost_tier || '-') + '</td>' +
        '<td>' + multDisplay(m) + '</td>' +
        '<td>' + money(m.price_input) + '</td>' +
        '<td>' + money(m.price_cached) + '</td>' +
        '<td>' + money(m.price_output) + '</td>' +
        '<td class="mono">' + esc(m.pricing_type || '-') + '</td>' +
        '<td>' + badges(m) + '</td></tr>';
    });
    tbody.innerHTML = html;
  }

  async function load() {
    if (allModels.length) return;
    try {
      const d = await api('/models');
      allModels = d.models || [];
      const providers = new Set(), apis = new Set(), pricings = new Set();
      allModels.forEach(m => {
        if (m.provider) providers.add(m.provider);
        if (m.api_provider) apis.add(m.api_provider);
        if (m.pricing_type) pricings.add(m.pricing_type);
      });
      fillSelect('fProvider', providers);
      fillSelect('fApi', apis);
      fillSelect('fPricing', pricings);
      apply();
    } catch (e) {
      document.querySelector('#modelTable tbody').innerHTML = '<tr><td colspan="10" class="loading">模型目录拉取失败: ' + esc(String(e)) + '</td></tr>';
    }
  }

  $('search').addEventListener('input', debounce(apply, 200));
  ['fProvider', 'fApi', 'fTier', 'fPricing', 'fSort'].forEach(id => $(id).addEventListener('change', apply));
  $('chips').addEventListener('click', e => {
    const c = e.target.closest('.chip');
    if (!c) return;
    const t = c.dataset.tag;
    if (activeTags.has(t)) { activeTags.delete(t); c.classList.remove('on'); }
    else { activeTags.add(t); c.classList.add('on'); }
    apply();
  });

  Tabs.register('models', load);
  return {};
})();
