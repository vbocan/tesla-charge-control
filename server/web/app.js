const MAX_OUTPUT = (typeof CONFIG !== 'undefined' && CONFIG.maxOutputW) || 6000;
const GAUGE_MAX_GRID = MAX_OUTPUT;
const LOAD_MAX = MAX_OUTPUT;
const INV_MAX = MAX_OUTPUT;

let connected = false;
let lastUpdateTime = 0;

// --- Two-way gauge (battery, grid) ---

function updateGauge(arcId, value, posMax, negMax, posGradient, negGradient) {
  const el = document.getElementById(arcId);
  if (!el) return;

  const cx = 110, cy = 115, r = 85;

  if (Math.abs(value) < 5) {
    el.setAttribute('d', '');
    return;
  }

  let norm;
  if (value > 0) {
    norm = Math.min(value / posMax, 1);
  } else {
    norm = Math.max(value / negMax, -1);
  }

  const zeroX = cx, zeroY = cy - r;
  const angle = Math.PI / 2 * (1 - norm);
  const valX = cx + r * Math.cos(angle);
  const valY = cy - r * Math.sin(angle);

  let d;
  if (value > 0) {
    d = `M ${zeroX} ${zeroY} A ${r} ${r} 0 0 1 ${valX} ${valY}`;
    el.setAttribute('stroke', `url(#${posGradient})`);
  } else {
    d = `M ${zeroX} ${zeroY} A ${r} ${r} 0 0 0 ${valX} ${valY}`;
    el.setAttribute('stroke', `url(#${negGradient})`);
  }
  el.setAttribute('d', d);
}

// --- One-way load gauge (0 to max, left to right) ---

function updateLoadGauge(value) {
  const el = document.getElementById('loadArc');
  if (!el) return;

  // The ring is a 270° sweep (387 of the 516 px circumference) from 0 to LOAD_MAX.
  const frac = Math.min(Math.max(value, 0), LOAD_MAX) / LOAD_MAX;
  el.setAttribute('stroke-dasharray', (387 * frac).toFixed(1) + ' 516');
  el.setAttribute('data-level', frac >= 0.9 ? 'high' : frac >= 0.7 ? 'warn' : 'ok');

  const pill = document.getElementById('loadPill');
  if (pill) {
    const near = frac >= 0.9;
    pill.textContent = near ? 'Near limit' : 'Healthy';
    pill.classList.toggle('bad', near);
    pill.classList.toggle('ok', !near);
  }
}

// --- Live power flow (solar / battery / grid / house / car) ---

function fmtKW(w) {
  return (Math.abs(w) / 1000).toFixed(2) + ' kW';
}

function setFlow(id, watts, reversed) {
  const el = document.getElementById(id);
  if (!el) return;
  el.classList.toggle('off', Math.abs(watts) < 50);
  el.classList.toggle('rev', !!reversed);
  // The beads get bigger as more power flows: 4 px up to ~9 px at the limit.
  const share = Math.min(Math.abs(watts) / LOAD_MAX, 1);
  el.style.strokeWidth = (4 + 5 * share).toFixed(1);
}

// Paths for solar, battery and grid run from the node into the house, so
// "reversed" means the energy flows out of the house to that node (battery
// charging, grid export). The car path runs from the house, so it never reverses.
function updatePowerFlow(d) {
  setText('pfSolar', fmtKW(d.total_pv));
  setText('pfSolarSub', d.total_pv > 10 ? 'Producing' : 'No production');
  setFlow('fSolar', d.total_pv, false);

  setText('pfBatt', fmtKW(d.batt_power));
  // The state of charge gets its own caption under the glyph: on one line with
  // the state, "100% · Discharging" is wider than the node box.
  setText('pfBattSoc', d.batt_soc + '%');
  setText('pfBattSub',
    d.batt_power < -10 ? 'Charging' : d.batt_power > 10 ? 'Discharging' : 'Idle');
  // The battery icon is 22 units tall inside its outline; fill it from the bottom.
  const fill = document.getElementById('pfBattFill');
  if (fill) {
    const h = Math.max(0, Math.min(100, d.batt_soc)) / 100 * 17;
    fill.setAttribute('height', h.toFixed(1));
    fill.setAttribute('y', (8.5 - h).toFixed(1));
  }
  setFlow('fBatt', d.batt_power, d.batt_power < 0);

  setText('pfGrid', fmtKW(d.grid_power));
  setText('pfGridSub', d.grid_power < -10 ? 'Exporting' : d.grid_power > 10 ? 'Importing' : 'Balanced');
  setFlow('fGrid', d.grid_power, d.grid_power < 0);

  setText('pfHouse', fmtKW(d.load_power));
  setText('pfHouseSub', 'of ' + (LOAD_MAX / 1000).toFixed(1) + ' kW limit');
  const houseArc = document.getElementById('pfHouseArc');
  if (houseArc) {
    const frac = Math.min(Math.max(d.load_power, 0), LOAD_MAX) / LOAD_MAX;
    houseArc.setAttribute('stroke-dasharray', (439.8 * frac).toFixed(1) + ' 440');
    houseArc.setAttribute('data-level', frac >= 0.9 ? 'high' : frac >= 0.7 ? 'warn' : 'ok');
  }

  let note = 'Solar covers the house';
  if (d.grid_power > 50 && d.batt_power > 50) note = 'Battery and grid support the house';
  else if (d.grid_power > 50) note = 'Grid supplies the shortfall';
  else if (d.batt_power > 50) note = 'Battery supports the house';
  else if (d.grid_power < -50) note = 'Exporting surplus to the grid';
  else if (d.total_pv < 10) note = 'No solar production';
  setText('flowNote', note);
}

