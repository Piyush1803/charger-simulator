// Dashboard front-end for the charger simulator.
// Vanilla ES2020+. Loaded at end of <body>, so the DOM is already parsed.

(() => {
  'use strict';

  // -------------------------------------------------------------------------
  // 1) MODULE STATE
  // -------------------------------------------------------------------------
  const state = {
    connected: false,
    ws: null,
    lastSnap: null,
    lastCpState: null,
    durationTimer: null,
    sliderTimer: null,
    userScrolledUp: false,
    toastTimer: null,
    activeConnector: 1, // 1 or 2 — which connector tab is focused
  };

  // Live power chart state
  const powerSamples = [];
  const POWER_MAX_AGE_MS = 120 * 1000;
  let lastPowerAppendMs = 0;
  let powerCanvas = null;
  let powerCtx = null;
  let powerDpr = 1;
  let powerResizeTimer = null;

  // -------------------------------------------------------------------------
  // 2) DOM HELPERS
  // -------------------------------------------------------------------------
  const $ = (id) => document.getElementById(id);

  function showToast(msg, isError) {
    const t = $('toast');
    if (!t) return;
    t.textContent = msg;
    t.className = 'toast' + (isError ? ' toast-error' : '');
    t.classList.remove('hidden');
    if (state.toastTimer) {
      clearTimeout(state.toastTimer);
    }
    state.toastTimer = setTimeout(() => {
      t.classList.add('hidden');
      state.toastTimer = null;
    }, 3500);
  }

  // -------------------------------------------------------------------------
  // 3) API HELPER
  // -------------------------------------------------------------------------
  async function api(method, path, body) {
    const opts = { method, headers: {} };
    if (body !== undefined && body !== null) {
      opts.headers['Content-Type'] = 'application/json';
      opts.body = JSON.stringify(body);
    }
    const res = await fetch(path, opts);
    if (!res.ok) {
      let detail = '';
      try {
        const errJson = await res.json();
        detail = errJson && errJson.error ? errJson.error : '';
      } catch (_) {
        try { detail = await res.text(); } catch (_) { /* ignore */ }
      }
      throw new Error(`${method} ${path} → ${res.status}${detail ? ': ' + detail : ''}`);
    }
    if (res.status === 204) return null;
    const ct = res.headers.get('content-type') || '';
    if (ct.includes('application/json')) {
      return res.json();
    }
    return res.text();
  }

  // -------------------------------------------------------------------------
  // 12) LOG RENDERING HELPERS  (declared early so others can call them)
  // -------------------------------------------------------------------------
  function nowHMS() {
    return fmtHMS(new Date());
  }

  function fmtHMS(d) {
    const hh = String(d.getHours()).padStart(2, '0');
    const mm = String(d.getMinutes()).padStart(2, '0');
    const ss = String(d.getSeconds()).padStart(2, '0');
    return `${hh}:${mm}:${ss}`;
  }

  function truncate(s, n) {
    if (!s) return '';
    if (s.length > n) return s.slice(0, n) + '…';
    return s;
  }

  function appendRow(rowClasses, tagClass, tagText, ts, bodyText) {
    const stream = $('log-stream');
    if (!stream) return;
    const row = document.createElement('div');
    row.className = rowClasses.filter(Boolean).join(' ');

    const tsSpan = document.createElement('span');
    tsSpan.className = 'log-ts';
    tsSpan.textContent = ts;

    const tagSpan = document.createElement('span');
    tagSpan.className = 'log-tag' + (tagClass ? ' ' + tagClass : '');
    tagSpan.textContent = tagText;

    const bodySpan = document.createElement('span');
    bodySpan.className = 'log-body';
    bodySpan.textContent = bodyText;

    row.appendChild(tsSpan);
    row.appendChild(tagSpan);
    row.appendChild(bodySpan);
    stream.appendChild(row);

    while (stream.children.length > 250) {
      stream.removeChild(stream.firstChild);
    }
    if (!state.userScrolledUp) {
      stream.scrollTop = stream.scrollHeight;
    }
  }

  function appendBanner(text) {
    appendRow(['log-row', 'log-info'], '', 'INFO', nowHMS(), text);
  }

  function appendOcppRow(ev) {
    const d = ev.time ? new Date(ev.time) : new Date();
    const ts = fmtHMS(d);
    let tagClass = 'log-call';
    if (ev.ocpp_type === 'CALLRESULT') tagClass = 'log-callresult';
    else if (ev.ocpp_type === 'CALLERROR') tagClass = 'log-callerror';
    const tagText = ev.ocpp_type || 'CALL';
    const dirClass = ev.dir === 'out' ? 'log-out' : 'log-in';
    let body;
    if (ev.ocpp_type === 'CALLERROR') {
      body = `${ev.action || ''} ${ev.error_code || ''}: ${ev.error_desc || ''}`;
    } else {
      body = `${ev.action || ''} ${truncate(ev.payload || '', 240)}`;
    }
    appendRow(['log-row', dirClass, tagClass], tagClass, tagText, ts, body);
  }

  function appendLogRow(ev) {
    const d = ev.time ? new Date(ev.time) : new Date();
    const ts = fmtHMS(d);
    const level = ev.level || 'info';
    const levelClass = 'log-' + level;
    const body = (ev.msg || '') + (ev.fields ? ' ' + JSON.stringify(ev.fields) : '');
    appendRow(['log-row', levelClass], levelClass, 'LOG', ts, body);
  }

  // -------------------------------------------------------------------------
  // 7b) Active-connector helpers
  // -------------------------------------------------------------------------
  // Pick the connector to "focus" based on state.activeConnector, with sane
  // fallback to the first connector or a synthesized one from the back-compat
  // top-level fields.
  function pickActiveConnector(s) {
    if (!s) return null;
    const arr = Array.isArray(s.connectors) ? s.connectors : null;
    if (arr && arr.length > 0) {
      const found = arr.find((c) => c && c.id === state.activeConnector);
      if (found) return found;
      return arr[0];
    }
    // Back-compat: synthesize from top-level mirror fields.
    return {
      id: 1,
      status: s.connector_status || 'Available',
      error_code: s.connector_error_code || '',
      tx_id: s.tx_id || 0,
      id_tag: s.id_tag || '',
      energy_wh: s.energy_wh || 0,
      power_w: s.power_w || 0,
      voltage_v: s.voltage_v || 0,
      current_a: s.current_a || 0,
      soc: s.soc || 0,
      session_start_unix: s.session_start_unix || 0,
      session_energy_wh: s.session_energy_wh || 0,
      max_power_w_configured: s.max_power_w_configured || 0,
      max_power_w_current: s.max_power_w_current || 0,
    };
  }

  function activateConnector(n) {
    if (n !== 1 && n !== 2) return;
    state.activeConnector = n;
    if (state.lastSnap) {
      renderState(state.lastSnap);
    }
  }

  // -------------------------------------------------------------------------
  // 8) renderState
  // -------------------------------------------------------------------------
  function setText(id, val) {
    const el = $(id);
    if (el) el.textContent = val;
  }

  // Which meter tiles each fault mode strips from the MeterValues the CMS sees.
  // Drives the dashboard's "what the CMS is NOT getting" dimming.
  const METER_FAULT_DROPPED = {
    none: [],
    dead_silent: ['energy', 'power', 'voltage', 'current', 'soc'],
    dead_empty: ['energy', 'power', 'voltage', 'current', 'soc'],
    dead_zero: ['power', 'voltage', 'current'],
    no_current: ['current'],
    no_vip: ['power', 'voltage', 'current'],
  };

  function tileFor(key) {
    if (key === 'soc') return $('soc-tile');
    const val = $(key + '-val');
    return val ? val.closest('.meter-card') : null;
  }

  // Reflect the active connector's meter fault: sync the <select>, show the
  // badge, and dim the tiles whose measurand is no longer on the wire.
  function reflectMeterFault(activeConn) {
    const mode = (activeConn && activeConn.meter_fault) || 'none';

    const sel = $('meter-fault-select');
    if (sel && document.activeElement !== sel) sel.value = mode;

    const badge = $('meter-fault-badge');
    if (badge) badge.classList.toggle('hidden', mode === 'none');

    const dropped = METER_FAULT_DROPPED[mode] || [];
    ['energy', 'power', 'voltage', 'current', 'soc'].forEach((k) => {
      const tile = tileFor(k);
      if (tile) tile.classList.toggle('meter-card-dropped', dropped.includes(k));
    });
  }

  function renderState(s) {
    if (!s) return;

    // Top-bar identity comes from the charger itself, not per-connector.
    setText('cp-id-val', s.cp_id || '—');
    setText('cms-url-val', s.cms_url || '—');
    setText('vendor-val', s.vendor || '—');
    setText('model-val', s.model || '—');

    // Charger-type badge: visible only for DC.
    const badge = $('charger-type-badge');
    if (badge) {
      badge.textContent = s.charger_type || 'AC';
      badge.classList.toggle('hidden', s.charger_type !== 'DC');
    }

    // CP-state pill comes from the charger (lifecycle, not connector).
    const connPill = $('conn-pill');
    if (connPill) {
      const cps = s.cp_state || 'Disconnected';
      connPill.textContent = cps;
      connPill.className = 'pill pill-cp-' + cps.toLowerCase();
    }

    // Connector tabs: show/hide based on num_connectors, and reflect each
    // connector's status on its own tab.
    const tabsHost = $('connector-tabs');
    if (tabsHost) {
      tabsHost.classList.toggle('hidden', !(s.num_connectors && s.num_connectors > 1));
    }
    const connectorsArr = Array.isArray(s.connectors) ? s.connectors : [];
    for (let i = 1; i <= 2; i++) {
      const tab = $('connector-tab-' + i);
      if (!tab) continue;
      const conn = connectorsArr.find((c) => c && c.id === i);
      const cs = conn ? (conn.status || 'Available') : 'Available';
      tab.dataset.status = cs;
      const statusSpan = tab.querySelector('.conn-tab-status');
      if (statusSpan) statusSpan.textContent = cs;
      tab.classList.toggle('connector-tab-active', i === state.activeConnector);
    }

    // The rest of the UI shows the ACTIVE connector.
    const activeConn = pickActiveConnector(s);
    const cnStatus = (activeConn && activeConn.status) || 'Available';

    const connectorPill = $('connector-pill');
    if (connectorPill) {
      connectorPill.textContent = cnStatus;
      connectorPill.className = 'pill pill-conn-' + cnStatus;
    }

    // SVG charger illustration state — reflects the active connector.
    const art = $('charger-art');
    if (art) {
      art.classList.remove('is-available', 'is-preparing', 'is-charging', 'is-finishing', 'is-faulted');
      let cls = 'is-available';
      let screenText = 'READY';
      switch (cnStatus) {
        case 'Available':
          cls = 'is-available'; screenText = 'READY'; break;
        case 'Preparing':
          cls = 'is-preparing'; screenText = 'PREPARE'; break;
        case 'Charging':
          cls = 'is-charging'; screenText = 'CHARGING'; break;
        case 'Finishing':
          cls = 'is-finishing'; screenText = 'DONE'; break;
        case 'Faulted':
          cls = 'is-faulted'; screenText = 'FAULT'; break;
        default:
          cls = 'is-available'; screenText = cnStatus.toUpperCase(); break;
      }
      art.classList.add(cls);
      const screen = art.querySelector('.art-charger-screen-text');
      if (screen) screen.textContent = screenText;
    }

    // Per-connector telemetry tiles.
    setText('tx-id-val', (activeConn && activeConn.tx_id) || '—');
    setText('id-tag-val', (activeConn && activeConn.id_tag) || '—');
    setText('energy-val', (activeConn && activeConn.energy_wh) || 0);
    setText('power-val', Math.round((activeConn && activeConn.power_w) || 0));
    setText('voltage-val', ((activeConn && activeConn.voltage_v) || 0).toFixed(1));
    setText('current-val', ((activeConn && activeConn.current_a) || 0).toFixed(2));
    setText('max-power-current-val', (((activeConn && activeConn.max_power_w_current) || 0) / 1000).toFixed(1) + ' kW');
    setText('max-power-configured-val', (((activeConn && activeConn.max_power_w_configured) || 0) / 1000).toFixed(1) + ' kW');

    // SoC tile: visible only for DC; value taken from the active connector.
    const socTile = $('soc-tile');
    if (socTile) {
      socTile.classList.toggle('hidden', s.charger_type !== 'DC');
    }
    setText('soc-val', Math.round(((activeConn && activeConn.soc) || 0) * 10) / 10);

    // Energy-meter fault: sync the selector + badge + dim the dropped tiles.
    reflectMeterFault(activeConn);

    // Power slider tracks the active connector's per-connector ceilings.
    const slider = $('power-slider');
    if (slider) {
      const prevActive = state.lastSnap ? pickActiveConnector(state.lastSnap) : null;
      const prevCfg = prevActive ? prevActive.max_power_w_configured : null;
      const cfg = (activeConn && activeConn.max_power_w_configured) || 0;
      if (prevCfg !== cfg) {
        slider.min = 1;
        slider.max = Math.max(1, Math.round(cfg / 1000));
        slider.step = 1;
      }
      const desired = Math.round(((activeConn && activeConn.max_power_w_current) || 0) / 1000);
      if (document.activeElement !== slider) {
        slider.value = desired;
      }
    }

    state.lastSnap = s;
    updateDuration();
    updateButtonStates(s);
    appendPowerSample(s);
  }

  // -------------------------------------------------------------------------
  // 8b) POWER CHART
  // -------------------------------------------------------------------------
  function appendPowerSample(s) {
    const now = Date.now();
    const activeConn = pickActiveConnector(s);
    const watts = Number(activeConn && activeConn.power_w) || 0;

    // Drop entries older than the window
    const cutoff = now - POWER_MAX_AGE_MS;
    while (powerSamples.length > 0 && powerSamples[0].ts < cutoff) {
      powerSamples.shift();
    }

    if (now - lastPowerAppendMs >= 1500 || powerSamples.length === 0) {
      powerSamples.push({ ts: now, watts });
      lastPowerAppendMs = now;
    } else {
      // Track current value in the most recent sample
      powerSamples[powerSamples.length - 1].watts = watts;
    }

    const nowEl = document.getElementById('power-chart-now-val');
    if (nowEl) nowEl.textContent = Math.round(watts);

    const empty = document.getElementById('power-chart-empty');
    if (empty) empty.classList.toggle('hidden', powerSamples.length >= 2);

    drawPowerChart(s);
  }

  function sizePowerCanvas() {
    if (!powerCanvas) return;
    const rect = powerCanvas.getBoundingClientRect();
    const w = Math.max(1, Math.floor(rect.width));
    const h = Math.max(1, Math.floor(rect.height));
    const dpr = window.devicePixelRatio || 1;
    powerDpr = dpr;
    // Only resize when needed to avoid resetting transform every draw
    if (powerCanvas.width !== w * dpr || powerCanvas.height !== h * dpr) {
      powerCanvas.width = w * dpr;
      powerCanvas.height = h * dpr;
      powerCtx.setTransform(1, 0, 0, 1, 0, 0);
      powerCtx.scale(dpr, dpr);
    }
  }

  function drawPowerChart(s) {
    if (!powerCanvas || !powerCtx) return;
    sizePowerCanvas();

    const rect = powerCanvas.getBoundingClientRect();
    const W = rect.width;
    const H = rect.height;
    const ctx = powerCtx;

    ctx.clearRect(0, 0, W, H);

    // Compute yMax — use the active connector's configured ceiling.
    const snap = s || state.lastSnap;
    const activeConn = pickActiveConnector(snap);
    const cfgMax = (activeConn && activeConn.max_power_w_configured) || 0;
    let observedPeak = 0;
    for (let i = 0; i < powerSamples.length; i++) {
      if (powerSamples[i].watts > observedPeak) observedPeak = powerSamples[i].watts;
    }
    const yMaxRaw = Math.max(cfgMax, observedPeak, 1);
    const yMax = yMaxRaw * 1.05;

    const topPad = 24;
    const bottomPad = 4;
    const leftPad = 0;
    const rightPad = 0;
    const plotW = Math.max(1, W - leftPad - rightPad);
    const plotH = Math.max(1, H - topPad - bottomPad);

    // Grid lines at 25/50/75/100% of yMax
    ctx.strokeStyle = 'rgba(255,255,255,0.05)';
    ctx.lineWidth = 1;
    for (let i = 1; i <= 4; i++) {
      const frac = i / 4;
      const y = topPad + plotH * (1 - frac);
      ctx.beginPath();
      ctx.moveTo(leftPad, y + 0.5);
      ctx.lineTo(leftPad + plotW, y + 0.5);
      ctx.stroke();
    }

    if (powerSamples.length < 2) return;

    const now = Date.now();
    const xMin = now - POWER_MAX_AGE_MS;
    const xMax = now;
    const xSpan = Math.max(1, xMax - xMin);

    const tsToX = (ts) => leftPad + ((ts - xMin) / xSpan) * plotW;
    const wToY = (w) => topPad + plotH * (1 - (w / yMax));

    // Area fill under the polyline
    const areaGrad = ctx.createLinearGradient(0, topPad, 0, topPad + plotH);
    areaGrad.addColorStop(0, 'rgba(63,185,80,0.18)');
    areaGrad.addColorStop(1, 'rgba(63,185,80,0)');
    ctx.fillStyle = areaGrad;
    ctx.beginPath();
    const first = powerSamples[0];
    ctx.moveTo(tsToX(first.ts), wToY(first.watts));
    for (let i = 1; i < powerSamples.length; i++) {
      const p = powerSamples[i];
      ctx.lineTo(tsToX(p.ts), wToY(p.watts));
    }
    const last = powerSamples[powerSamples.length - 1];
    ctx.lineTo(tsToX(last.ts), topPad + plotH);
    ctx.lineTo(tsToX(first.ts), topPad + plotH);
    ctx.closePath();
    ctx.fill();

    // Polyline stroke (left→right gradient)
    const lineGrad = ctx.createLinearGradient(leftPad, 0, leftPad + plotW, 0);
    lineGrad.addColorStop(0, 'rgba(47,129,247,1)');
    lineGrad.addColorStop(1, 'rgba(63,185,80,1)');
    ctx.strokeStyle = lineGrad;
    ctx.lineWidth = 2;
    ctx.lineCap = 'round';
    ctx.lineJoin = 'round';
    ctx.beginPath();
    ctx.moveTo(tsToX(first.ts), wToY(first.watts));
    for (let i = 1; i < powerSamples.length; i++) {
      const p = powerSamples[i];
      ctx.lineTo(tsToX(p.ts), wToY(p.watts));
    }
    ctx.stroke();
  }

  // -------------------------------------------------------------------------
  // 9) updateButtonStates
  // -------------------------------------------------------------------------
  function updateButtonStates(s) {
    const on = !!(s && s.connected);
    const activeConn = pickActiveConnector(s);
    const cn = (activeConn && activeConn.status) || '';

    const setDisabled = (id, disabled) => {
      const el = $(id);
      if (el) el.disabled = !!disabled;
    };

    // Plug-In/Plug-Out toggle button — JS owns label, classes, and disabled state
    const plug = $('btn-plug-in');
    if (plug) {
      plug.classList.remove('btn-primary', 'btn-warning', 'btn-toggle', 'btn-toggle-out');
      if (cn === 'Preparing') {
        plug.textContent = 'Plug Out Gun';
        plug.classList.add('btn-warning', 'btn-toggle-out');
        plug.disabled = !on;
      } else {
        plug.textContent = 'Plug In Gun';
        plug.classList.add('btn-primary', 'btn-toggle');
        plug.disabled = !(on && cn === 'Available');
      }
    }

    setDisabled('btn-start', !(on && (cn === 'Preparing' || cn === 'Available')));
    setDisabled('btn-stop', !(on && cn === 'Charging'));
    setDisabled('btn-emergency', !(on && cn !== 'Faulted'));

    setDisabled('btn-clear-fault', !(on && cn === 'Faulted'));
    const clearFaultBtn = $('btn-clear-fault');
    if (clearFaultBtn) {
      if (cn === 'Faulted') clearFaultBtn.classList.remove('hidden');
      else clearFaultBtn.classList.add('hidden');
    }

    setDisabled('btn-load-down', !on);
    setDisabled('btn-load-up', !on);
    const slider = $('power-slider');
    if (slider) slider.disabled = !on;

    setDisabled('meter-fault-select', !on);

    // Offline/online toggle. Unlike the other controls it must stay enabled
    // while offline (connected === false) so the charger can be revived.
    const offlineBtn = $('btn-offline');
    if (offlineBtn) {
      const isOffline = !!(s && s.cp_state === 'Offline');
      offlineBtn.classList.remove('btn-neutral', 'btn-success');
      if (isOffline) {
        offlineBtn.textContent = 'Go Online';
        offlineBtn.classList.add('btn-success');
      } else {
        offlineBtn.textContent = 'Go Offline';
        offlineBtn.classList.add('btn-neutral');
      }
      offlineBtn.disabled = false;
    }
  }

  // -------------------------------------------------------------------------
  // 10) updateDuration
  // -------------------------------------------------------------------------
  function updateDuration() {
    const el = $('duration-val');
    if (!el) return;
    const s = state.lastSnap;
    const activeConn = pickActiveConnector(s);
    if (!activeConn || !activeConn.session_start_unix) {
      el.textContent = '—';
      return;
    }
    const nowSec = Math.floor(Date.now() / 1000);
    const secs = Math.max(0, nowSec - activeConn.session_start_unix);
    const h = Math.floor(secs / 3600);
    const m = Math.floor((secs % 3600) / 60);
    const sec = secs % 60;
    const pad = (n) => String(n).padStart(2, '0');
    el.textContent = h >= 1
      ? `${pad(h)}:${pad(m)}:${pad(sec)}`
      : `${pad(m)}:${pad(sec)}`;
  }

  // -------------------------------------------------------------------------
  // 7) handleEvent
  // -------------------------------------------------------------------------
  function handleEvent(ev) {
    if (!ev || !ev.kind) return;
    switch (ev.kind) {
      case 'state':
        if (ev.state) {
          renderState(ev.state);
          if (ev.state.cp_state !== state.lastCpState) {
            if (state.lastCpState !== null) {
              appendBanner(ev.state.cp_state);
            }
            state.lastCpState = ev.state.cp_state;
          }
        }
        break;
      case 'ocpp':
        appendOcppRow(ev);
        break;
      case 'log':
        appendLogRow(ev);
        break;
    }
  }

  // -------------------------------------------------------------------------
  // 6) WS
  // -------------------------------------------------------------------------
  function connectWS() {
    if (state.ws) {
      try { state.ws.close(); } catch (_) { /* ignore */ }
      state.ws = null;
    }
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    const url = `${proto}//${location.host}/api/events`;
    let ws;
    try {
      ws = new WebSocket(url);
    } catch (e) {
      console.error('ws construct failed', e);
      return;
    }
    state.ws = ws;
    const self = ws;

    ws.onopen = () => {
      appendBanner('live stream connected');
    };
    ws.onmessage = (e) => {
      try {
        const ev = JSON.parse(e.data);
        handleEvent(ev);
      } catch (err) {
        console.error('ws parse failed', err);
      }
    };
    ws.onclose = () => {
      if (state.ws !== self) return;
      state.ws = null;
      const pill = $('conn-pill');
      if (pill) {
        pill.textContent = 'Disconnected';
        pill.className = 'pill pill-cp-disconnected';
      }
      const statusSection = $('status-section');
      if (statusSection && !statusSection.classList.contains('hidden')) {
        setTimeout(connectWS, 2000);
      }
    };
    ws.onerror = (e) => {
      if (state.ws !== self) return;
      console.error('ws error', e);
    };
  }

  // -------------------------------------------------------------------------
  // 13) fetchAndRender
  // -------------------------------------------------------------------------
  async function fetchAndRender() {
    try {
      const cfg = await api('GET', '/api/config');
      if (cfg) fillConfigForm(cfg);
    } catch (_) { /* ignore */ }
    try {
      const snap = await api('GET', '/api/state');
      if (snap) renderState(snap);
    } catch (_) { /* ignore */ }
  }

  // -------------------------------------------------------------------------
  // 14) fillConfigForm
  // -------------------------------------------------------------------------
  function fillConfigForm(cfg) {
    const form = $('config-form');
    if (!form || !cfg) return;
    const set = (name, val) => {
      const el = form.elements[name];
      if (el != null && val !== undefined && val !== null) el.value = val;
    };
    let base = cfg.CMSURL || '';
    if (cfg.CPID && base.endsWith('/' + cfg.CPID)) {
      base = base.slice(0, base.length - ('/' + cfg.CPID).length);
    }
    set('cms_url', base);
    set('cp_id', cfg.CPID);
    set('user', cfg.BasicAuthUser);
    // pass intentionally left empty
    set('vendor', cfg.Vendor);
    set('model', cfg.Model);
    if (typeof cfg.MaxPowerW === 'number') set('max_kw', cfg.MaxPowerW / 1000);
    if (typeof cfg.Phases === 'number') set('phases', cfg.Phases);
    if (typeof cfg.NominalVoltageV === 'number') set('voltage', cfg.NominalVoltageV);
    if (typeof cfg.HeartbeatInterval === 'number') set('heartbeat_s', cfg.HeartbeatInterval / 1e9);
    if (typeof cfg.MeterInterval === 'number') set('meter_s', cfg.MeterInterval / 1e9);

    // Slice-4 additions: charger type, connector count, DC battery params.
    const typeVal = cfg.Type || 'AC';
    set('charger_type', typeVal);
    set('num_connectors', String(cfg.NumConnectors || 1));
    if (typeof cfg.BatteryCapacityKWh === 'number') set('battery_capacity_kwh', cfg.BatteryCapacityKWh);
    if (typeof cfg.InitialSoC === 'number') set('initial_soc', cfg.InitialSoC);

    // Re-evaluate AC/DC visibility based on the freshly-populated type.
    reflectChargerTypeOnForm();
  }

  // -------------------------------------------------------------------------
  // AC/DC form visibility toggle
  // -------------------------------------------------------------------------
  function reflectChargerTypeOnForm() {
    const form = $('config-form');
    if (!form) return;
    const typeSel = form.elements['charger_type'];
    const isDC = typeSel && typeSel.value === 'DC';
    document.querySelectorAll('.dc-only').forEach((el) => el.classList.toggle('hidden', !isDC));
    document.querySelectorAll('.ac-only').forEach((el) => el.classList.toggle('hidden', isDC));
  }

  // -------------------------------------------------------------------------
  // 5) CONFIGURE submit
  // -------------------------------------------------------------------------
  async function onConfigSubmit(e) {
    e.preventDefault();
    const form = $('config-form');
    if (!form) return;
    const fd = new FormData(form);

    const chargerType = String(fd.get('charger_type') || 'AC');
    const numConnectors = parseInt(fd.get('num_connectors'), 10) || 1;

    const body = {
      cms_url: String(fd.get('cms_url') || ''),
      cp_id: String(fd.get('cp_id') || ''),
      user: String(fd.get('user') || ''),
      pass: String(fd.get('pass') || ''),
      vendor: String(fd.get('vendor') || ''),
      model: String(fd.get('model') || ''),
      max_kw: parseFloat(fd.get('max_kw')),
      voltage: parseFloat(fd.get('voltage')),
      heartbeat_s: parseInt(fd.get('heartbeat_s'), 10),
      meter_s: parseInt(fd.get('meter_s'), 10),
      charger_type: chargerType,
      num_connectors: numConnectors,
    };

    if (chargerType === 'DC') {
      body.battery_capacity_kwh = parseFloat(fd.get('battery_capacity_kwh'));
      body.initial_soc = parseFloat(fd.get('initial_soc'));
    } else {
      body.phases = parseInt(fd.get('phases'), 10);
    }

    const errEl = $('config-error');
    try {
      await api('POST', '/api/configure', body);
      if (errEl) {
        errEl.textContent = '';
        errEl.classList.add('hidden');
      }
      // After a fresh configure, always start focused on connector 1.
      state.activeConnector = 1;
      const cfgSec = $('config-section');
      const statSec = $('status-section');
      const logSec = $('log-section');
      if (cfgSec) cfgSec.classList.add('hidden');
      if (statSec) statSec.classList.remove('hidden');
      if (logSec) logSec.classList.remove('hidden');
      await fetchAndRender();
      connectWS();
    } catch (err) {
      if (errEl) {
        errEl.textContent = err.message;
        errEl.classList.remove('hidden');
      }
    }
  }

  // -------------------------------------------------------------------------
  // 11) BUTTON WIRING
  // -------------------------------------------------------------------------
  function wireButtons() {
    const click = (id, fn) => {
      const el = $(id);
      if (el) el.addEventListener('click', fn);
    };

    click('btn-plug-in', () => {
      const activeConn = pickActiveConnector(state.lastSnap);
      const cn = activeConn && activeConn.status;
      const url = (cn === 'Preparing') ? '/api/unplug' : '/api/plug-in';
      api('POST', url, { connector_id: state.activeConnector })
        .catch((e) => showToast(e.message, true));
    });

    click('btn-start', () => {
      const tagInput = $('start-id-tag');
      const idTag = (tagInput && tagInput.value) ? tagInput.value : 'TESTRFID0001';
      api('POST', '/api/start-charging', { connector_id: state.activeConnector, id_tag: idTag })
        .catch((e) => showToast(e.message, true));
    });

    click('btn-stop', () => {
      api('POST', '/api/stop-charging', { connector_id: state.activeConnector })
        .catch((e) => showToast(e.message, true));
    });

    click('btn-emergency', () => {
      if (!confirm('Trigger emergency stop?')) return;
      api('POST', '/api/emergency-stop', { connector_id: state.activeConnector })
        .catch((e) => showToast(e.message, true));
    });

    click('btn-clear-fault', () => {
      api('POST', '/api/clear-fault', { connector_id: state.activeConnector })
        .catch((e) => showToast(e.message, true));
    });

    click('btn-load-down', () => {
      const activeConn = pickActiveConnector(state.lastSnap);
      const cur = (activeConn && activeConn.max_power_w_current) || 0;
      const watts = Math.max(1000, cur - 1000);
      api('POST', '/api/set-power', { watts })
        .catch((e) => showToast(e.message, true));
    });

    click('btn-load-up', () => {
      const activeConn = pickActiveConnector(state.lastSnap);
      const cur = (activeConn && activeConn.max_power_w_current) || 0;
      const cfg = (activeConn && activeConn.max_power_w_configured) || cur;
      const watts = Math.min(cfg, cur + 1000);
      api('POST', '/api/set-power', { watts })
        .catch((e) => showToast(e.message, true));
    });

    const slider = $('power-slider');
    if (slider) {
      slider.addEventListener('input', () => {
        if (state.sliderTimer) clearTimeout(state.sliderTimer);
        state.sliderTimer = setTimeout(() => {
          const watts = Number(slider.value) * 1000;
          api('POST', '/api/set-power', { watts })
            .catch((e) => showToast(e.message, true));
        }, 150);
      });
    }

    const meterFaultSel = $('meter-fault-select');
    if (meterFaultSel) {
      meterFaultSel.addEventListener('change', () => {
        api('POST', '/api/meter-fault', {
          connector_id: state.activeConnector,
          mode: meterFaultSel.value,
        }).catch((e) => showToast(e.message, true));
      });
    }

    click('btn-offline', () => {
      const isOffline = state.lastSnap && state.lastSnap.cp_state === 'Offline';
      const url = isOffline ? '/api/go-online' : '/api/go-offline';
      api('POST', url).catch((e) => showToast(e.message, true));
    });

    click('btn-disconnect', () => {
      api('POST', '/api/disconnect')
        .then(() => {
          const cfgSec = $('config-section');
          const statSec = $('status-section');
          const logSec = $('log-section');
          if (statSec) statSec.classList.add('hidden');
          if (logSec) logSec.classList.add('hidden');
          if (cfgSec) cfgSec.classList.remove('hidden');
          if (state.ws) {
            try { state.ws.close(); } catch (_) { /* ignore */ }
            state.ws = null;
          }
          // Reset live power chart
          powerSamples.length = 0;
          lastPowerAppendMs = 0;
          drawPowerChart(null);
          const emptyEl = document.getElementById('power-chart-empty');
          if (emptyEl) emptyEl.classList.remove('hidden');
          const nowEl = document.getElementById('power-chart-now-val');
          if (nowEl) nowEl.textContent = '0';
        })
        .catch((e) => showToast(e.message, true));
    });

    click('btn-clear-log', () => {
      const stream = $('log-stream');
      if (stream) stream.innerHTML = '';
    });

    // Connector tabs — clicking just changes which connector the UI focuses.
    // They never issue API calls themselves.
    click('connector-tab-1', () => activateConnector(1));
    click('connector-tab-2', () => activateConnector(2));
  }

  // -------------------------------------------------------------------------
  // log-stream scroll tracking
  // -------------------------------------------------------------------------
  function wireScroll() {
    const stream = $('log-stream');
    if (!stream) return;
    stream.addEventListener('scroll', () => {
      state.userScrolledUp = (stream.scrollTop + stream.clientHeight) < (stream.scrollHeight - 40);
    });
  }

  // -------------------------------------------------------------------------
  // 4) INIT
  // -------------------------------------------------------------------------
  async function init() {
    const form = $('config-form');
    if (form) {
      form.addEventListener('submit', onConfigSubmit);
      // Live AC/DC visibility toggle on the form.
      const typeSel = form.elements['charger_type'];
      if (typeSel) {
        typeSel.addEventListener('change', reflectChargerTypeOnForm);
      }
      reflectChargerTypeOnForm(); // honour defaults before user input
    }
    wireButtons();
    wireScroll();

    // Power chart: capture canvas + ctx and set up debounced resize
    powerCanvas = document.getElementById('power-chart');
    if (powerCanvas) {
      powerCtx = powerCanvas.getContext('2d');
      sizePowerCanvas();
      window.addEventListener('resize', () => {
        if (powerResizeTimer) clearTimeout(powerResizeTimer);
        powerResizeTimer = setTimeout(() => {
          sizePowerCanvas();
          drawPowerChart(state.lastSnap);
        }, 150);
      });
    }

    state.durationTimer = setInterval(updateDuration, 1000);

    try {
      const snap = await api('GET', '/api/state');
      if (snap) {
        const cfgSec = $('config-section');
        const statSec = $('status-section');
        const logSec = $('log-section');
        if (cfgSec) cfgSec.classList.add('hidden');
        if (statSec) statSec.classList.remove('hidden');
        if (logSec) logSec.classList.remove('hidden');
        renderState(snap);
        try {
          const cfg = await api('GET', '/api/config');
          if (cfg) fillConfigForm(cfg);
        } catch (_) { /* ignore */ }
        connectWS();
      }
    } catch (err) {
      // 404 or other → leave config-section visible, no WS
    }
  }

  // -------------------------------------------------------------------------
  // 15) unload cleanup
  // -------------------------------------------------------------------------
  window.addEventListener('beforeunload', () => {
    try { state.ws && state.ws.close(); } catch (_) { /* ignore */ }
  });

  // Kick things off — DOM is ready (script at end of body).
  init();
})();
