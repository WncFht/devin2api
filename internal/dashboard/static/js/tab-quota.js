// 配额页：日/周配额曲线与燃烧速率预测 + 账户/套餐/容量/渠道/模型状态。
// status 端点聚合多个上游调用（最长 610s 超时），失败字段以 *_error 透出。

const Quota = (() => {
  async function load() {
    try {
      const d = await api('/quota');
      renderQuota(d);
    } catch (e) {
      $('quotaKpis').innerHTML = '<div class="note">配额数据拉取失败: ' + esc(String(e)) + '</div>';
    }
    try {
      const d = await api('/status');
      renderStatus(d);
    } catch (e) {
      $('accountPanel').innerHTML = '<h3>账户与套餐</h3><div class="note">状态拉取失败: ' + esc(String(e)) + '</div>';
    }
  }

  function renderQuota(d) {
    const pts = d.points || [];
    const kEl = $('quotaKpis');
    if (!pts.length) {
      kEl.innerHTML = '<div class="note">暂无配额快照——采样器每 debug.quota_interval_minutes 分钟写一条，重启后开始积累。</div>';
      $('quotaCurve').innerHTML = '';
      Charts.empty($('quotaCurve'), '暂无配额快照');
      return;
    }
    const last = pts[pts.length - 1];
    let html = '';
    if (last.daily_remaining != null) {
      const dd = d.daily || {};
      html += kpi('日配额剩余', Number(last.daily_remaining).toFixed(1) + '%',
        (dd.exhausted_at ? '约 ' + Number(dd.hours_left || 0).toFixed(1) + 'h 后耗尽 · ' : '') + '燃烧 ' + Number(dd.burn_per_hour || 0).toFixed(2) + '%/h',
        last.daily_remaining > 50 ? 'ok' : last.daily_remaining > 20 ? 'warn' : 'err');
    }
    if (last.weekly_remaining != null) {
      const wk = d.weekly || {};
      html += kpi('周配额剩余', Number(last.weekly_remaining).toFixed(1) + '%',
        (wk.exhausted_at ? fmtUnix(wk.exhausted_at) + ' 耗尽 · ' : '') + '燃烧 ' + Number(wk.burn_per_hour || 0).toFixed(3) + '%/h',
        last.weekly_remaining > 50 ? 'ok' : last.weekly_remaining > 20 ? 'warn' : 'err');
    }
    html += kpi('日重置', fmtIn(last.daily_reset_at), fmtUnixShort(last.daily_reset_at)) +
      kpi('周重置', fmtIn(last.weekly_reset_at), fmtUnixShort(last.weekly_reset_at));
    kEl.innerHTML = html;

    Charts.render($('quotaCurve'), {
      dataZoom: Charts.zoom(pts),
      yAxis: { min: 0, max: 100, axisLabel: { formatter: '{value}%', color: '#8b93a7', fontSize: 10.5 }, splitLine: { lineStyle: { color: 'rgba(148,163,184,0.08)' } } },
      tooltip: { trigger: 'axis', valueFormatter: v => v == null ? '-' : Number(v).toFixed(1) + '%', backgroundColor: 'rgba(18,21,31,.96)', borderColor: 'rgba(148,163,184,.25)', textStyle: { color: '#e5e9f2', fontSize: 12 } },
      series: [
        Charts.line('日剩余', '#818cf8', Charts.tsList(pts, 'at', 'daily_remaining')),
        Charts.line('周剩余', '#f472b6', Charts.tsList(pts, 'at', 'weekly_remaining')),
      ],
    });
  }

  function renderStatus(d) {
    let html = '<h3>账户与套餐</h3><div class="grid">';
    if (d.user) {
      const u = d.user;
      html += meta('用户名', u.name || '-') + meta('邮箱', u.email || '-') +
        meta('Pro', u.pro ? '是' : '否') + meta('Tier', u.teams_tier || '-') + meta('User ID', u.user_id || '-');
    }
    const ps = d.plan_status || {}, pi = d.plan_info || {};
    if (d.plan_status || d.plan_info) {
      html += meta('套餐', ps.plan_name || pi.plan_name || '-') +
        meta('计费', ps.billing_strategy || pi.billing_strategy || '-') +
        meta('月 Prompt', fmtQuota(ps.monthly_prompt_credits ?? pi.monthly_prompt_credits)) +
        meta('可用 Prompt', fmtQuota(ps.available_prompt_credits)) +
        meta('可用 Flow', fmtQuota(ps.available_flow_credits)) +
        meta('可用 Flex', fmtQuota(ps.available_flex_credits)) +
        meta('周期', (ps.plan_start || '?') + ' ~ ' + (ps.plan_end || '?')) +
        meta('ACU', (ps.acu_consumed ?? '-') + ' / ' + (ps.acu_limit ?? '-')) +
        meta('超额 micros', ps.overage_balance_micros ?? '-');
    }
    html += '</div>';
    if (!d.user && d.user_status_error) {
      html += '<div class="err-banner" style="margin-top:10px">账户用量拉取失败: ' + esc(d.user_status_error) + '</div>';
    } else if (!d.user) {
      html += '<div class="note">暂无账户用量数据。</div>';
    }
    if (d.capacity_error) html += '<div class="err-banner" style="margin-top:8px">' + esc(d.capacity_error) + '</div>';
    $('accountPanel').innerHTML = html;

    // 渠道 + 容量 + IDE
    const pv = $('providerPanel');
    let phtml = '';
    if (d.capacity) {
      phtml += '<div class="grid" style="margin-bottom:8px">' +
        meta('有容量', d.capacity.has_capacity ? '是' : '否') +
        meta('活跃会话', d.capacity.active_sessions ?? '-') +
        meta('容量消息', d.capacity.message || '-') +
        (d.ide_status ? meta('IDE 状态', d.ide_status.level || '-') + meta('IDE 消息', d.ide_status.message || '-') : '') +
        '</div>';
    }
    if (d.providers && d.providers.length) {
      phtml += '<div class="chip-row" style="margin:0">' +
        d.providers.map(p => '<span class="chip" style="cursor:default">' + esc(p.display_name || p.provider) + ' <span class="muted">' + esc(p.provider || '') + '</span></span>').join('') + '</div>';
    }
    pv.style.display = phtml ? '' : 'none';
    $('providerBody').innerHTML = phtml;

    // 模型状态告警
    const ms = $('modelStatusPanel');
    const bad = (d.model_statuses || []).filter(s => /WARN|ERROR|FATAL|DOWN/i.test(String(s.status || '')));
    if (bad.length) {
      ms.style.display = '';
      $('modelStatusBody').innerHTML = '<div class="grid">' + bad.map(s =>
        '<div class="mini"><span class="k">' + esc(s.model_uid || s.model || '-') + '</span><span class="v" style="color:var(--err)">' +
        esc(String(s.status || '-')) + (s.message ? ' · ' + esc(s.message) : '') + '</span></div>').join('') + '</div>';
    } else {
      ms.style.display = 'none';
    }
  }

  Tabs.register('quota', load);
  Polls.add('quota', load, 60000);
  return {};
})();