function updateCarFlow(watts, twc) {
  const node = document.getElementById('pfCarNode');
  if (node) node.style.display = '';
  setText('pfCar', fmtKW(watts));
  // The contactor is what actually feeds the car. With it open, the few dozen
  // watts the connector still reports are its own idle draw, not a charge, so
  // neither the label nor the beads may claim one. Same test as twcCarActivity.
  const charging = twc && twc.online
    ? (twc.contactor_closed && watts >= 100)
    : watts >= 100;
  setText('pfCarSub', charging ? 'Charging' : twcSeesCar(twc) ? 'Plugged in' : 'Not charging');
  setFlow('fCar', charging ? watts : 0, false);
}

// --- Sparklines: the last few minutes of battery, grid and solar power ---

const SPARK_POINTS = 90;
const sparkHistory = {spBatt: [], spGrid: [], spSolar: []};

function pushSpark(id, value) {
  const hist = sparkHistory[id];
  hist.push(value);
  if (hist.length > SPARK_POINTS) hist.shift();
  const el = document.getElementById(id);
  if (!el || hist.length < 2) return;
  const lo = Math.min(...hist), hi = Math.max(...hist);
  const span = (hi - lo) || 1;
  el.setAttribute('d', hist.map((v, i) => {
    const x = (i / (SPARK_POINTS - 1)) * 200;
    const y = 36 - ((v - lo) / span) * 32;
    return (i ? 'L' : 'M') + x.toFixed(1) + ' ' + y.toFixed(1);
  }).join(' '));
}

// --- Light / dark theme ---

function initTheme() {
  const btn = document.getElementById('themeBtn');
  if (!btn) return;
  btn.addEventListener('click', () => {
    const root = document.documentElement;
    const next = root.getAttribute('data-theme') === 'light' ? 'dark' : 'light';
    root.setAttribute('data-theme', next);
    try { localStorage.setItem('theme', next); } catch (e) { /* storage may be blocked */ }
  });
}

// --- Solar dual gauge (PV1 left, PV2 right, both from center) ---

function updateSolarGauge(pv1, pv2) {
  const cx = 110, cy = 115, r = 85;
  const zeroX = cx, zeroY = cy - r;

  // PV1 arc: from center leftward
  const pv1El = document.getElementById('pv1Arc');
  if (pv1El) {
    if (pv1 < 5) {
      pv1El.setAttribute('d', '');
    } else {
      const norm = Math.min(pv1 / CONFIG.pvPerStringW, 1);
      const angle = Math.PI / 2 * (1 + norm); // PI/2 to PI (center to left)
      const x = cx + r * Math.cos(angle);
      const y = cy - r * Math.sin(angle);
      pv1El.setAttribute('d', `M ${zeroX} ${zeroY} A ${r} ${r} 0 0 0 ${x} ${y}`);
    }
  }

  // PV2 arc: from center rightward
  const pv2El = document.getElementById('pv2Arc');
  if (pv2El) {
    if (pv2 < 5) {
      pv2El.setAttribute('d', '');
    } else {
      const norm = Math.min(pv2 / CONFIG.pvPerStringW, 1);
      const angle = Math.PI / 2 * (1 - norm); // PI/2 to 0 (center to right)
      const x = cx + r * Math.cos(angle);
      const y = cy - r * Math.sin(angle);
      pv2El.setAttribute('d', `M ${zeroX} ${zeroY} A ${r} ${r} 0 0 1 ${x} ${y}`);
    }
  }
}

// --- Inverter gauge (0 to max, left to right, small) ---

