function escapeHtml(s) {
  return String(s).replace(/[&<>"']/g, c => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'
  }[c]));
}

// Green from 99 %, amber from 95 %, red below: a link that drops one read in a
// hundred is normal; one that drops one in twenty needs a look.
function healthColor(pct) {
  if (pct >= 99) return '#10B981';
  if (pct >= 95) return '#F59E0B';
  return '#EF4444';
}

// Line icons, 48x48, drawn in currentColor so the tile can tint them.
const ICONS = {
  // Solar inverter: casing with a status LED, a sine wave (AC) over a flat line (DC), cable stubs below.
  inverter: '<svg viewBox="0 0 48 48" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">' +
    '<rect x="8" y="5" width="32" height="32" rx="4"/>' +
    '<circle cx="14" cy="11" r="1.2" fill="currentColor" stroke="none"/><path d="M33 11h3"/>' +
    '<path d="M12 20q3-8 6 0t6 0 6 0 6 0"/><path d="M12 29h24"/>' +
    '<path d="M16 37v6M24 37v6M32 37v6"/></svg>',
  // Wall connector: rounded wall unit with a T, and its cable looping down to the handle.
  twc: '<svg viewBox="0 0 48 48" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">' +
    '<rect x="9" y="4" width="22" height="24" rx="8"/>' +
    '<path d="M16 12h8M20 12v8"/>' +
    '<path d="M20 28v6c0 5 4 8 9 8h3c4 0 6-2 6-5V26"/>' +
    '<rect x="33" y="17" width="9" height="9" rx="2.5"/><path d="M36 17v-3M39 17v-3"/></svg>',
};

function fmtAgo(iso, nowMs) {
  if (!iso) return 'never';
  const s = Math.max(0, Math.round((nowMs - new Date(iso).getTime()) / 1000));
  if (s < 5) return 'just now';
  if (s < 60) return s + ' s ago';
  if (s < 3600) return Math.floor(s / 60) + ' min ago';
  if (s < 86400) return Math.floor(s / 3600) + ' h ' + Math.floor(s % 3600 / 60) + ' min ago';
  return Math.floor(s / 86400) + ' d ago';
}

function fmtMs(ms) {
  return ms >= 1000 ? (ms / 1000).toFixed(2) + ' s' : ms + ' ms';
}

// Half-circle gauge, 180° sweep, value 0-100.
function gaugeSvg(pct, hasData) {
  const cx = 110, cy = 100, r = 85;
  const pt = f => {
    const a = Math.PI * (1 - f);
    return [cx + r * Math.cos(a), cy - r * Math.sin(a)];
  };
  const [x0, y0] = pt(0), [x1, y1] = pt(1);
  const track = 'M' + x0 + ',' + y0 + ' A' + r + ',' + r + ' 0 0 1 ' + x1 + ',' + y1;
  let fill = '';
  if (hasData && pct > 0) {
    const [x, y] = pt(Math.min(pct, 99.999) / 100);
    fill = '<path d="M' + x0 + ',' + y0 + ' A' + r + ',' + r + ' 0 0 1 ' + x + ',' + y +
      '" fill="none" stroke="' + healthColor(pct) + '" stroke-width="16" stroke-linecap="round"/>';
  }
  let label = '–';
  if (hasData) {
    const shown = pct >= 99.95 ? '100' : pct.toFixed(pct >= 99 ? 2 : 1);
    label = shown + '<tspan font-size="16">%</tspan>';
  }
  return '<svg viewBox="0 0 220 120" class="gauge">' +
    '<path d="' + track + '" fill="none" style="stroke:var(--track)" stroke-width="16" stroke-linecap="round"/>' + fill +
    '<text x="110" y="92" text-anchor="middle" class="gauge-num">' + label + '</text></svg>';
}

