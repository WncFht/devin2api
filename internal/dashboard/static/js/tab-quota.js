// 配额页：日/周配额曲线与燃烧速率预测 + 账户/套餐/容量/渠道/模型状态。
// status 端点聚合多个上游调用（最长 610s 超时），失败字段以 *_error 透出。

import {
  $, api, esc, fmtUnix, fmtUnixShort, fmtIn, fmtQuota, kpi, meta,
  Tabs, Polls, morph,
} from './core.js';
import { Charts } from './charts.js';

const C = Charts.C, F = Charts.F;

async function load() {
  try {
    const d = await api('/quota');
    renderQuota(d);
  } catch (e) {
    morph($('quotaKpis'), '<div class="note">配额数据拉取失败: ' + esc(String(e)) + '</div>');
  }
  try {
    const d = await api('/status');
    renderStatus(d);
  } catch (e) {
    morph($('accountPanel'), '<h3>账户与套餐</h3><div class="note">状态拉取失败: ' + esc(String(e)) + '</div>');
  }
}

function renderQuota(d) {
  const pts = d.points || [];
  const kEl = $('quotaKpis');
  if (!pts.length) {
    morph(kEl, '<div class="note">暂无配额快照——采样器每 debug.quota_interval_minutes 分钟写一条，重启后开始积累。</div>');
    // quotaCurve 是静态图容器：Charts.empty 直接在既有实例上画空态，
    // 不要 innerHTML 清空——那会 detach echarts 的 canvas 而实例仍存活。
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
  morph(kEl, html);

  Charts.render($('quotaCurve'), {
    dataZoom: Charts.zoom(pts),
    yAxis: { min: 0, max: 100, axisLabel: { formatter: '{value}%', color: C.axis, fontSize: F.xs }, splitLine: { lineStyle: { color: Charts.slate(0.08) } } },
    tooltip: { trigger: 'axis', valueFormatter: v => v == null ? '-' : Number(v).toFixed(1) + '%' },
    series: [
      Charts.line('日剩余', C.accent, Charts.tsList(pts, 'at', 'daily_remaining')),
      Charts.line('周剩余', C.pink, Charts.tsList(pts, 'at', 'weekly_remaining')),
    ],
  });
}

// fmtDay 把 ISO 时间串渲染成本地日期（2026/9/7），用于套餐周期这类
// 只关心日不关心时刻的字段；无法解析时透传原串。
function fmtDay(v) {
  const d = new Date(v);
  return isNaN(d) ? (v || '?') : d.getFullYear() + '/' + (d.getMonth() + 1) + '/' + d.getDate();
}

// creditUsage 把月度额度与可用余额合成「已用 used / total（剩 avail）」。
// monthly≤0（上游 -1 表示不按固定额度计费）时退化为只显示可用量；
// 已用由 monthly-available 反推，额外购credit导致 available>monthly 时按 0 计。
function creditUsage(monthly, available) {
  // Number(null)===0，须先判空：字段缺失与"可用为 0"语义不同。
  const m = Number(monthly), a = available == null ? NaN : Number(available);
  if (!Number.isFinite(m) || m <= 0) return '可用 ' + fmtQuota(available);
  if (!Number.isFinite(a)) return '月 ' + fmtQuota(m);
  return '已用 ' + Math.max(0, Math.round(m - a)) + ' / ' + fmtQuota(m) + '（剩 ' + fmtQuota(a) + '）';
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
      meta('Prompt', creditUsage(ps.monthly_prompt_credits ?? pi.monthly_prompt_credits, ps.available_prompt_credits)) +
      meta('Flow', creditUsage(ps.monthly_flow_credits ?? pi.monthly_flow_credits, ps.available_flow_credits)) +
      meta('Flex', '可用 ' + fmtQuota(ps.available_flex_credits)) +
      meta('周期', fmtDay(ps.plan_start) + ' ~ ' + fmtDay(ps.plan_end)) +
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
  morph($('accountPanel'), html);

  // 渠道 + 容量 + IDE
  const pv = $('providerPanel');
  let phtml = '';
  if (d.capacity) {
    phtml += '<div class="grid" style="margin-bottom:8px">' +
      meta('有容量', d.capacity.has_capacity ? '是' : '否') +
      meta('活跃会话', d.capacity.active_sessions ?? '-') +
      meta('容量消息', d.capacity.message || '-') +
      (d.ide_status ? meta('IDE 状态', d.ide_status.level === 'UNSPECIFIED' ? '—' : (d.ide_status.level || '-')) + meta('IDE 消息', d.ide_status.message || '-') : '') +
      '</div>';
  }
  if (d.providers && d.providers.length) {
    phtml += '<div class="chip-row flat">' +
      d.providers.map(p => {
        const name = p.display_name || p.provider || '-';
        // provider 枚举与 display_name 同源时（OPENAI/OpenAI）只留前者，
        // 否则括号附原始枚举值区分显示名与渠道标识。
        const dup = p.provider && p.display_name && String(p.provider).toUpperCase() === String(p.display_name).toUpperCase();
        return '<span class="chip static">' + esc(name) +
          (p.provider && !dup ? ' <span class="muted">' + esc(p.provider) + '</span>' : '') + '</span>';
      }).join('') + '</div>';
  }
  pv.style.display = phtml ? '' : 'none';
  morph($('providerBody'), phtml);

  // 模型状态告警
  const ms = $('modelStatusPanel');
  const bad = (d.model_statuses || []).filter(s => /WARN|ERROR|FATAL|DOWN/i.test(String(s.status || '')));
  if (bad.length) {
    ms.style.display = '';
    morph($('modelStatusBody'), '<div class="grid">' + bad.map(s =>
      '<div class="mini"><span class="k">' + esc(s.model_uid || s.model || '-') + '</span><span class="v status-err">' +
      esc(String(s.status || '-')) + (s.message ? ' · ' + esc(s.message) : '') + '</span></div>').join('') + '</div>');
  } else {
    ms.style.display = 'none';
  }
}

Tabs.register('quota', load);
Polls.add('quota', load, 60000);