function updateInvGauge(value) {
  const el = document.getElementById('invArc');
  if (!el) return;

  const cx = 110, cy = 115, r = 85;

  if (value < 5) {
    el.setAttribute('d', '');
    return;
  }

  const frac = Math.min(Math.max(value, 0), INV_MAX) / INV_MAX;
  const startAngle = Math.PI; // left
  const endAngle = Math.PI * (1 - frac);
  const startX = cx + r * Math.cos(startAngle);
  const startY = cy - r * Math.sin(startAngle);
  const endX = cx + r * Math.cos(endAngle);
  const endY = cy - r * Math.sin(endAngle);
  const largeArc = 0; // arc never exceeds 180° on a semicircular gauge

  el.setAttribute('d', `M ${startX} ${startY} A ${r} ${r} 0 ${largeArc} 1 ${endX} ${endY}`);
}

// --- Source breakdown ---

function updateSources(d) {
  const load = Math.max(d.load_power, 0);
  const gridContrib = Math.max(d.grid_power, 0);
  const battContrib = Math.max(d.batt_power, 0);
  const solarContrib = Math.max(load - gridContrib - battContrib, 0);

  setText('srcSolarW', solarContrib + ' W');
  setText('srcBattW', battContrib + ' W');
  setText('srcGridW', gridContrib + ' W');

  // Proportional bar
  if (load > 0) {
    setWidth('srcBarSolar', (solarContrib / load) * 100);
    setWidth('srcBarBatt', (battContrib / load) * 100);
    setWidth('srcBarGrid', (gridContrib / load) * 100);
  } else {
    setWidth('srcBarSolar', 0);
    setWidth('srcBarBatt', 0);
    setWidth('srcBarGrid', 0);
  }
}

// --- Connection status ---

const STALE_THRESHOLD_S = 30; // data older than this = offline

function updateStatus() {
  const el = document.getElementById('timestamp');
  const dot = document.getElementById('statusDot');
  if (!el || !dot) return;

  if (!lastUpdateTime) {
    el.textContent = 'Connecting...';
    el.title = 'Waiting for first data from the inverter.';
    dot.classList.remove('connected');
    dot.classList.remove('offline');
    return;
  }

  const ago = Math.round((Date.now() - lastUpdateTime) / 1000);
  const stale = ago > STALE_THRESHOLD_S || !connected;
  // Show the "X ago" suffix only once the gap clearly exceeds a single poll
  // cycle, so a stray late packet doesn't briefly show "(7s ago)".
  const showAge = ago > 10;

  if (stale) {
    el.textContent = 'Offline (' + fmtAgo(ago) + ' ago)';
    el.title = 'Last update ' + fmtAgo(ago) + ' ago.';
    dot.classList.remove('connected');
    dot.classList.add('offline');
  } else if (showAge) {
    el.textContent = 'Connected (' + fmtAgo(ago) + ' ago)';
    el.title = 'Last fresh data ' + fmtAgo(ago) + ' ago — inverter may be struggling.';
    dot.classList.add('connected');
    dot.classList.remove('offline');
  } else {
    el.textContent = 'Connected';
    el.title = 'Live data every ' + CONFIG.pollIntervalS + 's.';
    dot.classList.add('connected');
    dot.classList.remove('offline');
  }
}

function fmtAgo(seconds) {
  if (seconds < 2) return 'just now';
  if (seconds < 60) return seconds + 's';
  return Math.floor(seconds / 60) + 'm ' + (seconds % 60) + 's';
}

// --- Data Update ---