// One bar per hour of the trailing day, coloured by that hour's success rate.
function hoursStrip(hours) {
  let html = '<div class="strip">';
  hours.forEach((h, i) => {
    const total = h.ok + h.fail;
    const ago = hours.length - 1 - i;
    const when = ago === 0 ? 'this hour' : ago + ' h ago';
    let cls = 'none', tip = when + ': no data';
    if (total > 0) {
      const pct = h.ok / total * 100;
      cls = pct >= 99 ? 'good' : pct >= 95 ? 'warn' : 'bad';
      tip = when + ': ' + pct.toFixed(1) + '% (' + h.fail + ' failed of ' + total + '), avg ' + fmtMs(h.avg_ms);
    }
    html += '<span class="bar ' + cls + '" title="' + escapeHtml(tip) + '"></span>';
  });
  return html + '</div><div class="strip-axis"><span>24 h ago</span><span>now</span></div>';
}

function stat(label, value, cls) {
  return '<div class="stat"><div class="stat-val ' + (cls || '') + '">' + value + '</div>' +
    '<div class="stat-lbl">' + label + '</div></div>';
}

function renderPanel(cfg, st) {
  // The icon tile takes the gauge's health colour, grey when there is nothing to judge.
  const tone = st && st.total > 0 ? (st.percent >= 99 ? 'good' : st.percent >= 95 ? 'warn' : 'bad') : 'none';
  let html = '<div class="panel-head"><div class="icon-tile ' + tone + '">' + ICONS[cfg.key] + '</div>' +
    '<div><h2>' + cfg.title + '</h2><div class="panel-sub">' + cfg.sub + '</div></div></div>';
  if (!st) return html + '<p class="empty">Not configured.</p>';
  const nowMs = new Date(st.server_time).getTime();
  const has = st.total > 0;
  html += gaugeSvg(st.percent, has);
  html += '<div class="stats">' +
    stat('Avg response', has && st.ok ? fmtMs(st.avg_ms) : '–') +
    stat('Slowest', has && st.ok ? fmtMs(st.max_ms) : '–') +
    stat('Reads', st.total.toLocaleString()) +
    stat('Failed', st.fail.toLocaleString(), st.fail ? 'bad' : '') +
    '</div>';
  const streak = st.streak_n ? st.streak_n.toLocaleString() + (st.streak_ok ? ' good' : ' failed') + ' in a row' : '';
  html += '<div class="seen">' +
    '<div><span class="dot good"></span>Last good <b>' + fmtAgo(st.last_ok, nowMs) + '</b></div>' +
    '<div><span class="dot bad"></span>Last failure <b>' + fmtAgo(st.last_fail, nowMs) + '</b></div>' +
    (streak ? '<div class="streak ' + (st.streak_ok ? 'good' : 'bad') + '">' + streak + '</div>' : '') +
    '</div>';
  html += hoursStrip(st.hours);
  if (st.top_errors.length) {
    html += '<h3>Why reads fail</h3><ul class="errors">';
    st.top_errors.forEach(e => {
      html += '<li><span class="count">' + e.count + '&times;</span>' + escapeHtml(e.error || 'error') + '</li>';
    });
    html += '</ul>';
  }
  if (st.failures.length) {
    html += '<details><summary>Recent failures (' + st.failures.length + ')</summary><div class="fails">';
    st.failures.slice(0, 50).forEach(f => {
      html += '<div class="fail-row"><span>' + escapeHtml(new Date(f.time).toLocaleTimeString()) + '</span>' +
        '<span>' + fmtMs(f.duration_ms) + '</span><span>' + escapeHtml(f.error) + '</span></div>';
    });
    html += '</div></details>';
  }
  return html;
}

function startLinkHealth(endpoint, panels) {
  async function refresh() {
    try {
      const r = await fetch(endpoint, { cache: 'no-store' });
      const data = await r.json();
      panels.forEach(p => {
        const el = document.getElementById(p.key);
        // Keep an opened failures list open across refreshes.
        const open = el.querySelector('details[open]') !== null;
        el.innerHTML = renderPanel(p, data[p.key]);
        if (open) el.querySelector('details').open = true;
      });
    } catch (e) { /* keep the last picture; try again next tick */ }
  }
  refresh();
  setInterval(refresh, 3000);
}
