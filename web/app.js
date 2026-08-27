/* pidash frontend.
   No framework, no bundler: the whole thing is served out of the Go binary. */
(() => {
  'use strict';

  const HISTORY = 180;              // samples kept per series (3 min at 1s)
  const $ = (sel) => document.querySelector(sel);

  const C = {
    cyan: '#2ee6ff', violet: '#a78bfa', magenta: '#f472b6',
    lime: '#7ff08a', amber: '#ffc75a', rose: '#ff6b81',
    grid: 'rgba(148,176,220,.09)', axis: 'rgba(148,176,220,.45)',
  };

  /* ---------------- formatting ---------------- */

  const pad = (n) => String(n).padStart(2, '0');

  function bytes(n, digits = 1) {
    if (!n && n !== 0) return '—';
    const u = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];
    let i = 0;
    while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
    return `${n.toFixed(i === 0 ? 0 : digits)} ${u[i]}`;
  }

  function bitrate(bytesPerSec) {
    const bits = bytesPerSec * 8;
    const u = ['bps', 'Kbps', 'Mbps', 'Gbps'];
    let i = 0, n = bits;
    while (n >= 1000 && i < u.length - 1) { n /= 1000; i++; }
    return `${n.toFixed(n < 10 && i > 0 ? 1 : 0)} ${u[i]}`;
  }

  function duration(secs) {
    if (secs == null) return '—';
    const d = Math.floor(secs / 86400);
    const h = Math.floor((secs % 86400) / 3600);
    const m = Math.floor((secs % 3600) / 60);
    if (d > 0) return `${d}d ${h}h`;
    if (h > 0) return `${h}h ${m}m`;
    return `${m}m ${Math.floor(secs % 60)}s`;
  }

  const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) =>
    ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

  /* ---------------- ring buffer ---------------- */

  class Series {
    constructor(cap = HISTORY) { this.cap = cap; this.data = []; }
    push(v) {
      this.data.push(Number.isFinite(v) ? v : 0);
      if (this.data.length > this.cap) this.data.shift();
    }
    get last() { return this.data.length ? this.data[this.data.length - 1] : 0; }
    max() { return this.data.length ? Math.max(...this.data) : 0; }
  }

  const hist = {
    cpu: new Series(), cpuUser: new Series(), cpuSys: new Series(), cpuWait: new Series(),
    temp: new Series(), mem: new Series(), rx: new Series(), tx: new Series(),
  };

  /* ---------------- canvas plumbing ---------------- */

  // Canvases are laid out by CSS; this keeps the backing store matched to the
  // element's real pixel size so nothing renders blurry on a retina display.
  function fitCanvas(canvas) {
    const dpr = window.devicePixelRatio || 1;
    const r = canvas.getBoundingClientRect();
    const w = Math.max(1, Math.round(r.width * dpr));
    const h = Math.max(1, Math.round(r.height * dpr));
    if (canvas.width !== w || canvas.height !== h) { canvas.width = w; canvas.height = h; }
    const ctx = canvas.getContext('2d');
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, r.width, r.height);
    return { ctx, w: r.width, h: r.height };
  }

  // Points are drawn right-aligned across the full capacity, so a fresh page
  // fills in from the right edge instead of stretching two samples across it.
  function pointsFor(data, cap, w, h, lo, hi, padTop, padBottom) {
    const span = Math.max(1e-9, hi - lo);
    const usable = h - padTop - padBottom;
    const step = w / Math.max(1, cap - 1);
    const start = w - (data.length - 1) * step;
    return data.map((v, i) => [
      start + i * step,
      padTop + usable - ((Math.min(hi, Math.max(lo, v)) - lo) / span) * usable,
    ]);
  }

  // A gentle midpoint-quadratic smoothing. Cheap, and keeps the trace legible
  // at one sample per second without inventing peaks the data does not have.
  function tracePath(ctx, pts) {
    if (!pts.length) return;
    ctx.moveTo(pts[0][0], pts[0][1]);
    for (let i = 1; i < pts.length; i++) {
      const [x0, y0] = pts[i - 1], [x1, y1] = pts[i];
      ctx.quadraticCurveTo(x0, y0, (x0 + x1) / 2, (y0 + y1) / 2);
    }
    ctx.lineTo(pts[pts.length - 1][0], pts[pts.length - 1][1]);
  }

  // Charts are registered so a ResizeObserver can repaint them whenever their
  // box changes — which is what makes a resized card redraw its chart crisply
  // instead of stretching a stale bitmap.
  const charts = new Map();
  const chartRO = new ResizeObserver((entries) => {
    for (const e of entries) {
      const def = charts.get(e.target);
      if (def) def.draw();
    }
  });

  function plot(canvas, series, opts) {
    if (!charts.has(canvas)) chartRO.observe(canvas);
    charts.set(canvas, { draw: () => drawChart(canvas, series, opts) });
    drawChart(canvas, series, opts);
  }

  function plotSpark(canvas, series, color, max) {
    if (!charts.has(canvas)) chartRO.observe(canvas);
    charts.set(canvas, { draw: () => drawSpark(canvas, series, color, max) });
    drawSpark(canvas, series, color, max);
  }

  function drawChart(canvas, series, opts = {}) {
    const { ctx, w, h } = fitCanvas(canvas);
    if (!w || !h) return;
    const padTop = 10, padBottom = 16, padRight = opts.labels === false ? 0 : 44;
    const plotW = w - padRight;

    let hi = opts.max;
    if (hi === undefined) {
      hi = Math.max(...series.map((s) => s.series.max()), opts.floor ?? 1);
      hi = hi * 1.25;
    }
    const lo = opts.min ?? 0;

    // horizontal grid + right-hand scale
    ctx.save();
    ctx.strokeStyle = C.grid;
    ctx.lineWidth = 1;
    ctx.font = '10px ui-monospace, monospace';
    ctx.fillStyle = 'rgba(148,176,220,.5)';
    ctx.textBaseline = 'middle';
    const rows = 4;
    for (let i = 0; i <= rows; i++) {
      const y = padTop + (h - padTop - padBottom) * (i / rows);
      ctx.beginPath();
      ctx.moveTo(0, Math.round(y) + 0.5);
      ctx.lineTo(plotW, Math.round(y) + 0.5);
      ctx.stroke();
      if (opts.labels !== false) {
        const v = hi - (hi - lo) * (i / rows);
        ctx.fillText((opts.fmt || ((n) => n.toFixed(0)))(v), plotW + 7, y);
      }
    }
    ctx.restore();

    for (const s of series) {
      const pts = pointsFor(s.series.data, s.series.cap, plotW, h, lo, hi, padTop, padBottom);
      if (pts.length < 2) continue;

      if (s.fill !== false) {
        const g = ctx.createLinearGradient(0, padTop, 0, h - padBottom);
        g.addColorStop(0, s.color + '55');
        g.addColorStop(1, s.color + '00');
        ctx.beginPath();
        tracePath(ctx, pts);
        ctx.lineTo(pts[pts.length - 1][0], h - padBottom);
        ctx.lineTo(pts[0][0], h - padBottom);
        ctx.closePath();
        ctx.fillStyle = g;
        ctx.fill();
      }

      ctx.beginPath();
      tracePath(ctx, pts);
      ctx.strokeStyle = s.color;
      ctx.lineWidth = s.width ?? 1.9;
      ctx.lineJoin = 'round';
      ctx.lineCap = 'round';
      ctx.shadowColor = s.color;
      ctx.shadowBlur = 9;
      ctx.stroke();
      ctx.shadowBlur = 0;

      // leading dot, so the eye finds "now" immediately
      const [lx, ly] = pts[pts.length - 1];
      ctx.beginPath();
      ctx.arc(lx, ly, 2.6, 0, Math.PI * 2);
      ctx.fillStyle = s.color;
      ctx.shadowColor = s.color;
      ctx.shadowBlur = 11;
      ctx.fill();
      ctx.shadowBlur = 0;
    }
  }

  function drawSpark(canvas, series, color, max) {
    const { ctx, w, h } = fitCanvas(canvas);
    if (!w || !h || series.data.length < 2) return;
    const hi = max ?? Math.max(series.max() * 1.2, 1);
    const pts = pointsFor(series.data, series.cap, w, h, 0, hi, 3, 3);
    const g = ctx.createLinearGradient(0, 0, 0, h);
    g.addColorStop(0, color + '45');
    g.addColorStop(1, color + '00');
    ctx.beginPath();
    tracePath(ctx, pts);
    ctx.lineTo(pts[pts.length - 1][0], h);
    ctx.lineTo(pts[0][0], h);
    ctx.closePath();
    ctx.fillStyle = g;
    ctx.fill();
    ctx.beginPath();
    tracePath(ctx, pts);
    ctx.strokeStyle = color;
    ctx.lineWidth = 1.6;
    ctx.lineJoin = 'round';
    ctx.shadowColor = color;
    ctx.shadowBlur = 7;
    ctx.stroke();
  }

  /* ---------------- radial gauges ---------------- */

  const ARC_R = 80;
  const ARC_LEN = 2 * Math.PI * ARC_R * 0.75;   // the visible 270° sweep
  const CIRC = 2 * Math.PI * ARC_R;

  class Gauge {
    constructor(root, opts) {
      this.root = root;
      this.opts = opts;
      this.arc = root.querySelector('.arc');
      this.track = root.querySelector('.track');
      this.num = root.querySelector('.val .n');
      this.svg = root.querySelector('.gauge');
      this.track.setAttribute('stroke-dasharray', `${ARC_LEN} ${CIRC}`);
      this.arc.setAttribute('stroke-dasharray', `0 ${CIRC}`);
      this.ticks = [];
      this.buildTicks(root.querySelector('.ticks'));
    }
    buildTicks(g, count = 44) {
      const ns = 'http://www.w3.org/2000/svg';
      for (let i = 0; i < count; i++) {
        const t = i / (count - 1);
        const a = (135 + t * 270) * Math.PI / 180;
        const [c, s] = [Math.cos(a), Math.sin(a)];
        const line = document.createElementNS(ns, 'line');
        line.setAttribute('x1', 100 + 93 * c); line.setAttribute('y1', 100 + 93 * s);
        line.setAttribute('x2', 100 + 99 * c); line.setAttribute('y2', 100 + 99 * s);
        line.setAttribute('class', 'tick');
        g.appendChild(line);
        this.ticks.push(line);
      }
    }
    set(value) {
      const { min = 0, max = 100, thresholds = [] } = this.opts;
      const frac = Math.max(0, Math.min(1, (value - min) / (max - min)));

      let c1 = C.cyan, c2 = C.violet;
      for (const t of thresholds) if (value >= t.at) { c1 = t.c1; c2 = t.c2; }
      this.svg.style.setProperty('--c1', c1);
      this.svg.style.setProperty('--c2', c2);
      this.root.style.setProperty('--c2', c2);
      this.arc.style.stroke = `url(#${this.gradient(c1, c2)})`;
      this.arc.setAttribute('stroke-dasharray', `${ARC_LEN * frac} ${CIRC}`);

      this.num.textContent = this.opts.fmt ? this.opts.fmt(value) : value.toFixed(0);
      const on = Math.round(frac * this.ticks.length);
      this.ticks.forEach((t, i) => t.classList.toggle('on', i < on));
    }
    // One <linearGradient> per colour pair, created lazily and shared.
    gradient(c1, c2) {
      const id = 'grad-' + (c1 + c2).replace(/[^a-z0-9]/gi, '');
      if (!document.getElementById(id)) {
        const ns = 'http://www.w3.org/2000/svg';
        let defs = document.querySelector('svg.gauge defs');
        if (!defs) {
          defs = document.createElementNS(ns, 'defs');
          document.querySelector('svg.gauge').appendChild(defs);
        }
        const g = document.createElementNS(ns, 'linearGradient');
        g.setAttribute('id', id);
        g.setAttribute('gradientUnits', 'userSpaceOnUse');
        g.setAttribute('x1', '20'); g.setAttribute('y1', '20');
        g.setAttribute('x2', '180'); g.setAttribute('y2', '180');
        for (const [off, col] of [['0%', c1], ['100%', c2]]) {
          const s = document.createElementNS(ns, 'stop');
          s.setAttribute('offset', off);
          s.setAttribute('stop-color', col);
          g.appendChild(s);
        }
        defs.appendChild(g);
      }
      return id;
    }
  }

  const gauges = {
    temp: new Gauge($('#gauge-temp'), {
      min: 20, max: 90, fmt: (v) => v.toFixed(1),
      thresholds: [{ at: 60, c1: C.amber, c2: C.magenta }, { at: 75, c1: C.rose, c2: C.magenta }],
    }),
    cpu: new Gauge($('#gauge-cpu'), {
      fmt: (v) => v.toFixed(0),
      thresholds: [{ at: 70, c1: C.amber, c2: C.magenta }, { at: 90, c1: C.rose, c2: C.magenta }],
    }),
    mem: new Gauge($('#gauge-mem'), {
      fmt: (v) => v.toFixed(0),
      thresholds: [{ at: 75, c1: C.amber, c2: C.magenta }, { at: 90, c1: C.rose, c2: C.magenta }],
    }),
  };

  /* ---------------- panel rendering ---------------- */

  // Slow-moving panels re-render only when their data actually changed;
  // rebuilding a table every second would fight text selection and scrolling.
  const lastHTML = {};
  function setHTML(el, html) {
    if (lastHTML[el.id] === html) return;
    lastHTML[el.id] = html;
    el.innerHTML = html;
  }

  function renderTop(s) {
    $('#hostname').textContent = s.host.hostname || 'raspberrypi';
    $('#model').textContent = s.host.model || '';
    const chips = [
      `<span class="chip">uptime <b>${duration(s.host.uptime)}</b></span>`,
      `<span class="chip">kernel <b>${esc(s.host.kernel)}</b></span>`,
      `<span class="chip">cores <b>${s.host.cores}</b></span>`,
      `<span class="chip">procs <b>${s.host.procs}</b></span>`,
    ];
    const th = s.thermal || {};
    if (th.anyNow) chips.push(`<span class="chip bad">⚠ throttling now</span>`);
    else if (th.anyEver) chips.push(`<span class="chip warn">⚠ throttled since boot</span>`);
    setHTML($('#topchips'), chips.join(''));
  }

  function renderCPU(s) {
    const c = s.cpu;
    $('#cpu-tag').textContent = `${c.freqMHz || '—'} MHz · ${c.minMHz}–${c.maxMHz}`;
    $('#core-tag').textContent = c.governor || '';
    plot($('#chart-cpu'), [
      { series: hist.cpu, color: C.cyan },
      { series: hist.cpuSys, color: C.magenta, fill: false, width: 1.4 },
      { series: hist.cpuWait, color: C.amber, fill: false, width: 1.4 },
    ], { min: 0, max: 100, fmt: (v) => v.toFixed(0) + '%' });

    setHTML($('#cpu-legend'), [
      ['total', C.cyan, c.total.toFixed(1) + '%'],
      ['system', C.magenta, c.system.toFixed(1) + '%'],
      ['iowait', C.amber, c.iowait.toFixed(1) + '%'],
      ['user', 'rgba(148,176,220,.45)', c.user.toFixed(1) + '%'],
    ].map(([k, col, v]) => `<span><i style="background:${col}"></i>${k} <b>${v}</b></span>`).join(''));

    renderCores(c.perCore || []);
  }

  // Cores are rebuilt only when the core count changes; the bars are then
  // driven by style so the CSS width transition actually has something to
  // animate between ticks.
  let coreEls = [];
  function renderCores(per) {
    const box = $('#cores');
    if (coreEls.length !== per.length) {
      box.innerHTML = per.map((_, i) =>
        `<div class="core"><span class="lbl">cpu${i}</span>` +
        `<span class="bar"><span></span></span><span class="pct">0%</span></div>`).join('');
      coreEls = [...box.querySelectorAll('.core')].map((el) => ({
        bar: el.querySelector('.bar'),
        fill: el.querySelector('.bar > span'),
        pct: el.querySelector('.pct'),
      }));
    }
    per.forEach((p, i) => {
      const e = coreEls[i];
      if (!e) return;
      const [c1, c2] = p > 90 ? [C.rose, C.magenta] : p > 70 ? [C.amber, C.magenta] : [C.cyan, C.violet];
      e.bar.style.setProperty('--c1', c1);
      e.bar.style.setProperty('--c2', c2);
      e.fill.style.width = p.toFixed(1) + '%';
      e.pct.textContent = p.toFixed(0) + '%';
    });
  }

  function renderThermal(s) {
    const t = s.thermal || {};
    $('#throttle-raw').textContent = t.raw || '';
    $('#volts').innerHTML = t.voltsCore ? `${t.voltsCore.toFixed(3)}<small>V</small>` : '—';
    $('#freq').innerHTML = `${s.cpu.freqMHz}<small>MHz</small>`;
    $('#gov').textContent = s.cpu.governor || '—';
    plot($('#chart-temp'), [{ series: hist.temp, color: C.magenta }],
      { min: 25, max: 90, fmt: (v) => v.toFixed(0) + '°' });

    setHTML($('#flags'), (t.flags || []).map((f) => {
      const level = f.now ? 'now' : f.ever ? 'ever' : 'clear';
      const when = f.now ? 'active' : f.ever ? 'since boot' : 'clear';
      return `<div class="flag" data-level="${level}"><span class="led"></span>` +
        `<span class="name">${esc(f.label)}</span><span class="when">${when}</span></div>`;
    }).join('') || '<div class="empty">firmware throttle state unavailable</div>');
  }

  function renderMemory(s) {
    const m = s.mem;
    $('#mem-tag').textContent = bytes(m.total);
    $('#ram-amt').textContent = `${bytes(m.used)} / ${bytes(m.total)}`;
    const pct = (v) => (m.total ? (100 * v / m.total) : 0).toFixed(2) + '%';
    const spans = $('#ram-stack').children;
    spans[0].style.width = pct(m.used);
    spans[1].style.width = pct(m.cached);
    spans[2].style.width = pct(m.buffers);
    setHTML($('#ram-legend'), [
      ['used', 'linear-gradient(90deg,#2ee6ff,#a78bfa)', bytes(m.used)],
      ['cached', 'rgba(167,139,250,.55)', bytes(m.cached)],
      ['buffers', 'rgba(46,230,255,.3)', bytes(m.buffers)],
      ['free', 'rgba(148,176,220,.25)', bytes(m.available)],
    ].map(([k, bg, v]) => `<span><i style="background:${bg}"></i>${k} <b>${v}</b></span>`).join(''));

    $('#swap-amt').textContent = m.swapTotal ? `${bytes(m.swapUsed)} / ${bytes(m.swapTotal)}` : 'not configured';
    $('#swap-bar').style.width = (m.swapPct || 0).toFixed(2) + '%';

    const c = s.cpu, n = s.host.cores || 1;
    const loadClass = (v) => v > n ? 'style="color:var(--rose)"' : v > n * 0.7 ? 'style="color:var(--amber)"' : '';
    $('#l1').innerHTML = `<span ${loadClass(c.load1)}>${c.load1.toFixed(2)}</span>`;
    $('#l5').textContent = c.load5.toFixed(2);
    $('#l15').textContent = c.load15.toFixed(2);
    $('#nprocs').innerHTML = `${s.host.procs}<small> / ${s.host.running} run</small>`;
  }

  function renderDisks(s) {
    setHTML($('#disks'), (s.disks || []).map((d) => {
      const col = d.pct > 90 ? [C.rose, C.magenta] : d.pct > 75 ? [C.amber, C.magenta] : [C.cyan, C.violet];
      return `<div class="meter"><div class="top">` +
        `<span class="name">${esc(d.mount)}</span>` +
        `<span class="sub">${esc(d.fstype)} · ${esc(d.device)}</span>` +
        `<span class="amt">${bytes(d.used)} / ${bytes(d.used + d.free)} · ${d.pct.toFixed(0)}%</span></div>` +
        `<div class="bar" style="--c1:${col[0]};--c2:${col[1]}"><span style="width:${d.pct.toFixed(2)}%"></span></div></div>`;
    }).join('') || '<div class="empty">no filesystems</div>');
  }

  function renderNetwork(s) {
    const nets = s.nets || [];
    const total = nets.reduce((a, n) => ({ rx: a.rx + n.rxBps, tx: a.tx + n.txBps }), { rx: 0, tx: 0 });
    $('#net-tag').textContent = `↓ ${bitrate(total.rx)}  ↑ ${bitrate(total.tx)}`;
    plot($('#chart-net'), [
      { series: hist.rx, color: C.cyan },
      { series: hist.tx, color: C.magenta },
    ], { floor: 8 * 1024, fmt: (v) => bitrate(v) });
    setHTML($('#net-legend'),
      `<span><i style="background:${C.cyan}"></i>down <b>${bitrate(total.rx)}</b></span>` +
      `<span><i style="background:${C.magenta}"></i>up <b>${bitrate(total.tx)}</b></span>`);

    setHTML($('#ifaces'), nets.map((n) => {
      const state = n.up ? 'ok' : 'bad';
      const drops = n.rxDrop ? ` · ${n.rxDrop.toLocaleString()} drops` : '';
      return `<div class="meter"><div class="top">` +
        `<span class="name">${esc(n.name)}</span>` +
        `<span class="pill ${state}">${n.up ? 'up' : 'down'}</span>` +
        `<span class="amt">${esc(n.addr || '')}</span></div>` +
        `<div class="legend" style="margin-top:2px">` +
        `<span><i style="background:${C.cyan}"></i>${bitrate(n.rxBps)} <b>${bytes(n.rxTotal)}</b></span>` +
        `<span><i style="background:${C.magenta}"></i>${bitrate(n.txBps)} <b>${bytes(n.txTotal)}</b></span>` +
        `<span class="muted">${esc(drops)}</span></div></div>`;
    }).join('') || '<div class="empty">no interfaces</div>');
  }

  function renderContainers(s) {
    const cs = s.containers || [];
    $('#docker-tag').textContent = cs.length ? `${cs.filter((c) => c.state === 'running').length} / ${cs.length} running` : '';
    if (!cs.length) { setHTML($('#containers'), '<div class="empty">no containers (or docker unavailable)</div>'); return; }
    setHTML($('#containers'),
      '<table><thead><tr><th>Name</th><th>Image</th><th class="num">CPU</th><th class="num">Mem</th><th>State</th></tr></thead><tbody>' +
      cs.map((c) => {
        const cls = c.state === 'running' ? (c.health && !c.health.includes('healthy') ? 'warn' : 'ok') : 'bad';
        return `<tr><td class="mono">${esc(c.name)}</td>` +
          `<td class="trunc muted">${esc(c.image)}</td>` +
          `<td class="num">${c.state === 'running' ? c.cpu.toFixed(1) + '%' : '—'}</td>` +
          `<td class="num">${c.state === 'running' && c.memUsed ? bytes(c.memUsed) : '—'}</td>` +
          `<td><span class="pill ${cls}">${esc(c.health || c.state)}</span></td></tr>`;
      }).join('') + '</tbody></table>');
  }

  function renderUnits(s) {
    const us = s.units || [];
    $('#units-tag').textContent = us.length ? `${us.filter((u) => u.active === 'active').length} / ${us.length} active` : '';
    if (!us.length) { setHTML($('#units'), '<div class="empty">no units watched</div>'); return; }
    const now = Date.now() / 1000;
    setHTML($('#units'),
      '<table><thead><tr><th>Unit</th><th>State</th><th class="num">Mem</th><th class="num">Uptime</th><th class="num">Restarts</th></tr></thead><tbody>' +
      us.map((u) => {
        const cls = u.active === 'active' ? 'ok' : u.active === 'failed' ? 'bad' : 'warn';
        return `<tr><td class="mono">${esc(u.name)}<div class="sub2" title="${esc(u.description || '')}">${esc(u.description || '')}</div></td>` +
          `<td><span class="pill ${cls}">${esc(u.active)} · ${esc(u.sub)}</span></td>` +
          `<td class="num">${u.mem ? bytes(u.mem) : '—'}</td>` +
          `<td class="num">${u.since ? duration(now - u.since) : '—'}</td>` +
          `<td class="num" ${u.restarts ? 'style="color:var(--amber)"' : ''}>${u.restarts}</td></tr>`;
      }).join('') + '</tbody></table>');
  }

  function renderProcs(s) {
    const ps = s.procs || [];
    if (!ps.length) { setHTML($('#procs'), '<div class="empty">sampling…</div>'); return; }
    $('#procs').innerHTML =
      '<table><thead><tr><th class="num">PID</th><th>Command</th><th>User</th><th class="num">CPU</th><th class="num">RSS</th></tr></thead><tbody>' +
      ps.map((p) => {
        const hot = p.cpu > 50 ? 'style="color:var(--rose)"' : p.cpu > 15 ? 'style="color:var(--amber)"' : '';
        return `<tr><td class="num muted">${p.pid}</td>` +
          `<td class="trunc mono" title="${esc(p.cmd || p.name)}">${esc(p.name)}</td>` +
          `<td class="muted">${esc(p.user)}</td>` +
          `<td class="num" ${hot}>${p.cpu.toFixed(1)}%</td>` +
          `<td class="num muted">${bytes(p.rss)}</td></tr>`;
      }).join('') + '</tbody></table>';
  }

  /* ---------------- log stream ---------------- */

  // BUFFER is what stays searchable; DOM_CAP is what is actually rendered.
  // Keeping those apart means a filter can reach further back than the visible
  // list without paying to lay out every row.
  const BUFFER = 500, DOM_CAP = 300;
  const logState = { lines: [], paused: false, filter: '', pending: 0, follow: true };
  const logsEl = $('#logs');
  const jumpBtn = $('#logjump');

  function prioClass(p) { return p <= 3 ? 'err' : p === 4 ? 'warn' : ''; }

  function logRow(l) {
    const d = new Date(l.ts);
    return `<div class="row" data-p="${prioClass(l.prio)}">` +
      `<span class="t">${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}</span>` +
      `<span class="u" title="${esc(l.unit)}">${esc(l.unit)}</span>` +
      `<span class="m">${esc(l.msg)}</span></div>`;
  }

  const matches = (l) =>
    !logState.filter || (l.msg + ' ' + l.unit).toLowerCase().includes(logState.filter);

  const nearBottom = () => logsEl.scrollHeight - logsEl.scrollTop - logsEl.clientHeight < 24;
  const toBottom = () => { logsEl.scrollTop = logsEl.scrollHeight; };

  // Appending one row beats re-rendering the list: it keeps the slide-in
  // animation on genuinely new lines instead of replaying it on all of them,
  // and it does not throw away the user's scroll position mid-read.
  function appendLine(l) {
    if (!matches(l)) return;
    const placeholder = logsEl.querySelector('.empty');
    if (placeholder) placeholder.remove();
    logsEl.insertAdjacentHTML('beforeend', logRow(l));
    while (logsEl.childElementCount > DOM_CAP) logsEl.firstElementChild.remove();
    if (logState.follow) toBottom();
  }

  // Full rebuild, for when the filter changes or a pause is lifted.
  function renderLogs() {
    const shown = logState.lines.filter(matches).slice(-DOM_CAP);
    if (!shown.length) {
      logsEl.innerHTML = `<div class="empty">${logState.lines.length ? 'nothing matches that filter' : 'waiting for journal…'}</div>`;
      return;
    }
    logsEl.innerHTML = shown.map(logRow).join('');
    if (logState.follow) toBottom();
  }

  // Following the tail is the default, and it resumes on its own the moment the
  // reader scrolls back down — no need to re-arm it by hand.
  logsEl.addEventListener('scroll', () => {
    logState.follow = nearBottom();
    jumpBtn.hidden = logState.follow;
  });
  jumpBtn.addEventListener('click', () => {
    logState.follow = true;
    jumpBtn.hidden = true;
    toBottom();
  });

  $('#logfilter').addEventListener('input', (e) => {
    logState.filter = e.target.value.trim().toLowerCase();
    logState.follow = true;
    jumpBtn.hidden = true;
    renderLogs();
  });

  $('#logpause').addEventListener('click', (e) => {
    logState.paused = !logState.paused;
    e.target.setAttribute('aria-pressed', String(logState.paused));
    e.target.textContent = logState.paused ? 'resume' : 'pause';
    if (!logState.paused) {
      logState.pending = 0;
      logState.follow = true;
      jumpBtn.hidden = true;
      renderLogs();
    }
  });

  /* ---------------- live connection ---------------- */

  const statusEl = $('#status'), statusText = $('#statustext');
  let lastSnapAt = 0;

  function setStatus(state, text) {
    statusEl.dataset.state = state;
    statusText.textContent = text;
  }

  function onSnapshot(s) {
    lastSnapAt = Date.now();
    setStatus('up', 'live');

    hist.cpu.push(s.cpu.total);
    hist.cpuSys.push(s.cpu.system);
    hist.cpuWait.push(s.cpu.iowait);
    hist.temp.push(s.thermal.tempC);
    hist.mem.push(s.mem.pct);
    const totals = (s.nets || []).reduce((a, n) => ({ rx: a.rx + n.rxBps, tx: a.tx + n.txBps }), { rx: 0, tx: 0 });
    hist.rx.push(totals.rx);
    hist.tx.push(totals.tx);

    gauges.temp.set(s.thermal.tempC);
    gauges.cpu.set(s.cpu.total);
    gauges.mem.set(s.mem.pct);
    $('#temp-tag').textContent = `peak ${hist.temp.max().toFixed(1)}°C`;
    $('#cpu-gauge-tag').textContent = `${s.host.cores} cores`;
    $('#mem-gauge-tag').textContent = bytes(s.mem.total);
    $('#temp-note').textContent = `${s.thermal.voltsCore ? s.thermal.voltsCore.toFixed(3) + ' V core · ' : ''}peak ${hist.temp.max().toFixed(1)}°C`;
    $('#cpu-note').textContent = `${s.cpu.freqMHz} MHz · load ${s.cpu.load1.toFixed(2)}`;
    $('#mem-note').textContent = `${bytes(s.mem.used)} used · ${bytes(s.mem.available)} available`;
    plotSpark(document.querySelector('[data-spark="temp"]'), hist.temp, C.magenta, 90);
    plotSpark(document.querySelector('[data-spark="cpu"]'), hist.cpu, C.cyan, 100);
    plotSpark(document.querySelector('[data-spark="mem"]'), hist.mem, C.violet, 100);

    renderTop(s);
    renderCPU(s);
    renderThermal(s);
    renderMemory(s);
    renderDisks(s);
    renderNetwork(s);
    renderContainers(s);
    renderUnits(s);
    renderProcs(s);
  }

  function connect(path, event, handler, onDrop) {
    let backoff = 1000;
    const open = () => {
      const es = new EventSource(path);
      es.addEventListener('open', () => { backoff = 1000; });
      es.addEventListener(event, (e) => {
        backoff = 1000;
        try { handler(JSON.parse(e.data)); } catch (err) { console.error(path, err); }
      });
      es.addEventListener('error', () => {
        es.close();
        if (onDrop) onDrop();
        setTimeout(open, backoff);
        backoff = Math.min(backoff * 1.7, 15000);
      });
    };
    open();
  }

  connect('/api/stream', 'snapshot', onSnapshot, () => setStatus('down', 'reconnecting'));

  connect('/api/logs', 'log', (l) => {
    logState.lines.push(l);
    if (logState.lines.length > BUFFER) logState.lines = logState.lines.slice(-BUFFER);
    if (logState.paused) {
      logState.pending++;
      $('#logpause').textContent = `resume (${logState.pending})`;
      return;
    }
    appendLine(l);
  });

  /* ---------------- chrome ---------------- */

  setInterval(() => {
    const d = new Date();
    $('#clock').textContent = `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
    // Nothing on the wire for a while means the stream is wedged even if the
    // EventSource has not surfaced an error yet.
    if (lastSnapAt && Date.now() - lastSnapAt > 8000) setStatus('down', 'stalled');
  }, 1000);


  /* ---------------- card layout: drag to rearrange, grip to resize ----------------

     The grid is 12 columns wide. A card's size is just its column span plus an
     optional pixel height, and its position is just its DOM order — so the whole
     layout serialises to three small maps that live in localStorage. */

  const grid = $('#grid');
  const LAYOUT_KEY = 'pidash.layout.v1';
  const GAP = 16, MIN_SPAN = 3, MAX_SPAN = 12, MIN_H = 180;
  const resetBtn = $('#resetlayout');

  const cardsOf = () => [...grid.children].filter((c) => c.dataset.card);
  const clamp = (v, lo, hi) => Math.max(lo, Math.min(hi, v));

  // Captured before anything is restored, so "reset" has something to go back to.
  const DEFAULT_ORDER = cardsOf().map((c) => c.dataset.card);

  function persist() {
    const out = { order: [], span: {}, h: {} };
    for (const c of cardsOf()) {
      out.order.push(c.dataset.card);
      const m = /span\s+(\d+)/.exec(c.style.gridColumn || '');
      if (m) out.span[c.dataset.card] = +m[1];
      if (c.style.height) out.h[c.dataset.card] = parseInt(c.style.height, 10);
    }
    try { localStorage.setItem(LAYOUT_KEY, JSON.stringify(out)); } catch { /* private mode */ }
    resetBtn.hidden = false;
  }

  function restore() {
    let saved = null;
    try { saved = JSON.parse(localStorage.getItem(LAYOUT_KEY) || 'null'); } catch { /* ignore */ }
    if (!saved || !Array.isArray(saved.order)) { resetBtn.hidden = true; return; }
    const byId = new Map(cardsOf().map((c) => [c.dataset.card, c]));
    // Re-appending in saved order also leaves any card the save predates in
    // place at the front, rather than dropping it.
    for (const id of saved.order) { const el = byId.get(id); if (el) grid.appendChild(el); }
    for (const [id, n] of Object.entries(saved.span || {})) {
      const el = byId.get(id);
      if (el) el.style.gridColumn = `span ${clamp(n, MIN_SPAN, MAX_SPAN)}`;
    }
    for (const [id, h] of Object.entries(saved.h || {})) {
      const el = byId.get(id);
      if (el) el.style.height = Math.max(MIN_H, h) + 'px';
    }
    resetBtn.hidden = false;
  }

  resetBtn.addEventListener('click', () => {
    const byId = new Map(cardsOf().map((c) => [c.dataset.card, c]));
    for (const id of DEFAULT_ORDER) {
      const el = byId.get(id);
      if (!el) continue;
      el.style.gridColumn = '';
      el.style.height = '';
      grid.appendChild(el);
    }
    try { localStorage.removeItem(LAYOUT_KEY); } catch { /* ignore */ }
    resetBtn.hidden = true;
  });

  /* --- drag to rearrange --- */

  let drag = null;

  function beginDrag(d, e) {
    const { card } = d;
    const ph = document.createElement('div');
    ph.className = 'card-placeholder';
    ph.style.gridColumn = card.style.gridColumn || getComputedStyle(card).gridColumn;
    ph.style.height = d.h + 'px';
    grid.insertBefore(ph, card);
    d.ph = ph;
    d.started = true;
    card.classList.add('dragging');
    grid.classList.add('is-dragging');
    // Fixed positioning takes the card out of grid flow, so the placeholder is
    // the only thing occupying a cell while the drag is in progress.
    Object.assign(card.style, {
      position: 'fixed', left: `${e.clientX - d.ox}px`, top: `${e.clientY - d.oy}px`,
      width: `${d.w}px`, height: `${d.h}px`, zIndex: '900', margin: '0',
    });
    document.body.style.userSelect = 'none';
  }

  function moveDrag(d, e) {
    d.card.style.left = `${e.clientX - d.ox}px`;
    d.card.style.top = `${e.clientY - d.oy}px`;

    let best = null, bestDist = Infinity;
    for (const c of cardsOf()) {
      if (c === d.card) continue;
      const r = c.getBoundingClientRect();
      const dist = Math.hypot(e.clientX - (r.left + r.width / 2), e.clientY - (r.top + r.height / 2));
      if (dist < bestDist) { bestDist = dist; best = { c, r }; }
    }
    if (!best) return;
    // Within a card's own row the decision is horizontal; otherwise vertical.
    const { r } = best;
    const after = e.clientY > r.top && e.clientY < r.bottom
      ? e.clientX > r.left + r.width / 2
      : e.clientY > r.top + r.height / 2;
    grid.insertBefore(d.ph, after ? best.c.nextSibling : best.c);
  }

  function endDrag(d) {
    if (d.started) {
      grid.insertBefore(d.card, d.ph);
      d.ph.remove();
      d.card.classList.remove('dragging');
      grid.classList.remove('is-dragging');
      for (const p of ['position', 'left', 'top', 'width', 'height', 'zIndex', 'margin']) {
        d.card.style[p] = '';
      }
      // Height is a layout property the grip owns; restore what it had set.
      if (d.savedHeight) d.card.style.height = d.savedHeight;
      document.body.style.userSelect = '';
      persist();
    }
    drag = null;
  }

  /* --- grip to resize --- */

  let resize = null;

  function endResize(r) {
    r.card.classList.remove('resizing');
    r.hint.remove();
    persist();
    resize = null;
  }

  for (const card of cardsOf()) {
    const grip = document.createElement('div');
    grip.className = 'grip';
    grip.title = 'Drag to resize';
    card.appendChild(grip);

    grip.addEventListener('pointerdown', (e) => {
      if (e.button) return;
      e.preventDefault();
      e.stopPropagation();
      grip.setPointerCapture(e.pointerId);
      const r = card.getBoundingClientRect();
      const col = (grid.getBoundingClientRect().width + GAP) / 12;
      const hint = document.createElement('div');
      hint.className = 'size-hint';
      card.appendChild(hint);
      resize = {
        card, hint, col, id: e.pointerId,
        sx: e.clientX, sy: e.clientY,
        span: clamp(Math.round((r.width + GAP) / col), MIN_SPAN, MAX_SPAN),
        h: r.height,
      };
      hint.textContent = `${resize.span}/12`;
      card.classList.add('resizing');
    });

    const head = card.querySelector(':scope > h2');
    if (!head) continue;
    head.addEventListener('pointerdown', (e) => {
      if (e.button || e.target.closest('input, button, .logbar')) return;
      head.setPointerCapture(e.pointerId);
      const r = card.getBoundingClientRect();
      drag = {
        card, id: e.pointerId, handle: head, started: false,
        sx: e.clientX, sy: e.clientY,
        ox: e.clientX - r.left, oy: e.clientY - r.top,
        w: r.width, h: r.height, savedHeight: card.style.height,
      };
    });
  }

  document.addEventListener('pointermove', (e) => {
    if (resize && e.pointerId === resize.id) {
      const span = clamp(resize.span + Math.round((e.clientX - resize.sx) / resize.col), MIN_SPAN, MAX_SPAN);
      const h = Math.max(MIN_H, Math.round(resize.h + (e.clientY - resize.sy)));
      resize.card.style.gridColumn = `span ${span}`;
      resize.card.style.height = `${h}px`;
      resize.hint.textContent = `${span}/12 · ${h}px`;
      return;
    }
    if (!drag || e.pointerId !== drag.id) return;
    if (!drag.started) {
      if (Math.hypot(e.clientX - drag.sx, e.clientY - drag.sy) < 6) return;
      beginDrag(drag, e);
    }
    moveDrag(drag, e);
  });

  for (const ev of ['pointerup', 'pointercancel']) {
    document.addEventListener(ev, (e) => {
      if (resize && e.pointerId === resize.id) endResize(resize);
      else if (drag && e.pointerId === drag.id) endDrag(drag);
    });
  }

  restore();

})();