function updateDashboard(payload) {
  const d = payload.data;
  const proj = payload.projection;

  // Use the server's timestamp of the last successful inverter read so the age
  // is accurate across browser refreshes (the cached SSE payload still carries
  // the original read time, not the resend time).
  if (payload.inverter_ok_unix_ms) {
    lastUpdateTime = payload.inverter_ok_unix_ms;
  } else {
    lastUpdateTime = Date.now();
  }
  updateStatus();

  // --- House Load Gauge ---
  updateLoadGauge(d.load_power);
  const loadEl = document.getElementById('loadGaugeVal');
  if (loadEl) loadEl.innerHTML = (d.load_power / 1000).toFixed(2) + '<small>kW</small>';
  setText('loadGaugeLabel', 'of ' + (LOAD_MAX / 1000).toFixed(1) + ' kW limit');
  updateSources(d);
  updatePowerFlow(d);
  pushSpark('spBatt', -d.batt_power);   // up = charging
  pushSpark('spGrid', -d.grid_power);   // up = exporting
  pushSpark('spSolar', d.total_pv);

  // --- Battery Gauge (negated: discharge left, charge right) ---
  updateGauge('battArc', -d.batt_power, CONFIG.battMaxChargeW, CONFIG.battMaxDischargeW, 'battChargeGrad', 'battDischargeGrad');
  setText('battGaugeVal', Math.abs(d.batt_power) + ' W');
  const battState = d.batt_power < -10 ? 'Charging' : d.batt_power > 10 ? 'Discharging' : 'Idle';
  setText('battGaugeLabel', battState);

  // --- Grid Gauge (negated: import left, export right) ---
  updateGauge('gridArc', -d.grid_power, GAUGE_MAX_GRID, GAUGE_MAX_GRID, 'gridExportGrad', 'gridImportGrad');
  setText('gridGaugeVal', Math.abs(d.grid_power) + ' W');
  const gridState = d.grid_power < 0 ? 'Exporting' : d.grid_power > 0 ? 'Importing' : '--';
  setText('gridGaugeLabel', gridState);

  // --- Battery details ---
  setWidth('socBar', d.batt_soc);
  setText('socBarLabel', d.batt_soc + ' %');
  setText('battVolt', d.batt_voltage.toFixed(1) + ' V');
  setText('battTempVal', d.batt_temp.toFixed(1) + ' \u00B0C');
  setText('battCurr', d.batt_current.toFixed(1) + ' A');

  let projText = '--';
  if (proj.state === 'collecting') {
    projText = 'Collecting...';
  } else if (proj.state === 'charging') {
    projText = proj.hours > 0 ? 'Full in ' + fmtHM(proj.hours) : 'Full';
  } else if (proj.state === 'discharging') {
    projText = proj.hours > 0 ? 'Empty in ' + fmtHM(proj.hours) : 'At floor';
  } else if (proj.state === 'idle') {
    projText = proj.remaining_kwh > 0 ? proj.remaining_kwh.toFixed(1) + ' kWh avail' : 'Idle';
  }
  setText('battProj', projText);

  // --- Grid details ---
  setText('gridVolt', d.grid_voltage.toFixed(1) + ' V');
  setText('gridFreq', d.grid_freq.toFixed(2) + ' Hz');
  setText('ctL1', d.ct_l1 + ' W');
  setText('extCtL1', d.ext_ct_l1 + ' W');

  // --- Solar gauge ---
  updateSolarGauge(d.pv1_w, d.pv2_w);
  setText('solarGaugeVal', d.total_pv + ' W');
  setText('solarGaugeLabel', d.total_pv > 10 ? 'Total' : 'No production');
  setText('pv1Watt', d.pv1_w + ' W');
  setText('pv1Volt', d.pv1_v.toFixed(1) + ' V');
  setText('pv1Amp', d.pv1_a.toFixed(1) + ' A');
  setText('pv2Watt', d.pv2_w + ' W');
  setText('pv2Volt', d.pv2_v.toFixed(1) + ' V');
  setText('pv2Amp', d.pv2_a.toFixed(1) + ' A');

  // --- Inverter gauge ---
  updateInvGauge(d.inv_total);
  setText('invGaugeVal', d.inv_total + ' W');
  setText('invVolt', d.inv_voltage.toFixed(1) + ' V');
  setText('invFreq', d.inv_freq.toFixed(2) + ' Hz');
  setText('invCurr', d.inv_current.toFixed(2) + ' A');
  setText('invSelf', d.self_consumption + ' W');

  // --- Energy counters ---
  setText('dailyProd', d.daily_production.toFixed(1) + ' kWh');
  setText('dailyBought', d.daily_bought.toFixed(1) + ' kWh');
  setText('dailySold', d.daily_sold.toFixed(1) + ' kWh');
  setText('dailyCons', d.daily_consumption.toFixed(1) + ' kWh');
  setText('dailyBattChg', d.daily_batt_charge.toFixed(1) + ' kWh');
  setText('dailyBattDis', d.daily_batt_discharge.toFixed(1) + ' kWh');

  // --- Temperature ---
  setText('tempDC', d.dc_temp.toFixed(1) + ' \u00B0C');
  setText('tempAC', d.ac_temp.toFixed(1) + ' \u00B0C');

  // --- Tesla ---
  if (CONFIG.teslaEnabled) {
    updateTesla(payload.tesla);
  }
}

// --- Helpers ---

function setText(id, text) {
  const el = document.getElementById(id);
  if (el) el.textContent = text;
}

function setWidth(id, pct) {
  const el = document.getElementById(id);
  if (el) el.style.width = Math.min(100, Math.max(0, pct)) + '%';
}

function fmtHM(hours) {
  if (hours < 0) return '--';
  const h = Math.floor(hours);
  const m = Math.round((hours - h) * 60);
  return h > 0 ? h + 'h ' + m + 'm' : m + 'm';
}

// --- Tesla charging UI ---

function updateTesla(t) {
  if (!t || !t.enabled) return;

  // Show the section wrapper and header log link
  const section = document.getElementById('teslaSection');
  if (section) section.style.display = '';
  const logLink = document.getElementById('logLink');
  if (logLink) logLink.style.display = '';
  const pairLink = document.getElementById('openPairModal');
  if (pairLink) pairLink.style.display = '';

  // Update mode buttons — disable all when TWC offline, disable Solar/Max when car not connected
  const twcOnline = t.twc && t.twc.online;
  const carConnected = twcOnline && t.twc.vehicle_connected;
  document.querySelectorAll('.tesla-mode-btn').forEach(btn => {
    const isActive = btn.dataset.mode === t.mode;
    btn.classList.toggle('active', isActive);
    if (!twcOnline) {
      btn.disabled = true;
    } else if (btn.dataset.mode !== 'idle') {
      btn.disabled = !carConnected;
    } else {
      btn.disabled = false;
    }
  });

  // Gauge: 0 to max amps, left to right
  updateTeslaGauge(t.current_amps, t.max_amps);
  setText('teslaMaxLabel', t.max_amps + ' A');
  setText('teslaGaugeVal', t.current_amps + ' A');
  setText('teslaGaugePower', t.power_w + ' W');

  // State banner above gauge
  const bannerEl = document.getElementById('teslaModeStateBanner');
  if (bannerEl) {
    const st = t.mode === 'idle' ? 'idle' : (t.mode_state || 'idle');
    const labels = {idle:'Idle', standby:'Standby', starting:'Starting...', charging:'Charging', grace:'Grace Period', cooldown:'Cooldown'};
    const amps = t.current_amps || 0;
    const powerW = (t.power_w || 0).toLocaleString();
    const chargingDesc =
      t.mode === 'max'   ? `Charging at ${amps} A (${powerW} W) — using solar, battery, and grid.` :
      t.mode === 'nobattery' ? `Charging at ${amps} A (${powerW} W) — solar goes to the car first; grid covers any shortfall. Battery stays at its current level.` :
                           `Charging at ${amps} A (${powerW} W) from solar surplus. The inverter charges the battery first; the car gets the remainder.`;
    const hints = {
      idle: '',
      standby: 'Solar production below 200 W. Monitoring only.',
      starting: 'Surplus sustained. BLE wake sent — starting charge shortly.',
      charging: chargingDesc,
      grace: 'Surplus dropped. Holding at minimum — will stop if it doesn\'t recover.',
      cooldown: 'Contactor protection. Restart available shortly.',
    };
    bannerEl.textContent = labels[st] || st;
    bannerEl.setAttribute('data-state', st);
    const descEl = document.getElementById('teslaModeStateDesc');
    // Charging state: prefer our composed mode-aware sentence over the backend reason.
    // Other states: backend reason carries dynamic info (remaining seconds, home load, etc).
    if (descEl) {
      descEl.textContent = st === 'charging' ? chargingDesc : (t.reason || hints[st] || '');
    }
  }

  // TWC vitals
  if (t.twc) updateTWC(t.twc);

  // The wall connector's own reading is the car's real draw; fall back to the
  // commanded power while it is offline.
  updateCarFlow(t.twc && t.twc.online ? (t.twc.actual_power_w || 0) : (t.power_w || 0), t.twc);

  // Vehicle charge (SoC)
  updateVehicle(t.vehicle, t.twc);

  updatePauseBanner(t);
}

// Car charging can be paused on purpose: during a grid outage (to keep the battery
// reserve for the house), while the grid proves stable afterwards, or when the Pi
// has lost the inverter. The server says why in t.paused and explains in t.reason.
// It matters only while a car is connected: with none, the Vehicle Charge card
// already says "Waiting for car", and the server's reason has become "car not
// connected" anyway.
function updatePauseBanner(t) {
  const el = document.getElementById('teslaPauseBanner');
  if (!el) return;
  const carConnected = !!(t && t.twc && t.twc.online && t.twc.vehicle_connected);
  if (t && t.paused && carConnected) {
    el.style.display = '';
    el.textContent = t.reason;
    el.setAttribute('data-kind', t.paused);
  } else {
    el.style.display = 'none';
    el.textContent = '';
    el.removeAttribute('data-kind');
  }
}

// --- Vehicle charge (SoC) hero ---

// The SoC comes from the car over Bluetooth, but the wall connector reports the
// car independently. When the wall connector sees a car and we still have no
// reading, the car isn't missing: it is asleep (a sleeping car answers over
// Bluetooth slowly or not at all) or out of Bluetooth range.
function twcSeesCar(twc) {
  return !!(twc && twc.online && twc.vehicle_connected);
}

// Short description of what the wall connector says the car is doing.
function twcCarActivity(twc) {
  const w = twc.actual_power_w || 0;
  return (twc.contactor_closed && w >= 100) ? 'charging at ' + (w / 1000).toFixed(1) + ' kW' : 'plugged in';
}

function updateVehicle(v, twc) {
  const card = document.getElementById('teslaSocCard');
  if (!card) return;

  const pill = document.getElementById('socChargeState');
  const fill = document.getElementById('evBattFill');
  const limit = document.getElementById('evBattLimit');

  // No reading yet (first boot, car asleep, Bluetooth unreachable, or just
  // disconnected) — dim and wait.
  if (!v || !v.available) {
    card.classList.add('soc-waiting');
    if (fill) { fill.style.width = '0%'; fill.setAttribute('data-charging', 'false'); fill.removeAttribute('data-low'); }
    if (limit) limit.style.display = 'none';
    setText('evBattPct', '-- %');
    setText('socLimit', '-- %');
    setText('socEta', '--');
    setText('socAdded', '-- kWh');
    setText('socRange', '-- km');
    const unreachable = twcSeesCar(twc);
    if (pill) {
      pill.textContent = unreachable ? 'Not reachable' : 'Waiting for car';
      pill.setAttribute('data-state', unreachable ? 'unreachable' : 'unknown');
    }
    const staleEl = document.getElementById('socStale');
    if (staleEl) {
      if (unreachable) {
        staleEl.style.display = '';
        staleEl.textContent = 'Car may be asleep or out of Bluetooth range — the wall connector shows it ' + twcCarActivity(twc) + '. Checked only now and then while parked: woke it? Tap Refresh.';
      } else {
        staleEl.style.display = 'none';
      }
    }
    return;
  }

  card.classList.remove('soc-waiting');

  // Freshness gating. The reading is a cached BLE snapshot; while the car is
  // asleep, reads fail and the snapshot freezes — so a hours-old "Charging"
  // must NOT be presented as live (no animation, no ETA, no "Charging" pill).
  // A genuinely charging car is refreshed every ~3 min, so 5 min is a safe
  // "still live" cutoff; past 30 min we visibly de-emphasize the gauge.
  const FRESH_S = 300;
  const VERY_STALE_S = 1800;
  const age = v.age_s || 0;
  const fresh = age <= FRESH_S;
  const charging = fresh && v.charging_state === 'Charging';
  const pct = v.battery_level;

  card.classList.toggle('is-stale', age > VERY_STALE_S);

  // Battery fill + percentage
  setWidth('evBattFill', pct);
  setText('evBattPct', pct + ' %');
  if (fill) {
    fill.setAttribute('data-charging', charging ? 'true' : 'false');
    if (pct <= 15) fill.setAttribute('data-low', 'true');
    else fill.removeAttribute('data-low');
  }

  // Charge-limit marker
  if (limit) {
    if (v.charge_limit_soc > 0) {
      limit.style.display = '';
      limit.style.left = Math.min(100, Math.max(0, v.charge_limit_soc)) + '%';
    } else {
      limit.style.display = 'none';
    }
  }

  // Status pill. Only assert the live charging state when the reading is fresh.
  // A stale "Charging" snapshot degrades to a neutral "Last known" so the UI
  // never implies the car is charging while it's actually asleep/idle.
  if (pill) {
    if (fresh) {
      pill.textContent = prettyChargeState(v.charging_state);
      pill.setAttribute('data-state', (v.charging_state || 'unknown').toLowerCase());
    } else if (v.charging_state === 'Charging') {
      pill.textContent = 'Last known';
      pill.setAttribute('data-state', 'unknown');
    } else {
      // Non-charging states (Charged/Stopped/Idle) don't imply live activity,
      // so they're safe to keep showing even when a little stale.
      pill.textContent = prettyChargeState(v.charging_state);
      pill.setAttribute('data-state', (v.charging_state || 'unknown').toLowerCase());
    }
  }

  // Meta row
  setText('socLimit', v.charge_limit_soc + ' %');
  setText('socEta', (charging && v.minutes_to_full > 0) ? fmtMinutes(v.minutes_to_full) : '--');
  setText('socAdded', (v.charge_energy_added_kwh || 0).toFixed(1) + ' kWh');
  setText('socRange', Math.round(v.battery_range_km || 0) + ' km');

  // Stale note — shown whenever the reading is no longer "live" so the age is
  // always visible alongside a degraded pill.
  const staleEl = document.getElementById('socStale');
  if (staleEl) {
    if (!fresh) {
      staleEl.style.display = '';
      staleEl.textContent = 'Updated ' + fmtAgo(age) + ' ago';
    } else {
      staleEl.style.display = 'none';
    }
  }
}

// Ask the server to read the car now. The read goes over Bluetooth and can take
// a while, so the button stays off for a short spell; the new reading arrives
// through the normal live updates.
function initSocRefresh() {
  const btn = document.getElementById('socRefresh');
  if (!btn) return;
  btn.addEventListener('click', () => {
    btn.disabled = true;
    btn.textContent = 'Reading…';
    fetch('/api/tesla/refresh', {method: 'POST'})
      .catch(err => console.error('Car refresh error:', err));
    setTimeout(() => { btn.disabled = false; btn.textContent = 'Refresh'; }, 20000);
  });
}

function prettyChargeState(s) {
  switch (s) {
    case 'Charging': return 'Charging';
    case 'Complete': return 'Charged';
    case 'Stopped': return 'Stopped';
    case 'Starting': return 'Starting';
    case 'NoPower': return 'No power';
    case 'Disconnected': return 'Unplugged';
    case 'Calibrating': return 'Calibrating';
    default: return 'Idle';
  }
}

function fmtMinutes(mins) {
  if (mins <= 0) return '--';
  const h = Math.floor(mins / 60);
  const m = mins % 60;
  return h > 0 ? h + 'h ' + m + 'm' : m + 'm';
}

function updateTWC(twc) {
  const banner = document.getElementById('twcStatusBanner');
  const detailsEl = document.getElementById('twcDetails');
  if (!banner) return;

  // Clear gauge when offline
  if (!twc.online) {
    const arc = document.getElementById('twcArc');
    if (arc) arc.setAttribute('d', '');
    setText('twcGaugeVal', '-- A');
    setText('twcGaugePower', '-- W');
    if (detailsEl) detailsEl.style.display = 'none';
    banner.textContent = 'Offline';
    banner.setAttribute('data-state', 'offline');
    return;
  }

  // Gauge
  updateTWCGauge(twc.vehicle_current_a, CONFIG.teslaMaxAmps);
  setText('twcGaugeMaxLabel', CONFIG.teslaMaxAmps + ' A');
  setText('twcGaugeVal', twc.vehicle_current_a.toFixed(1) + ' A');
  setText('twcGaugePower', twc.actual_power_w.toLocaleString() + ' W');

  // Details
  if (detailsEl) detailsEl.style.display = '';
  setText('twcGridV', twc.grid_v.toFixed(1) + ' V');
  setText('twcSessionWh', (twc.session_energy_wh / 1000).toFixed(1) + ' kWh');
  setText('twcLifetimeWh', twc.lifetime_wh > 0 ? (twc.lifetime_wh / 1000).toFixed(1) + ' kWh' : '-- kWh');
  setText('twcPCBATemp', twc.pcba_temp_c.toFixed(1) + ' °C');
  setText('twcHandleTemp', twc.handle_temp_c.toFixed(1) + ' °C');

  // Status banner
  if (twc.contactor_closed) {
    banner.textContent = 'Charging';
    banner.setAttribute('data-state', 'charging');
  } else if (twc.vehicle_connected) {
    banner.textContent = 'Connected';
    banner.setAttribute('data-state', 'connected');
  } else {
    banner.textContent = 'Idle';
    banner.setAttribute('data-state', 'offline');
  }
}

function updateTWCGauge(amps, maxAmps) {
  const el = document.getElementById('twcArc');
  if (!el) return;

  if (!maxAmps || maxAmps <= 0 || !amps || amps <= 0) {
    el.setAttribute('d', '');
    return;
  }

  const cx = 110, cy = 115, r = 85;
  const frac = Math.max(0, Math.min(amps / maxAmps, 1));

  if (frac < 0.01) {
    el.setAttribute('d', '');
    return;
  }

  const startAngle = Math.PI;
  const endAngle = Math.PI * (1 - frac);
  const startX = cx + r * Math.cos(startAngle);
  const startY = cy - r * Math.sin(startAngle);
  const endX = cx + r * Math.cos(endAngle);
  const endY = cy - r * Math.sin(endAngle);

  el.setAttribute('d', `M ${startX} ${startY} A ${r} ${r} 0 0 1 ${endX} ${endY}`);
}

function updateTeslaGauge(amps, maxAmps) {
  const el = document.getElementById('teslaArc');
  if (!el) return;

  // Clear arc for invalid/zero values
  if (!maxAmps || maxAmps <= 0 || !amps || amps <= 0) {
    el.setAttribute('d', '');
    return;
  }

  const cx = 110, cy = 115, r = 85;
  const frac = Math.max(0, Math.min(amps / maxAmps, 1));

  if (frac < 0.01) {
    el.setAttribute('d', '');
    return;
  }

  const startAngle = Math.PI;
  const endAngle = Math.PI * (1 - frac);
  const startX = cx + r * Math.cos(startAngle);
  const startY = cy - r * Math.sin(startAngle);
  const endX = cx + r * Math.cos(endAngle);
  const endY = cy - r * Math.sin(endAngle);
  const largeArc = 0; // arc never exceeds 180° on a semicircular gauge

  el.setAttribute('d', `M ${startX} ${startY} A ${r} ${r} 0 ${largeArc} 1 ${endX} ${endY}`);
}

function setTeslaMode(mode) {
  // Optimistic UI update — highlight button immediately
  document.querySelectorAll('.tesla-mode-btn').forEach(btn => {
    btn.classList.toggle('active', btn.dataset.mode === mode);
  });
  fetch('/api/tesla/mode', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({mode: mode}),
  })
  .then(r => r.json())
  .then(status => updateTesla(status))
  .catch(err => console.error('Tesla mode error:', err));
}

function initTesla() {
  if (!CONFIG.teslaEnabled) return;
  initSocRefresh();

  document.querySelectorAll('.tesla-mode-btn').forEach(btn => {
    btn.addEventListener('click', () => { if (!btn.disabled) setTeslaMode(btn.dataset.mode); });
  });
}

// --- SSE Connection ---

function connectSSE() {
  const es = new EventSource('/api/events');

  es.onopen = () => {
    connected = true;
    updateStatus();
  };

  es.onmessage = (e) => {
    try {
      const payload = JSON.parse(e.data);
      updateDashboard(payload);
    } catch (err) {
      console.error('Parse error:', err);
    }
  };

  es.onerror = () => {
    connected = false;
    updateStatus();
    es.close();
    setTimeout(connectSSE, 3000);
  };
}

// --- Init ---

document.addEventListener('DOMContentLoaded', () => {
  initTheme();
  initTesla();
  connectSSE();
  initPairDialog();
  setInterval(updateStatus, 1000);
});

// --- Pairing dialog ---

function initPairDialog() {
  const open = document.getElementById('openPairModal');
  const backdrop = document.getElementById('pairBackdrop');
  const closeBtn = document.getElementById('pairClose');
  const cancelBtn = document.getElementById('pairCancel');
  const startBtn = document.getElementById('pairStart');
  const logEl = document.getElementById('pairLog');
  if (!open || !backdrop) return;

  let activeEventSource = null;

  function show() {
    backdrop.style.display = 'flex';
    logEl.style.display = 'none';
    logEl.innerHTML = '';
    startBtn.disabled = false;
    startBtn.textContent = 'Start pairing';
    cancelBtn.textContent = 'Cancel';
  }

  function hide() {
    if (activeEventSource) {
      activeEventSource.close();
      activeEventSource = null;
    }
    backdrop.style.display = 'none';
  }

  function logLine(cls, msg) {
    logEl.style.display = 'block';
    const line = document.createElement('div');
    if (cls) line.className = cls;
    line.textContent = msg;
    logEl.appendChild(line);
    logEl.scrollTop = logEl.scrollHeight;
  }

  open.addEventListener('click', e => { e.preventDefault(); show(); });
  closeBtn.addEventListener('click', hide);
  cancelBtn.addEventListener('click', hide);
  backdrop.addEventListener('click', e => { if (e.target === backdrop) hide(); });

  startBtn.addEventListener('click', async () => {
    startBtn.disabled = true;
    startBtn.textContent = 'Pairing in progress…';
    cancelBtn.textContent = 'Close';
    logEl.innerHTML = '';
    logLine(null, 'Starting…');

    const params = new URLSearchParams();
    params.set('rotate', 'true'); // always rotate — a fresh key avoids the
                                  // "previously-removed public key" rejection.
    params.set('window', '120');

    // EventSource only supports GET. We POST first (server handler requires POST),
    // then use fetch with a streaming response parser.
    try {
      const resp = await fetch('/api/tesla/pair?' + params.toString(), { method: 'POST' });
      if (!resp.ok || !resp.body) {
        logLine('err', 'HTTP ' + resp.status + ': ' + (await resp.text()).slice(0, 200));
        startBtn.disabled = false;
        startBtn.textContent = 'Start pairing';
        return;
      }

      const reader = resp.body.getReader();
      const decoder = new TextDecoder();
      let buf = '';
      let pendingEvent = 'message';
      while (true) {
        const { value, done } = await reader.read();
        if (done) break;
        buf += decoder.decode(value, { stream: true });
        let idx;
        while ((idx = buf.indexOf('\n\n')) >= 0) {
          const chunk = buf.slice(0, idx);
          buf = buf.slice(idx + 2);
          let evType = 'message';
          let evData = '';
          chunk.split('\n').forEach(line => {
            if (line.startsWith('event:')) evType = line.slice(6).trim();
            else if (line.startsWith('data:')) evData += (evData ? ' ' : '') + line.slice(5).trim();
          });
          if (evType === 'progress') {
            logLine(null, evData);
          } else if (evType === 'done') {
            logLine('ok', '✓ ' + evData);
            startBtn.textContent = 'Done';
          } else if (evType === 'error') {
            logLine('err', '✗ ' + evData);
            startBtn.disabled = false;
            startBtn.textContent = 'Retry';
          }
        }
      }
    } catch (err) {
      logLine('err', 'Network error: ' + err.message);
      startBtn.disabled = false;
      startBtn.textContent = 'Retry';
    }
  });
}
