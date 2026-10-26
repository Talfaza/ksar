// Ksar web UI. Plain JavaScript, no dependencies, nothing loaded from
// outside. Untrusted data (guest output, file names, commands, errors) is
// only ever inserted with textContent: there is no innerHTML anywhere.
//
// Motion follows Apple's fluid-interface principles (motion.js): springs
// that start from the on-screen value, inherit gesture velocity, project
// momentum, rubber-band at edges, and can be grabbed or reversed mid-flight.
'use strict';

const { Spring, motion, project, rubberband, VelocityTracker, reducedMotion } = KsarMotion;

// ---------- DOM helpers ----------

const SVG_NS = 'http://www.w3.org/2000/svg';
const ICONS = {
  terminal: 'M4 17l6-6-6-6M12 19h8',
  plus: 'M12 5v14M5 12h14',
  layers: 'M12 2l10 5-10 5L2 7l10-5zM2 17l10 5 10-5M2 12l10 5 10-5',
  sliders: 'M4 21v-7M4 10V3M12 21v-9M12 8V3M20 21v-5M20 12V3M1 14h6M9 8h6M17 16h6',
  shield: 'M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10zM9 12l2 2 4-4',
  globe: 'M12 22a10 10 0 1 0 0-20 10 10 0 0 0 0 20zM2 12h20M12 2a15 15 0 0 1 0 20M12 2a15 15 0 0 0 0 20',
  shieldOff: 'M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10zM12 8v5M12 16v.01',
  stop: 'M7 7h10v10H7z',
  play: 'M7 4.5l12 7.5-12 7.5z',
  monitor: 'M3 4h18v12H3zM8 20h8M12 16v4',
  trash: 'M3 6h18M8 6V4h8v2M19 6l-1 14H6L5 6M10 11v6M14 11v6',
  copy: 'M9 9h11v11H9zM5 15H4V4h11v1',
  check: 'M20 6L9 17l-5-5',
  checkCircle: 'M12 22a10 10 0 1 0 0-20 10 10 0 0 0 0 20zM8 12l3 3 5-6',
  x: 'M18 6L6 18M6 6l12 12',
  upload: 'M12 16V4M7 9l5-5 5 5M4 20h16',
  download: 'M12 4v12M7 11l5 5 5-5M4 20h16',
  cpu: 'M6 6h12v12H6zM10 10h4v4h-4zM10 2v4M14 2v4M10 18v4M14 18v4M2 10h4M2 14h4M18 10h4M18 14h4',
  chevronLeft: 'M15 18l-6-6 6-6',
  chevronRight: 'M9 18l6-6-6-6',
  lock: 'M5 11h14v10H5zM8 11V7a4 4 0 0 1 8 0v4',
  alert: 'M12 3L2 20h20L12 3zM12 10v4M12 17v.01',
  bolt: 'M13 2L4 14h7l-1 8 9-12h-7l1-8z',
  netOff: 'M2 2l20 20M8.5 16.5a5 5 0 0 1 7 0M2 8.8a15 15 0 0 1 4.2-2.7M10.7 5.1A15 15 0 0 1 22 8.8M5 12.9a10 10 0 0 1 5.2-2.7M12 20h.01',
  disk: 'M4 4h16v16H4zM4 15h16M8 18h.01',
  folder: 'M3 6h6l2 2h10v11H3z',
  replay: 'M3 12a9 9 0 1 0 3-6.7L3 8M3 3v5h5',
  info: 'M12 22a10 10 0 1 0 0-20 10 10 0 0 0 0 20zM12 11v6M12 7v.01',
  wrap: 'M3 6h18M3 12h15a3 3 0 1 1 0 6h-4M16 16l-2 2 2 2M3 18h7',
  search: 'M11 19a8 8 0 1 0 0-16 8 8 0 0 0 0 16zM21 21l-4.3-4.3',
};

function icon(name, cls) {
  const span = document.createElement('span');
  span.className = 'ico' + (cls ? ' ' + cls : '');
  const svg = document.createElementNS(SVG_NS, 'svg');
  svg.setAttribute('viewBox', '0 0 24 24');
  svg.setAttribute('fill', 'none');
  svg.setAttribute('stroke', 'currentColor');
  svg.setAttribute('stroke-width', '2');
  svg.setAttribute('stroke-linecap', 'round');
  svg.setAttribute('stroke-linejoin', 'round');
  svg.setAttribute('aria-hidden', 'true');
  const p = document.createElementNS(SVG_NS, 'path');
  p.setAttribute('d', ICONS[name] || ICONS.info);
  svg.append(p);
  span.append(svg);
  return span;
}

// h(tag, props, ...children). Strings become text nodes; null/false skip.
function h(tag, props, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k === 'class') el.className = v;
    else if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else if (k === 'dataset') Object.assign(el.dataset, v);
    else if (k in el && k !== 'list') el[k] = v;
    else el.setAttribute(k, v === true ? '' : v);
  }
  put(el, children);
  return el;
}
function put(el, ...kids) {
  for (const c of kids.flat(Infinity)) {
    if (c === null || c === undefined || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}
function clear(el) { while (el.firstChild) el.firstChild.remove(); }
const clamp = (v, a, b) => Math.min(b, Math.max(a, v));

// ---------- API ----------

async function api(method, path, body) {
  const opts = { method, headers: {}, credentials: 'same-origin' };
  if (body instanceof FormData) opts.body = body;
  else if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(path, opts);
  if (res.status === 401) { location.reload(); throw new Error('signed out'); }
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || res.statusText);
  return data;
}

// ---------- Formatting ----------

function fmtBytes(n) {
  if (!n) return '0 B';
  const u = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return (i === 0 ? n : n.toFixed(n < 10 ? 1 : 0)) + ' ' + u[i];
}
function fmtDuration(ms) {
  if (ms < 0) ms = 0;
  if (ms < 1000) return Math.round(ms) + ' ms';
  const s = ms / 1000;
  if (s < 60) return s.toFixed(1) + ' s';
  const m = Math.floor(s / 60);
  return m + ' min ' + Math.floor(s % 60) + ' s';
}
function fmtClock(ms) {
  const s = Math.max(0, ms) / 1000;
  return String(Math.floor(s / 60)).padStart(2, '0') + ':' + (s % 60).toFixed(1).padStart(4, '0');
}
function fmtAgo(iso) {
  const d = (Date.now() - new Date(iso).getTime()) / 1000;
  if (d < 5) return 'Just now';
  if (d < 60) return Math.floor(d) + ' s ago';
  if (d < 3600) return Math.floor(d / 60) + ' min ago';
  if (d < 86400) return Math.floor(d / 3600) + ' h ago';
  return Math.floor(d / 86400) + ' d ago';
}
function fmtMem(mb) { return mb >= 1024 && mb % 1024 === 0 ? mb / 1024 + ' GB' : mb + ' MB'; }
function fmtSecs(s) {
  if (s % 3600 === 0) return s / 3600 + ' h';
  if (s % 60 === 0) return s / 60 + ' min';
  return s + ' s';
}
const STATUS_LABEL = {
  preparing: 'Installing desktop', booting: 'Booting', running: 'Running', succeeded: 'Succeeded', failed: 'Failed',
  timed_out: 'Timed out', stopped: 'Stopped', error: 'Ksar error',
};
function statusEl(status, code) {
  const extra = code !== undefined && code !== null && status !== 'succeeded' && !isLive(status) ? ' · ' + code : '';
  return h('span', { class: 'status ' + status }, h('span', { class: 'dot' }), (STATUS_LABEL[status] || status) + extra);
}
const isLive = s => s === 'preparing' || s === 'booting' || s === 'running';
function runDuration(r) {
  const end = r.ended ? new Date(r.ended).getTime() : Date.now();
  return end - new Date(r.created).getTime();
}
// What a run runs: its command, or the desktop for a display run without one.
const cmdLabel = spec => spec.label || spec.command || (spec.display ? 'Desktop (' + spec.display.replace('x', ' × ') + ')' : '');

// The puzzled mascot, for anything missing or not found.
const MISSING = '/mascot-missing.png';

function imageLabel(name) { return name ? name.replace(/\.tar$/, '') : 'Agent only'; }

// ---------- Toasts: materialize, exit the way they came, swipe away ----------

function toast(msg, kind) {
  const el = h('div', { class: 'toast' + (kind === 'err' ? ' err' : ''), role: kind === 'err' ? 'alert' : 'status' },
    icon(kind === 'err' ? 'alert' : 'checkCircle'), h('span', {}, msg));
  document.getElementById('toasts').append(el);
  const m = motion(el);
  const blur = new Spring({ value: 8, precision: 0.05, onUpdate: v => { el.style.filter = v > 0.05 ? `blur(${v}px)` : ''; } });
  blur.instantWhenReduced = true;
  m.set({ y: 20, scale: 0.92, opacity: 0 });
  m.to({ y: 0, scale: 1, opacity: 1 }, { damping: 1, response: 0.35 });
  blur.to(0, { damping: 1, response: 0.3 });

  let gone = false;
  const dismiss = (vx = 0) => {
    if (gone) return;
    gone = true;
    const out = vx ? { x: Math.sign(vx) * (el.offsetWidth + 60), opacity: 0 } : { y: 20, scale: 0.94, opacity: 0 };
    blur.to(vx ? 0 : 6, { damping: 1, response: 0.3 });
    m.to(out, { damping: 1, response: 0.32, velocity: { x: vx } }).then(() => el.remove());
  };
  let timer = setTimeout(dismiss, kind === 'err' ? 6000 : 3200);

  // Swipe sideways to dismiss: 1:1 tracking, momentum decides.
  let drag = null;
  el.addEventListener('pointerdown', e => {
    drag = { id: e.pointerId, sx: e.clientX, x0: m.state.x, vt: new VelocityTracker() };
    el.setPointerCapture(e.pointerId);
    m.springs.x.stop();
    clearTimeout(timer);
  });
  el.addEventListener('pointermove', e => {
    if (!drag || e.pointerId !== drag.id) return;
    const x = drag.x0 + e.clientX - drag.sx;
    m.set({ x, opacity: clamp(1 - Math.abs(x) / (el.offsetWidth * 1.2), 0.2, 1) });
    drag.vt.add(e.clientX, 0);
  });
  el.addEventListener('pointerup', release);
  el.addEventListener('pointercancel', release);
  function release(e) {
    if (!drag || e.pointerId !== drag.id) return;
    const vx = drag.vt.velocity().x;
    drag = null;
    const landing = m.state.x + project(vx);
    if (Math.abs(landing) > 120) dismiss(vx || Math.sign(m.state.x) * 600);
    else {
      m.to({ x: 0, opacity: 1 }, { velocity: { x: vx }, damping: 0.8, response: 0.3 });
      timer = setTimeout(dismiss, 2500);
    }
  }
}

// ---------- Segmented control: spring thumb you can drag and flick ----------

// segmented(items, value, onChange, {label, large}) where items are
// {value, label, tag?, count?}. A value that matches no item hides the
// thumb (e.g. a custom number).
function segmented(items, value, onChange, opts = {}) {
  const root = h('div', { class: 'seg' + (opts.large ? ' large' : ''), role: 'radiogroup', 'aria-label': opts.label || null });
  const thumb = h('div', { class: 'seg-thumb', 'aria-hidden': 'true' });
  root.append(thumb);
  const counts = [];
  const btns = items.map((it, i) => {
    const n = it.count !== undefined ? h('span', { class: 'n' }, String(it.count)) : null;
    counts.push(n);
    // The label reserves its semibold width, so selecting an item never
    // changes the layout (which would resize the control mid-animation).
    const b = h('button', { type: 'button', class: 'seg-item', role: 'radio' },
      h('span', { class: 'seg-label', 'data-text': it.label }, it.label), it.tag ? h('span', { class: 'tag' }, it.tag) : null, n);
    b.addEventListener('click', () => select(i, { commit: true }));
    b.addEventListener('keydown', e => {
      const d = e.key === 'ArrowRight' || e.key === 'ArrowDown' ? 1 : e.key === 'ArrowLeft' || e.key === 'ArrowUp' ? -1 : 0;
      if (!d) return;
      e.preventDefault();
      const j = clamp((index < 0 ? 0 : index) + d, 0, btns.length - 1);
      select(j, { commit: true });
      btns[j].focus();
    });
    root.append(b);
    return b;
  });
  let index = items.findIndex(it => it.value === value);

  const x = new Spring({ value: 0, precision: 0.1, onUpdate: v => { thumb.style.transform = `translateX(${v}px)`; } });
  const w = new Spring({ value: 0, precision: 0.1, onUpdate: v => { thumb.style.width = v + 'px'; } });
  x.instantWhenReduced = w.instantWhenReduced = true;
  const geom = i => ({ left: btns[i].offsetLeft, width: btns[i].offsetWidth });
  function place(i, o = {}) {
    root.classList.toggle('no-thumb', i < 0);
    if (i < 0 || !btns[i].offsetWidth) return;
    const g = geom(i);
    if (o.jump) { x.jump(g.left); w.jump(g.width); return; }
    x.to(g.left, { velocity: o.velocity, damping: o.damping ?? 1, response: 0.32 });
    w.to(g.width, { damping: 1, response: 0.32 });
  }
  function mark(i) {
    btns.forEach((b, j) => {
      b.setAttribute('aria-checked', String(j === i));
      b.tabIndex = j === i || (i < 0 && j === 0) ? 0 : -1;
    });
  }
  function select(i, o = {}) {
    index = i;
    mark(i);
    place(i, o);
    if (o.commit && onChange) onChange(items[i].value);
  }
  const nearest = cx => {
    let best = 0, bd = Infinity;
    btns.forEach((b, j) => { const d = Math.abs(b.offsetLeft + b.offsetWidth / 2 - cx); if (d < bd) { bd = d; best = j; } });
    return best;
  };

  // Drag the thumb: grab where you touched it, follow 1:1, resist past the
  // ends, and on release land where the flick was heading.
  let drag = null;
  root.addEventListener('pointerdown', e => {
    const b = e.target.closest('.seg-item');
    if (e.button !== 0 || !b || btns.indexOf(b) !== index || index < 0) return;
    drag = { id: e.pointerId, grab: e.clientX - x.value, sx: e.clientX, vt: new VelocityTracker(), moved: false, hover: index };
    drag.vt.add(e.clientX, 0);
  });
  root.addEventListener('pointermove', e => {
    if (!drag || e.pointerId !== drag.id) return;
    if (!drag.moved) {
      if (Math.abs(e.clientX - drag.sx) < 4) return; // hysteresis: a click, not a drag
      drag.moved = true;
      root.setPointerCapture(e.pointerId);
      root.classList.add('dragging');
      x.stop();
    }
    const min = geom(0).left, max = geom(btns.length - 1).left;
    let nx = e.clientX - drag.grab;
    if (nx < min) nx = min + rubberband(nx - min, root.offsetWidth);
    if (nx > max) nx = max + rubberband(nx - max, root.offsetWidth);
    x.jump(nx);
    drag.vt.add(e.clientX, 0);
    const near = nearest(nx + w.value / 2);
    if (near !== drag.hover) { drag.hover = near; mark(near); w.to(geom(near).width, { damping: 1, response: 0.25 }); }
  });
  const end = e => {
    if (!drag || e.pointerId !== drag.id) return;
    const d = drag;
    drag = null;
    if (!d.moved) return; // let the click handler run
    root.classList.remove('dragging');
    const v = d.vt.velocity().x;
    const i = nearest(x.value + w.value / 2 + project(v, 0.99));
    // Bounce only because the gesture carried momentum.
    select(i, { commit: true, velocity: v, damping: Math.abs(v) > 300 ? 0.8 : 1 });
    // The click that follows a drag must not re-select.
    const swallow = ev => { ev.stopPropagation(); ev.preventDefault(); };
    root.addEventListener('click', swallow, { capture: true, once: true });
    setTimeout(() => root.removeEventListener('click', swallow, { capture: true }), 0);
  };
  root.addEventListener('pointerup', end);
  root.addEventListener('pointercancel', end);

  // A real resize (window, container) snaps the thumb into place; while it
  // is moving, retarget instead so an animation in flight isn't cut short.
  new ResizeObserver(() => { if (!drag) place(index, { jump: !x.moving && !w.moving }); }).observe(root);
  mark(index);
  return {
    el: root,
    set(v) { const i = items.findIndex(it => it.value === v); index = i; mark(i); place(i); },
    setCount(i, n) { if (counts[i]) counts[i].textContent = String(n); requestAnimationFrame(() => place(index)); },
    get value() { return index >= 0 ? items[index].value : null; },
  };
}

// ---------- Value picker: presets + exact value ----------

// picker({presets: [[value, label, hint]], value, min, max, step, unit,
// defaultValue, describe(v), onChange(v)}). Out-of-range typing shows an
// inline error and keeps the last valid value; leaving the field snaps it
// into range.
function picker(o) {
  let value = o.value;
  const hint = h('p', { class: 't-foot', style: 'margin:0' });
  const err = h('p', { class: 'field-error', role: 'alert' });
  const seg = segmented(o.presets.map(([v, label]) => ({ value: v, label, tag: v === o.defaultValue ? 'default' : null })),
    value, v => set(v), { label: o.label });
  const input = h('input', {
    class: 'input', type: 'number', min: o.min, max: o.max, step: 1, inputMode: 'numeric', 'aria-label': o.label + ' (' + o.unit + ')',
  });
  const valid = v => Number.isInteger(v) && v >= o.min && v <= o.max;
  function render(keepInput) {
    if (!keepInput) input.value = value;
    input.classList.remove('invalid');
    err.textContent = '';
    seg.set(value);
    const p = o.presets.find(x => x[0] === value);
    hint.textContent = (p && p[2]) || o.describe(value);
  }
  function set(v, keepInput) { value = v; render(keepInput); o.onChange(v); }
  input.addEventListener('input', () => {
    const v = Number(input.value);
    if (valid(v)) { set(v, true); return; }
    input.classList.add('invalid');
    err.textContent = input.value === '' ? 'Enter a value.' : 'Choose between ' + o.min + ' and ' + o.max + ' ' + o.unit + '.';
  });
  const commit = () => {
    const v = Math.round(Number(input.value));
    set(Number.isFinite(v) && input.value !== '' ? clamp(v, o.min, o.max) : value);
  };
  input.addEventListener('blur', commit);
  input.addEventListener('change', commit);
  const step = d => () => set(clamp(value + d, o.min, o.max));
  render();
  return {
    el: h('div', { class: 'stack', style: 'gap:0.6rem' },
      seg.el,
      h('div', { class: 'row wrap' },
        h('span', { class: 't-sub' }, 'Exact'),
        h('div', { class: 'stepper' },
          h('button', { type: 'button', class: 'btn icon small', 'aria-label': 'Decrease', onclick: step(-o.step) }, '−'),
          input, h('span', { class: 'unit' }, o.unit),
          h('button', { type: 'button', class: 'btn icon small', 'aria-label': 'Increase', onclick: step(o.step) }, '+'))),
      err, hint),
    set(v) { set(v); },
    get value() { return value; },
  };
}

const MEM_PRESETS = [
  [128, '128 MB', 'Enough for shell scripts and small tools.'],
  [256, '256 MB', 'Most command-line tools and short scripts.'],
  [512, '512 MB', 'Python, Node and package managers.'],
  [1024, '1 GB', 'Builds, test suites, larger interpreters.'],
  [2048, '2 GB', 'Compilers and bigger datasets.'],
  [3072, '3 GB', 'The maximum for one VM.'],
];
const TIMEOUT_PRESETS = [
  [30, '30 s', 'Quick checks.'], [60, '1 min', 'Most scripts.'], [300, '5 min', 'Test suites and small builds.'],
  [900, '15 min', 'Larger builds.'], [3600, '1 h', 'Long jobs.'],
];
function memoryPicker(value, onChange, defaultValue) {
  return picker({
    presets: MEM_PRESETS, value, min: 64, max: 3072, step: 64, unit: 'MB', label: 'Guest memory', defaultValue, onChange,
    describe: v => 'Custom size: ' + fmtMem(v) + '. Files the command writes are kept in this memory too.',
  });
}
function timeoutPicker(value, onChange, defaultValue) {
  return picker({
    presets: TIMEOUT_PRESETS, value, min: 1, max: 86400, step: 30, unit: 's', label: 'Timeout', defaultValue, onChange,
    describe: v => 'Custom: ' + fmtSecs(v) + '. The VM is killed when it runs this long (exit code 124).',
  });
}

// ---------- Disclosure: spring height, interruptible ----------

function disclosure(title, body, open = false) {
  const wrap = h('div', { class: 'disclosure-body' }, h('div', { style: 'padding:0 1rem 1rem' }, body));
  const chev = icon('chevronRight', 'chev');
  const btn = h('button', { type: 'button', class: 'disclosure-btn', 'aria-expanded': String(open) }, chev, title);
  const height = new Spring({
    value: open ? -1 : 0, precision: 0.5,
    onUpdate: v => { wrap.style.height = v < 0 ? 'auto' : v + 'px'; },
    onRest: v => { if (v > 0 && isOpen) wrap.style.height = 'auto'; },
  });
  height.instantWhenReduced = true;
  const rot = new Spring({ value: open ? 90 : 0, precision: 0.1, onUpdate: v => { chev.style.transform = `rotate(${v}deg)`; } });
  rot.instantWhenReduced = true;
  let isOpen = open;
  wrap.style.height = open ? 'auto' : '0px';
  chev.style.transform = `rotate(${open ? 90 : 0}deg)`;
  btn.addEventListener('click', () => {
    isOpen = !isOpen;
    btn.setAttribute('aria-expanded', String(isOpen));
    // Start from the height on screen right now (it may be mid-flight).
    const current = wrap.getBoundingClientRect().height;
    height.value = current;
    height.to(isOpen ? wrap.scrollHeight : 0, { damping: 1, response: 0.35 });
    rot.to(isOpen ? 90 : 0, { damping: 1, response: 0.3 });
  });
  return h('div', { class: 'group' }, btn, wrap);
}

// ---------- Sheet: modal, slides up, drag down to dismiss ----------

// openSheet({build(close), onClosed}). Opening dims and pushes back the
// page; the sheet enters from the bottom and leaves the same way. Drag the
// grabber or header: it tracks 1:1, rubber-bands upward, and on release
// a flick downward (projected past a third of its height) dismisses.
function openSheet({ title, build, onClosed }) {
  const appEl = document.querySelector('.app');
  const restoreFocus = document.activeElement;
  const scrim = h('div', { class: 'scrim' });
  const closeBtn = h('button', { type: 'button', class: 'btn plain', onclick: () => dismiss() }, 'Cancel');
  const head = h('div', { class: 'sheet-head' }, h('h2', { class: 't-title', id: 'sheet-title' }, title), h('span', { class: 'spacer' }), closeBtn);
  const grip = h('div', { class: 'sheet-grip', 'aria-hidden': 'true' });
  const panel = h('div', { class: 'sheet', role: 'dialog', 'aria-modal': 'true', 'aria-labelledby': 'sheet-title' }, grip, head);
  const content = build(() => dismiss());
  put(panel, content);
  document.body.append(scrim, panel);
  document.body.classList.add('sheet-open');

  const m = motion(panel);
  const H = () => panel.offsetHeight || window.innerHeight;
  const reduce = reducedMotion();
  let closing = false;
  m.onChange = st => {
    const p = reduce ? st.opacity : clamp(1 - st.y / H(), 0, 1);
    scrim.style.opacity = String(p);
    if (!reduce) appEl.style.transform = p > 0.002 ? `scale(${1 - 0.03 * p})` : '';
  };
  m.set(reduce ? { y: 0, opacity: 0 } : { y: H(), opacity: 1 });
  m.to({ y: 0, opacity: 1 }, { damping: 1, response: 0.38 });

  let byUser = false;
  function finish() {
    panel.remove();
    scrim.remove();
    appEl.style.transform = '';
    document.body.classList.remove('sheet-open');
    document.removeEventListener('keydown', onKey, true);
    if (restoreFocus && restoreFocus.focus) restoreFocus.focus({ preventScroll: true });
    onClosed && onClosed({ byUser });
  }
  function close(velocity = 0) {
    if (closing) return;
    closing = true;
    byUser = false;
    m.to(reduce ? { opacity: 0 } : { y: H() + 24 }, { damping: 1, response: 0.32, velocity: { y: velocity } })
      .then(() => { if (closing) finish(); });
  }
  // A user dismissal updates the URL only once the sheet has actually
  // closed: grabbed back mid-close, it stays open and nothing changes.
  function dismiss(velocity) { close(velocity); byUser = true; }

  // Drag to dismiss.
  let drag = null;
  for (const handle of [grip, head]) {
    handle.addEventListener('pointerdown', e => {
      if (e.button !== 0 || e.target.closest('button, input, a')) return;
      drag = { id: e.pointerId, sy: e.clientY, y0: m.state.y, vt: new VelocityTracker() };
      handle.setPointerCapture(e.pointerId);
      m.springs.y.stop(); // grab it wherever it is, even mid-flight
      closing = false;
      drag.vt.add(0, e.clientY);
    });
    handle.addEventListener('pointermove', e => {
      if (!drag || e.pointerId !== drag.id) return;
      let y = drag.y0 + e.clientY - drag.sy;
      if (y < 0) y = rubberband(y, H());
      m.set({ y });
      drag.vt.add(0, e.clientY);
    });
    const up = e => {
      if (!drag || e.pointerId !== drag.id) return;
      const v = drag.vt.velocity().y;
      drag = null;
      if (m.state.y + project(v) > H() / 3) dismiss(v);
      else m.to({ y: 0 }, { velocity: { y: v }, damping: 0.8, response: 0.3 });
    };
    handle.addEventListener('pointerup', up);
    handle.addEventListener('pointercancel', up);
  }
  scrim.addEventListener('click', () => dismiss());

  // Keyboard: Escape closes; Tab stays inside the sheet.
  function onKey(e) {
    if (e.key === 'Escape') { e.preventDefault(); dismiss(); return; }
    if (e.key !== 'Tab') return;
    const f = Array.from(panel.querySelectorAll('button, input, a[href], [tabindex]:not([tabindex="-1"])')).filter(el => !el.disabled && el.offsetParent);
    if (!f.length) return;
    if (e.shiftKey && document.activeElement === f[0]) { e.preventDefault(); f[f.length - 1].focus(); }
    else if (!e.shiftKey && document.activeElement === f[f.length - 1]) { e.preventDefault(); f[0].focus(); }
  }
  document.addEventListener('keydown', onKey, true);
  return { close, panel };
}

// ---------- App shell, routing and transitions ----------

const app = { status: null, settings: null, cleanup: [], prefill: null, viewKey: null, viewEl: null, sheet: null, lastRoute: '#/runs' };
function onLeave(fn) { app.cleanup.push(fn); }
function every(ms, fn) { const t = setInterval(fn, ms); onLeave(() => clearInterval(t)); }

const DEPTH = { runs: 0, run: 1, images: 0, settings: 0 };

function route() {
  const hash = location.hash || '#/runs';
  const parts = hash.replace(/^#\/?/, '').split('/');
  for (const a of document.querySelectorAll('#nav a')) {
    a.classList.toggle('active', a.dataset.route === (parts[0] === 'run' ? 'runs' : parts[0]));
  }
  if (parts[0] === 'new') {
    app.sheetFromNav = !!app.viewKey; // opened from within the app (history has a page to return to)
    if (!app.viewKey) renderView(['runs']);
    if (!app.sheet) openNewRun();
    return;
  }
  app.lastRoute = hash;
  if (app.sheet) { app.sheet.close(); app.sheet = null; }
  renderView(parts);
}

function renderView(parts) {
  const key = parts.join('/');
  if (key === app.viewKey) return;
  const fromDepth = app.viewKey ? DEPTH[app.viewKey.split('/')[0]] ?? 0 : null;
  const toDepth = DEPTH[parts[0]] ?? 0;
  for (const fn of app.cleanup.splice(0)) fn();
  const main = document.getElementById('main');
  if (app.viewEl) app.viewEl.remove();
  const view = h('div', { class: 'view' });
  main.append(view);
  app.viewEl = view;
  app.viewKey = key;
  const views = { runs: viewRuns, run: v => viewRun(v, parts[1]), images: viewImages, settings: viewSettings };
  (views[parts[0]] || viewRuns)(view);
  window.scrollTo(0, 0);
  main.focus({ preventScroll: true });

  // Deeper pages enter from the right and return to the right; siblings
  // cross-fade. The new view starts moving at once: nothing waits.
  const m = motion(view);
  if (fromDepth === null) { m.set({ opacity: 0 }); m.to({ opacity: 1 }, { damping: 1, response: 0.3 }); return; }
  const dir = Math.sign(toDepth - fromDepth);
  m.set({ x: dir * 36, opacity: 0 });
  m.to({ x: 0, opacity: 1 }, { damping: 1, response: 0.38 }).then(() => { view.style.transform = ''; });
}

// toolbar + large title. The toolbar's material and small title appear
// once the large title scrolls under it.
function pageChrome(view, { title, subtitle, back, actions, header }) {
  const bar = h('div', { class: 'toolbar' },
    back ? h('a', { class: 'btn plain', href: back.href }, icon('chevronLeft'), back.label) : null,
    h('span', { class: 'small-title' }, title),
    h('span', { class: 'spacer' }),
    actions || null);
  const large = header || h('h1', { class: 't-large' }, title);
  const head = header || h('div', { class: 'page-head' }, h('div', {}, large, subtitle ? h('p', { class: 't-sub' }, subtitle) : null));
  view.append(bar, head);
  const io = new IntersectionObserver(([e]) => bar.classList.toggle('scrolled', !e.isIntersecting), { rootMargin: '-52px 0px 0px 0px' });
  io.observe(large);
  onLeave(() => io.disconnect());
  return { bar, head };
}

// The run list is pushed by the server when it changes (no polling).
// Views subscribe with onRuns(fn); the sidebar badge always listens.
const runsListeners = new Set();
let lastRuns = null;
function onRuns(fn) {
  runsListeners.add(fn);
  onLeave(() => runsListeners.delete(fn));
  if (lastRuns) fn(lastRuns);
}
function watchRuns() {
  const es = new EventSource('/api/runs/events');
  es.addEventListener('runs', e => {
    lastRuns = JSON.parse(e.data);
    const n = lastRuns.filter(r => isLive(r.status)).length;
    document.getElementById('nav-active').textContent = n ? String(n) : '';
    for (const fn of runsListeners) fn(lastRuns);
  });
}

async function stopRun(id) {
  try { await api('POST', '/api/runs/' + id + '/stop'); toast('Stopping run #' + id); } catch (e) { toast(e.message, 'err'); }
}

// ---------- Runs ----------

function viewRuns(view) {
  pageChrome(view, {
    title: 'Runs', subtitle: 'Each run boots a fresh microVM and destroys it when the command ends. Nothing it writes survives.',
    actions: h('div', { class: 'row' },
      h('button', { class: 'btn', onclick: () => { app.prefill = { project: true }; location.hash = '#/new'; } }, icon('folder'), 'Run a Project'),
      h('a', { class: 'btn primary', href: '#/new' }, icon('plus'), 'New Run')),
  });
  const filter = { tab: 'all', q: '' };
  const tabs = segmented([
    { value: 'all', label: 'All', count: 0 }, { value: 'active', label: 'Active', count: 0 },
    { value: 'succeeded', label: 'Succeeded', count: 0 }, { value: 'failed', label: 'Failed', count: 0 },
  ], 'all', v => { filter.tab = v; render(); }, { label: 'Filter runs' });
  const search = h('label', { class: 'command', style: 'height:2.25rem;max-width:20rem;flex:1;border-radius:9px' },
    icon('search'),
    h('input', { type: 'search', placeholder: 'Search', 'aria-label': 'Search runs', style: 'height:2.25rem;font-family:var(--font)', oninput: e => { filter.q = e.target.value.toLowerCase(); render(); } }));
  const body = h('div');
  view.append(h('div', { class: 'row wrap', style: 'margin-bottom:1rem' }, h('div', { style: 'min-width:22rem' }, tabs.el), h('span', { class: 'spacer' }), search), body);

  let runs = [];
  function render() {
    const counts = { all: runs.length, active: 0, succeeded: 0, failed: 0 };
    for (const r of runs) {
      if (isLive(r.status)) counts.active++;
      else if (r.status === 'succeeded') counts.succeeded++;
      else counts.failed++;
    }
    ['all', 'active', 'succeeded', 'failed'].forEach((k, i) => tabs.setCount(i, counts[k]));
    clear(body);
    if (!runs.length) {
      body.append(h('div', { class: 'group' }, h('div', { class: 'empty' },
        h('img', { src: '/mascot.png', alt: '' }),
        h('h2', { class: 't-title' }, 'No runs yet'),
        h('p', {}, 'Every run starts clean in its own microVM and is destroyed when it ends.'),
        h('a', { class: 'btn primary', href: '#/new', style: 'margin-top:0.5rem' }, icon('bolt'), 'Start a Run'))));
      return;
    }
    const shown = runs.filter(r => {
      if (filter.tab === 'active' && !isLive(r.status)) return false;
      if (filter.tab === 'succeeded' && r.status !== 'succeeded') return false;
      if (filter.tab === 'failed' && (isLive(r.status) || r.status === 'succeeded')) return false;
      return !filter.q || (cmdLabel(r.spec) + ' ' + r.spec.image + ' ' + r.id).toLowerCase().includes(filter.q);
    });
    const rows = shown.map(r => h('tr', { class: 'clickable', tabIndex: 0, onclick: () => { location.hash = '#/run/' + r.id; }, onkeydown: e => { if (e.key === 'Enter') location.hash = '#/run/' + r.id; } },
      h('td', {}, statusEl(r.status, r.exit_code)),
      h('td', {}, h('span', { class: 'cmd', title: cmdLabel(r.spec) }, cmdLabel(r.spec))),
      h('td', {}, imageLabel(r.spec.image)),
      h('td', { class: 'num' }, fmtDuration(runDuration(r))),
      h('td', { class: 'num', title: 'Peak memory of the run\'s cgroup' }, r.mem_peak || r.mem_current ? fmtBytes(r.mem_peak || r.mem_current) : '—'),
      h('td', {}, fmtAgo(r.created)),
      h('td', { style: 'text-align:right' }, isLive(r.status)
        ? h('button', { class: 'btn danger small', onclick: e => { e.stopPropagation(); stopRun(r.id); } }, icon('stop'), 'Stop')
        : icon('chevronRight', 'label-3'))));
    body.append(h('div', { class: 'table-wrap' }, h('div', { class: 'table-scroll' }, h('table', {},
      h('thead', {}, h('tr', {}, ['Status', 'Command', 'Image', 'Duration', 'Memory', 'Started', ''].map((t, i) => h('th', { class: i === 3 || i === 4 ? 'num' : null }, t)))),
      h('tbody', {}, rows)))));
    if (!rows.length) {
      clear(body);
      body.append(h('div', { class: 'group' }, h('div', { class: 'empty' }, h('img', { src: MISSING, alt: '' }),
        h('h2', { class: 't-title' }, 'No Matching Runs'),
        h('p', {}, filter.q ? 'Nothing matches “' + filter.q + '” here.' : 'No runs in this list yet.'))));
    }
  }
  onRuns(list => { runs = list; render(); });
}

// ---------- New run (sheet) ----------

function openNewRun() {
  const prefill = app.prefill || {};
  app.prefill = null;
  const fromNav = app.sheetFromNav;
  const sheet = openSheet({
    title: 'New Run',
    build: close => buildNewRun(prefill, close),
    onClosed: ({ byUser }) => {
      if (app.sheet === sheet) app.sheet = null;
      if (byUser && location.hash === '#/new') {
        if (fromNav) history.back(); else location.replace(app.lastRoute);
      }
    },
  });
  app.sheet = sheet;
}

// ---------- Network policy control ----------

// Hosts commonly needed to install dependencies, offered as one-tap adds.
const NET_PRESETS = [
  ['PyPI', ['pypi.org', 'files.pythonhosted.org']],
  ['npm', ['registry.npmjs.org']],
  ['Go modules', ['proxy.golang.org', 'sum.golang.org']],
  ['crates.io', ['crates.io', 'index.crates.io', 'static.crates.io']],
  ['GitHub', ['github.com', 'codeload.github.com', '*.githubusercontent.com']],
];
const NET_FOOT = {
  none: 'No network card at all: nothing inside can reach your network or the internet.',
  allow: 'Only these hosts, by name or address. Everything else is refused, and every lookup and connection is logged.',
  internet: 'Any public address. Your local network, this machine and cloud metadata stay blocked; every connection is logged.',
};
const parseAllow = text => text.split(/[\s,]+/).map(x => x.trim()).filter(Boolean);
const DISPLAY_FOOT = {
  off: 'No screen: the command\'s output appears as text.',
  on: 'Opens an XFCE desktop on the run page, with keyboard and mouse. Leave the command empty for just the desktop, or name a program to open on it. ' +
    'If the image has no desktop, Ksar installs a minimal XFCE into a separate cached layer the first time (a few minutes, inside a VM that reaches only the distribution\'s package mirrors).',
};

function netSummary(mode, allow) {
  if (mode === 'allow') return allow.length + ' allowed host' + (allow.length === 1 ? '' : 's');
  if (mode === 'internet') return 'Internet access';
  return 'No network';
}

// networkControl({mode, allow, onChange}) edits a network policy: off, an
// allow list, or the internet. onChange gets {mode, allow}.
function networkControl(o) {
  let mode = o.mode || 'none';
  const area = h('textarea', {
    class: 'input mono', rows: 4, spellcheck: false, autocomplete: 'off', 'aria-label': 'Allowed hosts',
    placeholder: 'pypi.org\nfiles.pythonhosted.org\n*.example.com\ngithub.com:443',
  });
  area.value = (o.allow || []).join('\n');
  const changed = () => o.onChange({ mode, allow: parseAllow(area.value) });
  area.addEventListener('input', changed);
  const chips = h('div', { class: 'chips' }, ...NET_PRESETS.map(([label, hosts]) => h('button', {
    type: 'button', class: 'btn small', onclick: () => {
      const have = parseAllow(area.value);
      area.value = have.concat(hosts.filter(x => !have.includes(x))).join('\n');
      changed();
    },
  }, icon('plus'), label)));
  const allowBox = h('div', { class: 'stack', style: 'gap:0.5rem' }, area, chips,
    h('span', { class: 't-foot' }, 'One per line: a host name, *.domain for its subdomains, an IPv4 address or prefix, each optionally with :port.'));
  const foot = h('span', { class: 't-foot' });
  const render = animate => {
    foot.textContent = NET_FOOT[mode];
    const show = mode === 'allow';
    if (show === !allowBox.hidden) return;
    allowBox.hidden = !show;
    if (show && animate) { // appears in place, from slightly above
      const m = motion(allowBox);
      m.set({ y: -6, opacity: 0 });
      m.to({ y: 0, opacity: 1 }, { damping: 1, response: 0.3 });
    }
  };
  const seg = segmented([
    { value: 'none', label: 'Off' }, { value: 'allow', label: 'Allow List' }, { value: 'internet', label: 'Internet' },
  ], mode, v => { mode = v; render(true); changed(); }, { label: 'Network' });
  render(false);
  return {
    el: h('div', { class: 'stack', style: 'gap:0.6rem' }, seg.el, foot, allowBox),
    set(v) { mode = v.mode; area.value = v.allow.join('\n'); seg.set(mode); render(false); changed(); },
  };
}

function buildNewRun(pre, close) {
  const s = app.settings || { mem_mb: 256, timeout_s: 60 };
  const form = {
    command: pre.command || '', image: pre.image ?? null, mem: pre.mem_mb || s.mem_mb, timeout: pre.timeout_s || s.timeout_s,
    workdir: pre.workdir || '', env: (pre.env || []).slice(), files: [], interactive: !!pre.interactive,
    net: pre.net || s.net || 'none', allow: (pre.allow || s.allow || []).slice(),
    display: pre.display || '',
  };
  const body = h('div', { class: 'sheet-body' });
  const summary = h('div', { class: 'summary t-sub' });
  const errBox = h('div');
  const runBtn = h('button', { class: 'btn primary large', type: 'button', onclick: submit }, 'Run in Sandbox');

  // Command
  const cmdInput = h('input', {
    value: form.command, placeholder: "sh -c 'echo hello from the sandbox'", spellcheck: false, autocomplete: 'off',
    'aria-label': 'Command', oninput: e => { form.command = e.target.value; renderSummary(); },
  });
  body.append(h('div', { class: 'section' },
    h('label', { class: 'command' }, h('span', { class: 'prompt' }, '$'), cmdInput),
    h('p', { class: 't-foot group-foot' }, "Plain commands run directly. Commands with shell syntax (; && | > $VAR *.py) run in the image's shell, sh -c.")));

  // Image
  const imageGrid = h('div', { class: 'image-grid' });
  let images = [];
  function renderImages() {
    clear(imageGrid);
    const choices = [{ name: '', label: 'Agent only', detail: 'No filesystem image' }]
      .concat(images.map(i => ({ name: i.name, label: imageLabel(i.name), detail: fmtBytes(i.size) + (i.cached ? ' · ready' : ' · converted on first run') })));
    if (form.importRef) {
      // Suggested for a project, not in the library yet: imported on Run.
      choices.unshift({ name: null, ref: form.importRef, label: form.importRef, detail: 'Imported from Docker when you run (first time only)' });
    }
    for (const c of choices) {
      const selected = c.ref ? !form.image : !form.importRef && form.image === c.name;
      imageGrid.append(h('button', {
        type: 'button', class: 'image-card pressable', 'aria-pressed': String(selected),
        onclick: () => { if (!c.ref) { form.image = c.name; form.importRef = null; } renderImages(); renderSummary(); },
      }, h('span', { class: 'name' }, c.label), h('span', { class: 't-foot' }, c.detail)));
    }
  }
  api('GET', '/api/images').then(list => {
    images = list.filter(i => !i.importing && !i.error);
    if (form.image === null) form.image = images.length ? images[0].name : '';
    if (form.image && !images.some(i => i.name === form.image)) form.image = '';
    renderImages(); renderSummary();
  }, () => { form.image = form.image || ''; renderImages(); });
  body.append(h('div', { class: 'section' }, h('div', { class: 't-caps group-title' }, 'Root filesystem'), imageGrid));

  // Files and folders
  const fileList = h('div', { class: 'stack', style: 'gap:0.5rem' });
  const uploads = h('div', { class: 'stack', style: 'gap:0.5rem' });
  const projectBox = h('div');
  const fileInput = h('input', { type: 'file', multiple: true, hidden: true, onchange: e => { uploadFiles(e.target.files); e.target.value = ''; } });
  const folderInput = h('input', { type: 'file', multiple: true, hidden: true, webkitdirectory: true, onchange: e => { uploadPickedFolder(e.target.files); e.target.value = ''; } });
  const drop = h('div', {
    class: 'drop',
    ondragover: e => { e.preventDefault(); drop.classList.add('over'); },
    ondragleave: () => drop.classList.remove('over'),
    ondrop: e => { e.preventDefault(); drop.classList.remove('over'); handleDrop(e.dataTransfer); },
  }, icon('upload', 'big'), h('span', { class: 'field-label' }, 'Drop files or folders'),
  h('div', { class: 'row', style: 'justify-content:center' },
    h('button', { type: 'button', class: 'btn small', onclick: () => fileInput.click() }, 'Add Files…'),
    h('button', { type: 'button', class: 'btn small', onclick: () => folderInput.click() }, 'Add Folder…')));
  body.append(h('div', { class: 'section' }, h('div', { class: 't-caps group-title' }, 'Files and folders'),
    h('div', { class: 'stack', style: 'gap:0.6rem' }, drop, fileInput, folderInput, uploads, projectBox, fileList),
    h('p', { class: 't-foot group-foot' }, 'Copied in before the command starts: files up to 64 MB, folders up to 512 MB. A .zip or .tar.gz is unpacked into a folder. ' +
      'Drop a project and Ksar suggests how to run it. Browsers don\'t send permissions, so scripts starting with #! are made executable. Your originals stay on this machine.')));
  if (pre.project) {
    setTimeout(() => {
      drop.scrollIntoView({ block: 'center', behavior: reducedMotion() ? 'auto' : 'smooth' });
      drop.classList.add('over');
      setTimeout(() => drop.classList.remove('over'), 1200);
    }, 350);
  }

  const isArchive = name => /\.(zip|tar|tar\.gz|tgz)$/i.test(name);
  // Detect what an uploaded folder is and suggest how to run it. The first
  // project fills the form if the command is still empty.
  let project = null;
  async function detect(entry) {
    if (project) return;
    let sug;
    try { sug = await api('POST', '/api/projects/detect', { upload: entry.upload }); } catch { return; }
    // ai: the built-in model's pick ({state, choices, ms} or a message).
    // option: the way to run it shown on the card (a command and its image,
    // network, memory and timeout; each part of a project has its own).
    const options = sug.options && sug.options.length ? sug.options : [sug];
    project = { sug, entry, applied: false, ai: { state: 'idle' }, option: options[0] };
    const setCommand = c => { form.command = c; cmdInput.value = c; renderSummary(); };
    const useOption = o => {
      project.applied = true;
      project.option = o;
      setCommand(o.command);
      form.workdir = entry.path; workdirInput.value = entry.path;
      if (o.image) { form.image = o.image; form.importRef = null; } else { form.image = ''; form.importRef = o.docker; }
      renderImages();
      net.set({ mode: o.net, allow: o.allow || [] });
      // The option's environment replaces the previous option's; variables
      // the user added stay.
      const key = e => e.split('=')[0];
      const mine = new Set((project.env || []).map(key));
      form.env = form.env.filter(e => !mine.has(key(e))).concat(o.env || []);
      project.env = o.env || [];
      renderEnv();
      if (form.mem < o.mem_mb) mem.set(o.mem_mb);
      if (form.timeout < o.timeout_s) tmo.set(o.timeout_s);
      renderSummary();
      renderProject();
    };
    const apply = () => useOption(project.option);
    const renderProject = () => {
      clear(projectBox);
      const ai = project.ai, o = project.option;
      const netText = o.net === 'allow' ? 'network: ' + o.allow.join(', ') + ' (for dependencies)' : 'no network';
      let aiLine = null;
      if (ai.state === 'thinking') aiLine = h('span', { class: 't-foot ai-status' }, h('span', { class: 'spinner' }), 'Choosing with ' + ai.model + '…');
      else if (ai.state === 'chosen' && ai.choices.length > 1) {
        // Every option the model weighed, its pick first, grouped by the
        // part of the project it runs; picking one uses its environment.
        const opt = (c, i) => h('option', { value: i, selected: c.command === form.command }, c.command);
        const parts = [...new Set(ai.choices.map(c => c.component || ''))];
        const select = h('select', { class: 'input mono', 'aria-label': 'Other commands the model considered',
          onchange: e => useOption(ai.choices[+e.target.value]) },
        ...(parts.length < 2 ? ai.choices.map(opt) : parts.map(part => h('optgroup', { label: part || 'Other' },
          ...ai.choices.map((c, i) => (c.component || '') === part ? opt(c, i) : null).filter(Boolean)))));
        aiLine = h('div', { class: 'stack', style: 'gap:0.35rem' },
          h('span', { class: 't-foot' }, 'Chosen by ' + ai.model + ' (on this machine, ' + fmtDuration(ai.ms) + ') from ' + ai.choices.length + ' options:'), select);
      } else if (ai.state === 'absent') {
        aiLine = h('div', { class: 'row wrap', style: 'gap:0.5rem' }, h('span', { class: 't-foot' }, 'Chosen by rules.'),
          h('button', { type: 'button', class: 'btn small', onclick: downloadModel }, icon('download'),
            'Let the built-in AI choose (' + fmtBytes(ai.size) + ', once)'));
      } else if (ai.state === 'downloading') {
        aiLine = h('div', { class: 'stack', style: 'gap:0.35rem' }, h('span', { class: 't-foot' }, 'Downloading ' + ai.model + '… ' + Math.floor(100 * ai.done / ai.size) + '%'),
          h('div', { class: 'progress' }, h('div', { style: 'width:' + (100 * ai.done / ai.size).toFixed(1) + '%' })));
      } else if (ai.state === 'error') aiLine = h('span', { class: 't-foot' }, 'Chosen by rules (' + ai.error + ').');
      projectBox.append(h('div', { class: 'project-card' + (project.applied ? ' applied' : '') },
        icon('bolt', 'accent'),
        h('div', { class: 'text' },
          h('span', { class: 'k' }, sug.label + (sug.reasons && sug.reasons.length ? ' · ' + sug.reasons.join(', ') : '')),
          // The project's own command; setup steps in front of it (a
          // runtime install, starting a database) are summed up below.
          h('code', { class: 'mono' }, project.applied && form.command !== o.command ? form.command : (o.core || o.command)),
          o.core && (!project.applied || form.command === o.command) ? h('span', { class: 't-foot' }, 'Runs after setup steps in the VM; the full command is in the Command field.') : null,
          h('span', { class: 't-foot' }, [o.component, o.image ? imageLabel(o.image) : o.docker + ' (imported on Run)', netText,
            fmtMem(o.mem_mb), fmtSecs(o.timeout_s)].filter(Boolean).join(' · ')),
          (o.note || sug.note) ? h('span', { class: 't-foot' }, o.note || sug.note) : null,
          aiLine),
        project.applied ? h('span', { class: 'badge ok' }, icon('checkCircle'), 'Filled in')
          : h('button', { type: 'button', class: 'btn small primary', onclick: apply }, 'Use This')));
    };
    // Ask the model, if it is installed; otherwise offer it.
    const askModel = async () => {
      let st;
      try { st = await api('GET', '/api/assistant'); } catch { st = { state: 'error', error: 'unavailable' }; }
      project.ai = { state: st.state, model: st.model, size: st.size, done: st.done, error: st.error };
      if (st.state === 'downloading') { renderProject(); setTimeout(askModel, 1000); return; }
      if (st.state !== 'ready' || options.length < 2) { renderProject(); return; }
      project.ai.state = 'thinking';
      renderProject();
      try {
        const r = await api('POST', '/api/projects/rank', { upload: entry.upload });
        const before = project.option.command;
        project.ai = { state: 'chosen', model: r.model, ms: r.ms, choices: r.choices };
        // Only take the model's pick if the user hasn't changed the command.
        if (!project.applied) project.option = r.choices[0];
        else if (form.command === before) { useOption(r.choices[0]); return; }
      } catch (e) { project.ai = { state: 'error', error: e.message }; }
      renderProject();
    };
    async function downloadModel() {
      try { await api('POST', '/api/assistant/download'); } catch (e) { toast(e.message, 'err'); return; }
      askModel();
    }
    if (!form.command.trim()) apply(); else renderProject();
    askModel();
  }

  async function uploadFiles(list) {
    for (const f of list) {
      if (isArchive(f.name)) { uploadArchive(f); continue; }
      const fd = new FormData();
      fd.append('file', f, f.name);
      try {
        const u = await api('POST', '/api/uploads', fd);
        form.files.push({ upload: u.id, name: u.name, size: u.size, path: '/work/' + u.name });
        renderFiles();
      } catch (e) { toast(f.name + ': ' + e.message, 'err'); }
    }
  }
  function uploadArchive(f) {
    const bar = h('div');
    const row = h('div', { class: 'file-row' }, icon('folder', 'accent'), h('span', { class: 'name' }, f.name),
      h('span', { class: 't-foot' }, 'unpacking · ' + fmtBytes(f.size)), h('div', { class: 'progress' }, bar));
    uploads.append(row);
    const fd = new FormData();
    fd.append('file', f, f.name);
    const xhr = new XMLHttpRequest();
    xhr.open('POST', '/api/uploads/archive');
    xhr.upload.onprogress = e => { if (e.lengthComputable) bar.style.width = (100 * e.loaded / e.total).toFixed(1) + '%'; };
    xhr.onload = () => {
      row.remove();
      let u = {};
      try { u = JSON.parse(xhr.responseText); } catch { /* not JSON */ }
      if (xhr.status !== 200) { toast(f.name + ': ' + (u.error || xhr.statusText), 'err'); return; }
      const entry = { upload: u.id, name: u.name, size: u.size, files: u.files, dir: true, path: '/work/' + u.name };
      form.files.push(entry);
      renderFiles();
      detect(entry);
    };
    xhr.onerror = () => { row.remove(); toast(f.name + ': upload failed', 'err'); };
    xhr.send(fd);
  }
  function uploadPickedFolder(list) {
    const byTop = new Map();
    for (const f of list) {
      const [top, ...rest] = f.webkitRelativePath.split('/');
      if (!byTop.has(top)) byTop.set(top, []);
      byTop.get(top).push({ file: f, rel: rest.join('/') });
    }
    for (const [name, items] of byTop) uploadFolder(name, items, []);
  }
  function handleDrop(dt) {
    const entries = Array.from(dt.items || [], it => it.webkitGetAsEntry && it.webkitGetAsEntry()).filter(Boolean);
    if (!entries.length) { uploadFiles(dt.files); return; }
    const files = [];
    for (const entry of entries) {
      if (entry.isDirectory) walkFolder(entry).then(({ items, dirs }) => uploadFolder(entry.name, items, dirs), e => toast(e.message, 'err'));
      else files.push(new Promise((res, rej) => entry.file(res, rej)));
    }
    Promise.all(files).then(uploadFiles, e => toast(e.message, 'err'));
  }
  async function walkFolder(root) {
    const items = [], dirs = [];
    const readAll = reader => new Promise((res, rej) => {
      const out = [];
      const next = () => reader.readEntries(batch => { if (!batch.length) res(out); else { out.push(...batch); next(); } }, rej);
      next();
    });
    async function walk(dir, prefix) {
      const children = await readAll(dir.createReader());
      if (!children.length && prefix) dirs.push(prefix.slice(0, -1));
      for (const c of children) {
        if (c.isDirectory) await walk(c, prefix + c.name + '/');
        else items.push({ file: await new Promise((res, rej) => c.file(res, rej)), rel: prefix + c.name });
      }
    }
    await walk(root, '');
    return { items, dirs };
  }
  function uploadFolder(name, items, dirs) {
    const total = items.reduce((a, it) => a + it.file.size, 0);
    const bar = h('div');
    const row = h('div', { class: 'file-row' }, icon('folder', 'accent'), h('span', { class: 'name' }, name + '/'),
      h('span', { class: 't-foot' }, items.length + ' files · ' + fmtBytes(total)), h('div', { class: 'progress' }, bar));
    uploads.append(row);
    const fd = new FormData();
    for (const d of dirs) fd.append('dir', d);
    for (const it of items) { fd.append('path', it.rel); fd.append('file', it.file, 'f'); }
    const xhr = new XMLHttpRequest(); // XMLHttpRequest, not fetch: upload progress
    xhr.open('POST', '/api/uploads/dir?name=' + encodeURIComponent(name));
    xhr.upload.onprogress = e => { if (e.lengthComputable) bar.style.width = (100 * e.loaded / e.total).toFixed(1) + '%'; };
    xhr.onload = () => {
      row.remove();
      let u = {};
      try { u = JSON.parse(xhr.responseText); } catch { /* not JSON */ }
      if (xhr.status !== 200) { toast(name + ': ' + (u.error || xhr.statusText), 'err'); return; }
      const entry = { upload: u.id, name: u.name, size: u.size, files: u.files, dir: true, path: '/work/' + u.name };
      form.files.push(entry);
      renderFiles();
      detect(entry);
    };
    xhr.onerror = () => { row.remove(); toast(name + ': upload failed', 'err'); };
    xhr.send(fd);
  }
  function renderFiles() {
    clear(fileList);
    for (const f of form.files) {
      fileList.append(h('div', { class: 'file-row' }, icon(f.dir ? 'folder' : 'disk', 'accent'),
        h('span', { class: 'name', title: f.name }, f.dir ? f.name + '/' : f.name),
        h('span', { class: 't-foot' }, (f.dir ? f.files + ' files · ' : '') + fmtBytes(f.size)),
        icon('chevronRight', 'label-3'),
        h('input', { class: 'input mono', value: f.path, 'aria-label': 'Guest path for ' + f.name, oninput: e => { f.path = e.target.value; } }),
        h('button', { type: 'button', class: 'btn icon plain', 'aria-label': 'Remove ' + f.name, onclick: () => { form.files.splice(form.files.indexOf(f), 1); renderFiles(); } }, icon('x'))));
    }
    renderSummary();
  }

  // Resources
  const mem = memoryPicker(form.mem, v => { form.mem = v; renderSummary(); }, s.mem_mb);
  const tmo = timeoutPicker(form.timeout, v => { form.timeout = v; renderSummary(); }, s.timeout_s);
  body.append(h('div', { class: 'section' }, h('div', { class: 't-caps group-title' }, 'Resources'),
    h('div', { class: 'group' },
      h('div', { class: 'cell stacked' }, h('span', { class: 'field-label' }, 'Memory'), mem.el),
      h('div', { class: 'cell stacked' }, h('span', { class: 'field-label' }, 'Timeout'), tmo.el))));

  // Display: a screen for GUI programs, off unless asked for
  const dispSeg = segmented([{ value: '', label: 'Off' }, { value: '1024x768', label: '1024 × 768' },
    { value: '1280x800', label: '1280 × 800' }, { value: '1920x1080', label: '1920 × 1080' }],
  form.display, v => {
    form.display = v; dispFoot.textContent = DISPLAY_FOOT[v ? 'on' : 'off'];
    cmdInput.placeholder = v ? 'Optional: a program to open on the desktop, e.g. xfce4-terminal' : "sh -c 'echo hello from the sandbox'";
    // A desktop needs more than a script: raise the defaults, visibly.
    if (v && (form.mem < 1536 || form.timeout < 1800)) {
      if (form.mem < 1536) mem.set(1536);
      if (form.timeout < 1800) tmo.set(1800);
      toast('Desktop: memory set to 1.5 GB and timeout to 30 min');
    }
    renderSummary();
  }, { label: 'Display' });
  const dispFoot = h('span', { class: 't-foot' }, DISPLAY_FOOT[form.display ? 'on' : 'off']);
  body.append(h('div', { class: 'section' }, h('div', { class: 't-caps group-title' }, 'Display'),
    h('div', { class: 'group' }, h('div', { class: 'cell stacked' }, dispSeg.el, dispFoot))));

  // Network: off unless asked for
  const net = networkControl({ mode: form.net, allow: form.allow, onChange: v => { form.net = v.mode; form.allow = v.allow; renderSummary(); } });
  body.append(h('div', { class: 'section' }, h('div', { class: 't-caps group-title' }, 'Network'),
    h('div', { class: 'group' }, h('div', { class: 'cell stacked' }, net.el))));

  // Advanced, one level deeper
  const workdirInput = h('input', { class: 'input mono', value: form.workdir, placeholder: 'From the image, or /', oninput: e => { form.workdir = e.target.value; } });
  const envList = h('div', { class: 'stack', style: 'gap:0.5rem' });
  function renderEnv() {
    clear(envList);
    form.env.forEach((kv, i) => {
      envList.append(h('div', { class: 'row' },
        h('input', { class: 'input mono', value: kv, placeholder: 'KEY=value', 'aria-label': 'Environment variable', oninput: e => { form.env[i] = e.target.value; } }),
        h('button', { type: 'button', class: 'btn icon plain', 'aria-label': 'Remove variable', onclick: () => { form.env.splice(i, 1); renderEnv(); } }, icon('x'))));
    });
  }
  renderEnv();
  const stdinSwitch = h('label', { class: 'switch' }, h('input', { type: 'checkbox', checked: form.interactive, 'aria-label': 'Interactive stdin', onchange: e => { form.interactive = e.target.checked; } }), h('span'));
  body.append(h('div', { class: 'section' }, disclosure('Advanced', h('div', { class: 'stack' },
    h('div', { class: 'stack', style: 'gap:0.4rem' }, h('span', { class: 'field-label' }, 'Working directory'),
      workdirInput),
    h('div', { class: 'stack', style: 'gap:0.4rem' }, h('span', { class: 'field-label' }, 'Environment variables'), envList,
      h('div', {}, h('button', { type: 'button', class: 'btn small', onclick: () => { form.env.push(''); renderEnv(); } }, icon('plus'), 'Add Variable'))),
    h('div', { class: 'row' }, h('div', { class: 'stack', style: 'gap:0.1rem;flex:1' }, h('span', { class: 'field-label' }, 'Interactive stdin'),
      h('span', { class: 't-foot' }, 'Type input to the command from the run page. Off: the command gets an empty stdin.')), stdinSwitch),
  ), !!(form.workdir || form.env.length || form.interactive))));
  body.append(errBox);

  function renderSummary() {
    clear(summary);
    const n = form.files.length;
    summary.append(h('div', { style: 'color:var(--label);font-weight:590' }, (!form.image && form.importRef ? form.importRef : imageLabel(form.image)) + ' · ' + fmtMem(form.mem) + ' · ' + fmtSecs(form.timeout)),
      h('div', {}, netSummary(form.net, form.allow) + (form.display ? ' · ' + form.display.replace('x', ' × ') + ' display' : '') + ' · ' + (n ? n + ' item' + (n === 1 ? '' : 's') + ' copied in · ' : '') + 'destroyed afterwards'));
  }
  // Imports a Docker image into the library and waits for it.
  async function importImage(ref) {
    const name = ref.replace(/[^A-Za-z0-9._-]+/g, '_') + '.tar';
    const label = runBtn.textContent;
    try { await api('POST', '/api/images/import', { ref }); } catch (e) {
      if (!/already in the library|already being imported/.test(e.message)) throw e;
    }
    const t0 = Date.now();
    try {
      for (;;) {
        runBtn.textContent = 'Importing ' + ref + '… ' + Math.round((Date.now() - t0) / 1000) + ' s';
        const img = (await api('GET', '/api/images')).find(i => i.name === name);
        if (img && img.error) throw new Error('importing ' + ref + ': ' + img.error);
        if (img && !img.importing) return name;
        await new Promise(r => setTimeout(r, 1000));
      }
    } finally { runBtn.textContent = label; }
  }
  async function submit() {
    clear(errBox);
    runBtn.disabled = true;
    try {
      if (!form.image && form.importRef) {
        form.image = await importImage(form.importRef);
        form.importRef = null;
      }
      const run = await api('POST', '/api/runs', {
        command: form.command, image: form.image || '', mem_mb: form.mem, timeout_s: form.timeout,
        // A project run is named by its own command, not its setup steps.
        label: project && project.option && project.option.core && form.command === project.option.command ? project.option.core : '',
        env: form.env.filter(e => e.trim()), workdir: form.workdir.trim(), interactive: form.interactive,
        net: form.net, allow: form.allow, display: form.display,
        files: form.files.map(f => ({ upload: f.upload, name: f.name, path: f.path })),
      });
      location.replace('#/run/' + run.id);
    } catch (e) {
      errBox.append(h('div', { class: 'error-box', role: 'alert' }, e.message));
      runBtn.disabled = false;
    }
  }
  body.addEventListener('keydown', e => { if ((e.ctrlKey || e.metaKey) && e.key === 'Enter') { e.preventDefault(); submit(); } });
  renderSummary();
  setTimeout(() => cmdInput.focus({ preventScroll: true }), 50);
  return [body, h('div', { class: 'sheet-foot' }, summary, h('kbd', { class: 't-foot' }, 'Ctrl ↵'), runBtn)];
}

// ---------- Terminal rendering ----------

// Term renders filtered terminal output. The server already removed every
// control sequence except SGR colors, so this only interprets colors,
// newlines, carriage returns and backspace; everything else is text.
class Term {
  constructor(el) { this.el = el; this.style = {}; this.lines = 0; this.newLine(); }
  newLine() {
    this.line = document.createElement('div');
    this.el.append(this.line);
    if (++this.lines > 5000) { this.el.firstChild.remove(); this.lines--; }
  }
  sgr(params) {
    const p = params === '' ? [0] : params.split(/[;:]/).map(Number);
    for (let i = 0; i < p.length; i++) {
      const n = p[i];
      if (n === 0) this.style = {};
      else if (n === 1) this.style.b = true;
      else if (n === 2) this.style.d = true;
      else if (n === 3) this.style.i = true;
      else if (n === 4) this.style.u = true;
      else if (n === 22) { this.style.b = false; this.style.d = false; }
      else if (n === 23) this.style.i = false;
      else if (n === 24) this.style.u = false;
      else if (n >= 30 && n <= 37) { this.style.fg = n - 30; this.style.rgb = null; }
      else if (n >= 90 && n <= 97) { this.style.fg = n - 90 + 8; this.style.rgb = null; }
      else if (n === 39) { this.style.fg = null; this.style.rgb = null; }
      else if (n >= 40 && n <= 47) this.style.bg = n - 40;
      else if (n === 49) this.style.bg = null;
      else if (n === 38 && p[i + 1] === 5) { this.style.rgb = xterm256(p[i + 2] || 0); this.style.fg = null; i += 2; }
      else if (n === 38 && p[i + 1] === 2) {
        const nums = p.slice(i + 2).filter(x => !Number.isNaN(x)).slice(-3);
        this.style.rgb = 'rgb(' + nums.map(x => Math.min(255, x || 0)).join(',') + ')'; this.style.fg = null; i = p.length;
      }
    }
  }
  text(s, cls) {
    if (!s) return;
    const span = document.createElement('span');
    const c = [cls];
    const st = this.style;
    if (st.b) c.push('b'); if (st.d) c.push('d'); if (st.i) c.push('i'); if (st.u) c.push('u');
    if (st.fg !== null && st.fg !== undefined) c.push('fg' + st.fg);
    if (st.bg !== null && st.bg !== undefined) c.push('bg' + st.bg);
    span.className = c.filter(Boolean).join(' ');
    if (st.rgb) span.style.color = st.rgb;
    span.textContent = s;
    this.line.append(span);
  }
  write(data, cls) {
    const stick = this.el.scrollHeight - this.el.scrollTop - this.el.clientHeight < 40;
    const parts = data.split(/\x1b\[([0-9;:]*)m/);
    for (let i = 0; i < parts.length; i++) {
      if (i % 2 === 1) { this.sgr(parts[i]); continue; }
      let buf = '';
      for (const ch of parts[i]) {
        if (ch === '\n') { this.text(buf, cls); buf = ''; this.newLine(); }
        else if (ch === '\r') { this.text(buf, cls); buf = ''; this.pendingCR = true; continue; }
        else if (ch === '\b') { if (buf) buf = buf.slice(0, -1); }
        else {
          if (this.pendingCR) clear(this.line);
          buf += ch;
        }
        this.pendingCR = false;
      }
      this.text(buf, cls);
    }
    if (stick) this.el.scrollTop = this.el.scrollHeight;
  }
  plainText() { return Array.from(this.el.children, l => l.textContent).join('\n'); }
}

function xterm256(n) {
  const base = ['#6e6e73', '#ff6b5f', '#4cd964', '#ffd60a', '#64b5ff', '#da8fff', '#5ac8fa', '#e5e1dc',
    '#98989d', '#ff8f85', '#7ae68c', '#ffe066', '#8fc8ff', '#e5b0ff', '#8ad8fb', '#ffffff'];
  if (n < 16) return base[n];
  if (n >= 232) { const v = 8 + (n - 232) * 10; return 'rgb(' + v + ',' + v + ',' + v + ')'; }
  n -= 16;
  const c = x => (x ? 55 + x * 40 : 0);
  return 'rgb(' + c(Math.floor(n / 36)) + ',' + c(Math.floor(n / 6) % 6) + ',' + c(n % 6) + ')';
}

// ---------- Run detail ----------

function viewRun(view, id) {
  if (!id) { location.hash = '#/runs'; return; }
  let info = null;
  let wrap = false;
  const outEl = h('pre', { class: 'term', tabIndex: 0, 'aria-label': 'Command output' });
  const conEl = h('pre', { class: 'term', tabIndex: 0, hidden: true, 'aria-label': 'Guest kernel log' });
  const netEl = h('div', { class: 'term netlog', tabIndex: 0, hidden: true, role: 'log', 'aria-label': 'Network activity' });
  let netCount = 0;
  const out = new Term(outEl);
  const con = new Term(conEl);
  let outBytes = 0, conBytes = 0;
  const hero = h('div', { class: 'run-hero' });
  pageChrome(view, { title: 'Run #' + id, back: { href: '#/runs', label: 'Runs' }, header: hero });

  // The display pane: the guest's screen, live over VNC while the run is.
  const screen = h('canvas', { class: 'screen', tabIndex: 0, width: 1280, height: 800, 'aria-label': 'Guest display. Click to type into it.' });
  const dispStatus = h('span', { class: 't-foot' }, 'Not connected');
  const dispEmptyTitle = h('h2', { class: 't-title' });
  const dispEmptyText = h('p', {});
  const dispEmpty = h('div', { class: 'empty display-empty', hidden: true }, h('img', { src: MISSING, alt: '' }), dispEmptyTitle, dispEmptyText);
  // An empty screen needs an explanation, not a black box: nothing draws
  // unless the command starts a graphical program.
  const NOTHING_DREW = 'A display shows graphical programs. Start X and an app in an image that has them, ' +
    "e.g. sh -c 'Xorg :0 & sleep 2; xterm' (see examples/desktop). Check Output for notes.";
  function showEmpty(title, text) { dispEmptyTitle.textContent = title; dispEmptyText.textContent = text; screen.hidden = true; dispEmpty.hidden = false; }
  const dispEl = h('div', { class: 'display-pane', hidden: true },
    h('div', { class: 'display-bar' }, dispStatus, h('span', { class: 'spacer' }),
      h('button', { class: 'btn small', onclick: () => vnc && vnc.combo(0xffe3, 0xffe9, 0xffff) }, 'Ctrl+Alt+Del'),
      h('button', { class: 'btn small', onclick: () => (document.fullscreenElement ? document.exitFullscreen() : dispEl.requestFullscreen()).catch(() => { }) }, 'Full Screen')),
    screen, dispEmpty);
  let vnc = null, everConnected = false, retries = 0, painted = false;
  function openDisplay() {
    if (vnc || !info) return;
    if (info.status === 'preparing') {
      dispStatus.textContent = 'Installing a desktop into this image: first time only, a few minutes. Progress is on the Output tab.';
      return;
    }
    if (!isLive(info.status)) {
      dispStatus.textContent = 'The run has ended';
      if (!everConnected) showEmpty('Nothing To Show', 'The run ended before its screen showed anything. ' + NOTHING_DREW);
      else if (!painted) showEmpty('Nothing Was Drawn', NOTHING_DREW);
      return;
    }
    const url = (location.protocol === 'https:' ? 'wss://' : 'ws://') + location.host + '/api/runs/' + encodeURIComponent(id) + '/vnc';
    let size = '';
    vnc = KsarVNC.connect(url, screen, (st, msg) => {
      if (st === 'painted') { painted = true; dispStatus.textContent = 'Live · ' + size + ' · click the screen to type'; return; }
      if (st === 'connected') size = msg;
      dispStatus.textContent = st === 'connected'
        ? 'Live · ' + msg + (painted ? ' · click the screen to type' : ' · waiting for a program to draw…') : msg;
      if (st === 'connected') { everConnected = true; retries = 0; screen.focus({ preventScroll: true }); }
      if (st === 'closed' || st === 'error') {
        vnc = null;
        // The run's VNC socket appears a moment after the run starts.
        if (!everConnected && info && isLive(info.status) && retries++ < 20) setTimeout(() => { if (!dispEl.hidden) openDisplay(); }, 500);
        else if (info && !isLive(info.status)) openDisplay();
      }
    });
  }
  onLeave(() => { if (vnc) vnc.close(); });

  const tabSlot = h('div', { style: 'min-width:16rem' });
  let wrapBtn, copyBtn;
  let tabs = null, tabOrder = [];
  const setCount = (v, n) => { const i = tabOrder.indexOf(v); if (tabs && i >= 0) tabs.setCount(i, n); };
  function showTab(v) {
    outEl.hidden = v !== 'output'; conEl.hidden = v !== 'console'; netEl.hidden = v !== 'net'; dispEl.hidden = v !== 'display';
    wrapBtn.hidden = copyBtn.hidden = v === 'display'; // text tools only
    if (v === 'display') openDisplay();
  }
  // Built once the run's state arrives: runs with a display open on it.
  function buildTabs(withDisplay) {
    const items = [{ value: 'output', label: 'Output', count: fmtBytes(outBytes) }, { value: 'console', label: 'Kernel Log', count: fmtBytes(conBytes) },
      { value: 'net', label: 'Network', count: netCount }];
    if (withDisplay) items.unshift({ value: 'display', label: 'Display' });
    tabOrder = items.map(it => it.value);
    tabs = segmented(items, items[0].value, showTab, { label: 'Output' });
    tabSlot.append(tabs.el);
    showTab(items[0].value);
  }
  const side = h('div', { class: 'stack sticky-col' });
  const stdinBar = h('div');
  const foot = h('div', { class: 'term-foot' });
  view.append(h('div', { class: 'split' },
    h('div', { class: 'term-card' },
      h('div', { class: 'term-bar' }, tabSlot, h('span', { class: 'spacer' }),
        wrapBtn = h('button', { class: 'btn small', onclick: () => { wrap = !wrap; outEl.classList.toggle('wrap', wrap); conEl.classList.toggle('wrap', wrap); } }, icon('wrap'), 'Wrap'),
        copyBtn = h('button', {
          class: 'btn small', onclick: () => navigator.clipboard.writeText(!netEl.hidden
            ? Array.from(netEl.children, l => l.textContent).join('\n') : (outEl.hidden ? con : out).plainText())
            .then(() => toast('Copied to clipboard'), () => toast('Copy failed', 'err')),
        }, icon('copy'), 'Copy')),
      dispEl, outEl, conEl, netEl, stdinBar, foot),
    side));

  // Persistent side panels, updated in place (the meter moves on a spring).
  const tileV = {};
  const tile = (k, label) => h('div', { class: 'tile' }, h('span', { class: 't-foot' }, label), tileV[k] = h('span', { class: 'v' }, '—'));
  const memText = h('span', { class: 'v mono' });
  const meterFill = h('div', { style: 'transform:scaleX(0)' });
  const meter = new Spring({ value: 0, precision: 0.001, onUpdate: v => { meterFill.style.transform = `scaleX(${v})`; } });
  const cpuText = h('span', { class: 'v' });
  const netText = h('span', { class: 'v' });
  const netTraffic = h('span', { class: 't-foot', style: 'text-align:right' });
  const netCell = h('div', { class: 'cell stacked', hidden: true, style: 'gap:0.15rem' },
    h('div', { class: 'row' }, h('span', { class: 'k' }, 'Network'), h('span', { class: 'spacer' }), netText), netTraffic);
  const memCell = h('div', { class: 'cell stacked' }, h('div', { class: 'row' }, h('span', { class: 'k' }, 'Memory'), h('span', { class: 'spacer' }), memText),
    h('div', { class: 'meter' }, meterFill));
  const metrics = h('div', { class: 'section' }, h('div', { class: 't-caps group-title' }, 'Live'),
    h('div', { class: 'group' },
      h('div', { class: 'tiles' }, tile('boot', 'Boot'), tile('dur', 'Duration'), tile('cid', 'vsock CID'), tile('pid', 'Host PID')),
      memCell, h('div', { class: 'cell' }, h('span', { class: 'k' }, 'CPU'), cpuText), netCell));
  const resultBox = h('div');
  const protBox = h('div', { class: 'section' });
  side.append(resultBox, metrics, protBox);

  function renderHero() {
    clear(hero);
    const live = isLive(info.status);
    put(hero,
      h('img', { src: '/mascot.png', alt: '', class: info.status === 'booting' ? 'booting' : null }),
      h('div', { class: 'what' },
        h('code', { title: cmdLabel(info.spec) }, cmdLabel(info.spec)),
        h('div', { class: 'row wrap t-sub' }, statusEl(info.status, info.exit_code),
          h('span', {}, [imageLabel(info.spec.image), info.boot_ms ? 'booted in ' + fmtDuration(info.boot_ms) : null,
            info.kernel, info.virtio ? 'virtio + vsock' : 'legacy serial'].filter(Boolean).join('  ·  ')))),
      h('span', { class: 'timer', id: 'run-timer' }, fmtClock(runDuration(info))),
      live ? h('button', { class: 'btn danger', onclick: () => stopRun(info.id) }, icon('stop'), 'Stop') : null);
  }
  function renderMetrics() {
    const live = isLive(info.status);
    tileV.boot.textContent = info.boot_ms ? fmtDuration(info.boot_ms) : '—';
    tileV.dur.textContent = fmtDuration(runDuration(info));
    tileV.cid.textContent = info.cid ? String(info.cid) : '—';
    tileV.pid.textContent = info.pid ? String(info.pid) : '—';
    if (info.scope) {
      memCell.hidden = false;
      memText.textContent = fmtBytes(info.mem_current || info.mem_peak) + ' of ' + fmtBytes(info.mem_limit);
      meter.to(info.mem_limit ? clamp((info.mem_current || 0) / info.mem_limit, 0, 1) : 0, { damping: 1, response: 0.5 });
    } else memCell.hidden = true;
    cpuText.textContent = live && info.scope ? info.cpu_percent.toFixed(0) + '% of one core' : '—';
    netCell.hidden = !info.net || info.net === 'none';
    netText.textContent = info.net_conns + ' connection' + (info.net_conns === 1 ? '' : 's') +
      (info.net_blocked ? ' · ' + info.net_blocked + ' blocked' : '');
    netTraffic.textContent = info.net_received || info.net_sent
      ? '↓ ' + fmtBytes(info.net_received) + ' received  ·  ↑ ' + fmtBytes(info.net_sent) + ' sent' : '';
    netTraffic.hidden = !netTraffic.textContent;
  }
  function renderProtections() {
    clear(protBox);
    const cell = (ok, t, d) => h('div', { class: 'cell ' + (ok ? 'ok' : 'warn') }, icon(ok ? 'shield' : 'shieldOff', 'state'),
      h('div', { class: 'text' }, h('span', { class: 'k' }, t), h('span', { class: 't-foot' }, d)));
    put(protBox, h('div', { class: 't-caps group-title' }, 'Protections'), h('div', { class: 'group' },
      cell(true, 'Hardware isolation', 'Its own guest kernel under KVM'),
      cell(info.lockdown, 'Seccomp lockdown', info.lockdown ? 'The VMM can\'t open files, create sockets, exec or fork' : 'Disabled for this run'),
      cell(!!info.scope, 'Cgroup limits', info.scope ? 'Memory ≤ ' + fmtBytes(info.mem_limit) + ', no swap, 256 tasks, 1.5 CPUs' : 'No systemd user session: not applied'),
      info.net === 'allow'
        ? cell(true, 'Allow-listed network', (info.spec.allow || []).length + ' host' + ((info.spec.allow || []).length === 1 ? '' : 's') + '; everything else refused and logged')
        : info.net === 'internet'
          ? cell(false, 'Internet access', 'Public addresses only; local network and this machine blocked')
          : cell(true, 'No network', 'No network device in the guest'),
      info.spec.image
        ? cell(info.virtio, info.virtio ? 'Read-only image' : 'Image in RAM', info.virtio ? 'Writes go to a RAM layer, gone with the VM' : 'Legacy mode: writes stay in guest RAM')
        : cell(true, 'No image', 'Only the agent is in the guest'),
      info.spec.display
        ? cell(true, 'Display', 'Screen, keyboard and mouse only; the clipboard never crosses, and only this page can view it')
        : null,
      cell(true, 'Output filtered', info.filtered ? info.filtered + ' control sequence' + (info.filtered === 1 ? '' : 's') + ' removed' : 'Colors kept, control sequences removed')));
  }
  function renderResult() {
    clear(resultBox);
    if (isLive(info.status)) return;
    const code = info.exit_code;
    const cls = info.status === 'succeeded' ? 'ok' : info.status === 'timed_out' || info.status === 'stopped' ? 'warn' : 'bad';
    const msg = {
      succeeded: 'Finished successfully.',
      failed: code > 128 ? 'Killed by signal ' + (code - 128) + '.' : 'Exited with an error.',
      timed_out: 'Killed after its ' + fmtSecs(info.spec.timeout_s) + ' timeout.',
      stopped: 'You stopped this run.',
      error: 'Ksar couldn\'t complete the run.',
    }[info.status];
    const card = h('div', { class: 'group' }, h('div', { class: 'cell stacked' },
      h('div', { class: 'result' },
        info.status === 'succeeded' ? h('img', { src: '/mascot.png', alt: '' }) : null,
        h('div', {}, h('div', { class: 't-foot' }, 'Exit code'), h('div', { class: 'code ' + cls }, code === undefined ? '—' : String(code))),
        h('span', { class: 'spacer' }), h('span', { class: 't-sub', style: 'text-align:right' }, msg)),
      info.error ? h('div', { class: 'error-box' }, info.error) : null,
      info.truncated ? h('p', { class: 't-foot', style: 'margin:0' }, 'Output beyond 4 MB was not kept.') : null,
      h('button', {
        class: 'btn primary', onclick: () => {
          app.prefill = Object.assign({}, info.spec, { files: [] });
          location.hash = '#/new';
          if (info.spec.files && info.spec.files.length) toast('Files aren\'t kept after a run; add them again.');
        },
      }, icon('replay'), 'Run Again'),
      h('button', {
        class: 'btn danger', onclick: async () => {
          if (!confirm('Delete this run and its output from the history?')) return;
          try { await api('DELETE', '/api/runs/' + encodeURIComponent(info.id)); toast('Run deleted'); location.hash = '#/runs'; } catch (e) { toast(e.message, 'err'); }
        },
      }, icon('trash'), 'Delete')));
    resultBox.append(card);
    const m = motion(card); // the result materializes, it doesn't pop
    m.set({ scale: 0.96, opacity: 0 });
    m.to({ scale: 1, opacity: 1 }, { damping: 1, response: 0.35 });
  }
  function renderStdin() {
    clear(stdinBar);
    if (!info.interactive || !isLive(info.status)) return;
    const input = h('input', { class: 'input mono', placeholder: 'Type a line for stdin and press Return', 'aria-label': 'Standard input' });
    const send = async closeIt => {
      try {
        await api('POST', '/api/runs/' + id + '/stdin', { data: closeIt ? '' : input.value + '\n', close: closeIt });
        if (!closeIt) { out.write(input.value + '\n', 'meta'); input.value = ''; } else { info.interactive = false; renderStdin(); toast('stdin closed'); }
      } catch (e) { toast(e.message, 'err'); }
    };
    input.addEventListener('keydown', e => { if (e.key === 'Enter') { e.preventDefault(); send(false); } });
    stdinBar.append(h('div', { class: 'stdin' }, h('span', { class: 'prompt' }, '›'), input,
      h('button', { class: 'btn small', onclick: () => send(false) }, 'Send'),
      h('button', { class: 'btn small', onclick: () => send(true) }, 'Send EOF')));
  }
  function renderFoot() {
    clear(foot);
    put(foot, h('span', {}, 'Agent channel: ' + (info.virtio ? 'vsock' : 'serial')),
      (info.args && info.args.length)
        ? h('span', { title: JSON.stringify(info.args) }, info.args[0] + ' · ' + info.args.length + ' argument' + (info.args.length === 1 ? '' : 's'))
        : h('span', {}, info.spec.display ? 'Desktop session' : 'No command'));
  }

  let last = { status: null, filtered: -1 };
  function update(next) {
    info = next;
    if (!tabs) buildTabs(!!info.spec.display);
    // Connect once the run itself starts (after any desktop install), and
    // explain the screen once it ends.
    if (!vnc && !dispEl.hidden && info.status !== last.status) openDisplay();
    renderHero();
    renderMetrics();
    if (info.status !== last.status || info.filtered !== last.filtered || !protBox.firstChild) renderProtections();
    if (info.status !== last.status) { renderResult(); renderStdin(); renderFoot(); }
    last = { status: info.status, filtered: info.filtered };
  }

  // One line per network event. Blocked attempts stand out; nothing from
  // the guest is ever parsed as markup.
  netEl.append(h('div', { class: 'meta' }, 'Lookups and connections appear here as they happen.'));
  function netLine(e) {
    if (!netCount) clear(netEl);
    const what = e.name && e.dst ? e.name + ' (' + e.dst + ')' : e.name || e.dst || '';
    let text;
    if (e.kind === 'dns') text = !e.allowed ? 'blocked lookup  ' + what + ': ' + e.reason
      : e.answers && e.answers.length ? 'lookup  ' + what + ' → ' + e.answers.join(', ') : 'lookup  ' + what + ': ' + (e.reason || 'no answer');
    else if (e.kind === 'connect') text = e.allowed ? 'connect  ' + what : 'blocked  ' + what + ': ' + e.reason;
    else if (e.kind === 'close') text = 'closed  ' + what + '  ↑ ' + fmtBytes(e.sent || 0) + '  ↓ ' + fmtBytes(e.received || 0) + '  in ' + fmtDuration(e.ms || 0);
    else if (e.kind === 'udp') text = 'blocked UDP  ' + what + ': ' + e.reason;
    else text = e.kind + '  ' + what;
    const stick = netEl.scrollHeight - netEl.scrollTop - netEl.clientHeight < 40;
    netEl.append(h('div', { class: e.allowed ? (e.kind === 'close' ? 'meta' : null) : 'stderr' }, text));
    if (netEl.childElementCount > 5000) netEl.firstChild.remove();
    if (stick) netEl.scrollTop = netEl.scrollHeight;
    setCount('net', ++netCount);
  }

  // Live stream; EventSource resumes from the last chunk (Last-Event-ID).
  const es = new EventSource('/api/runs/' + encodeURIComponent(id) + '/events');
  onLeave(() => es.close());
  es.addEventListener('state', e => update(JSON.parse(e.data)));
  es.addEventListener('chunk', e => {
    const c = JSON.parse(e.data);
    if (c.stream === 'net') { netLine(JSON.parse(c.data)); return; }
    if (c.stream === 'console') { con.write(c.data); conBytes += c.data.length; setCount('console', fmtBytes(conBytes)); }
    else { out.write(c.data, c.stream === 'stderr' ? 'stderr' : ''); outBytes += c.data.length; setCount('output', fmtBytes(outBytes)); }
  });
  es.addEventListener('end', () => es.close());
  es.onerror = () => {
    if (info) return;
    es.close();
    clear(view);
    view.append(h('div', { class: 'empty' }, h('img', { src: MISSING, alt: '' }), h('h2', { class: 't-title' }, 'Run Not Found'),
      h('p', {}, 'Runs are kept only while ksar ui is running.'), h('a', { class: 'btn', href: '#/runs' }, icon('chevronLeft'), 'Runs')));
  };
  every(100, () => {
    if (!info || !isLive(info.status)) return;
    const t = document.getElementById('run-timer');
    if (t) t.textContent = fmtClock(runDuration(info));
  });
}

// ---------- Images ----------

function viewImages(view) {
  const refInput = h('input', { class: 'input mono', placeholder: 'alpine:3.20, python:3.12-slim…', 'aria-label': 'Docker image', style: 'flex:1' });
  const importBtn = h('button', { class: 'btn primary', onclick: doImport }, icon('download'), 'Import');
  refInput.addEventListener('keydown', e => { if (e.key === 'Enter') doImport(); });
  pageChrome(view, { title: 'Images', subtitle: 'Each image is a filesystem tar, for example a Docker export. On first use Ksar converts it to a read-only EROFS disk and caches it, so later runs start instantly.' });
  const storage = h('div', { class: 'section' });
  const list = h('div', { class: 'section' });
  const importNote = h('p', { class: 't-foot group-foot' });
  view.append(h('div', { class: 'stack', style: 'gap:1.75rem' },
    h('div', { class: 'section' }, h('div', { class: 't-caps group-title' }, 'Import from Docker'),
      h('div', { class: 'group' }, h('div', { class: 'cell' }, refInput, importBtn)), importNote),
    list, storage));

  async function doImport() {
    const ref = refInput.value.trim();
    if (!ref) { refInput.focus(); return; }
    importBtn.disabled = true;
    try {
      const r = await api('POST', '/api/images/import', { ref });
      toast('Importing ' + ref + ' as ' + r.name);
      refInput.value = '';
      load();
    } catch (e) { toast(e.message, 'err'); }
    importBtn.disabled = !app.status?.docker;
  }

  let pollImporting = false;
  async function load() {
    let images, st;
    try { [images, st] = await Promise.all([api('GET', '/api/images'), api('GET', '/api/status')]); } catch (e) { toast(e.message, 'err'); return; }
    app.status = st;
    importBtn.disabled = !st.docker;
    importNote.textContent = st.docker
      ? 'Pulls the image, exports its filesystem and keeps its environment (PATH, working directory).'
      : 'Docker isn\'t installed. You can still add images: put a root filesystem .tar into ' + st.images_dir + '.';

    clear(list);
    list.append(h('div', { class: 't-caps group-title' }, 'Library'));
    if (!images.length) {
      list.append(h('div', { class: 'group' }, h('div', { class: 'empty' }, h('img', { src: MISSING, alt: '' }), h('h2', { class: 't-title' }, 'No Images Yet'),
        h('p', {}, 'Import one from Docker above, or copy a root filesystem tar into ' + st.images_dir + '.'))));
    } else {
      const group = h('div', { class: 'group' });
      pollImporting = false;
      for (const img of images) {
        if (img.importing || img.desktop === 'preparing') pollImporting = true;
        const facts = img.importing ? 'Importing…' : img.error ? 'Import failed' :
          [fmtBytes(img.size), img.cached ? 'disk ready' : 'not converted yet', img.last_used ? 'used ' + fmtAgo(img.last_used).toLowerCase() : null].filter(Boolean).join(' · ');
        group.append(h('div', { class: 'cell' },
          h('div', { class: 'img-icon' }, icon('layers')),
          h('div', { class: 'text', style: 'flex:1' }, h('span', { class: 'k', style: 'font-weight:590' }, imageLabel(img.name)), h('span', { class: 't-foot' }, facts),
            img.importing || img.error ? null : desktopLine(img),
            img.error ? h('div', { class: 'error-box', style: 'margin-top:0.4rem' }, img.error) : null),
          img.importing ? h('span', { class: 'status running' }, h('span', { class: 'dot' }), 'Importing') : null,
          !img.importing && !img.error && (img.desktop === '' || img.desktop === 'failed') ? h('button', {
            class: 'btn small', title: 'Install a desktop now, so the first display run starts in seconds', onclick: async () => {
              try { await api('POST', '/api/images/' + encodeURIComponent(img.name) + '/desktop'); load(); } catch (e) { toast(e.message, 'err'); }
            },
          }, icon('monitor'), img.desktop === 'failed' ? 'Retry Desktop' : 'Prepare Desktop') : null,
          !img.importing && !img.error ? h('button', { class: 'btn small', onclick: () => { app.prefill = { image: img.name }; location.hash = '#/new'; } }, icon('play'), 'Run') : null,
          img.importing ? null : h('button', {
            class: 'btn small danger', 'aria-label': 'Delete ' + img.name, onclick: async () => {
              if (!img.error && !confirm('Delete ' + img.name + ' and its cached disk?')) return;
              try { await api('DELETE', '/api/images/' + encodeURIComponent(img.name)); load(); } catch (e) { toast(e.message, 'err'); }
            },
          }, img.error ? 'Dismiss' : 'Delete')));
      }
      list.append(group);
    }

    clear(storage);
    const libBytes = images.reduce((a, i) => a + (i.size || 0), 0);
    put(storage, h('div', { class: 't-caps group-title' }, 'Storage'), h('div', { class: 'group' },
      h('div', { class: 'cell' }, h('span', { class: 'k' }, 'Library'), h('span', { class: 'v' }, fmtBytes(libBytes) + ' in ' + images.length + ' image' + (images.length === 1 ? '' : 's'))),
      h('div', { class: 'cell' }, h('span', { class: 'k' }, 'Disk cache'), h('span', { class: 'v' }, fmtBytes(st.cache_bytes) + ' in ' + st.cache_images + ' disk' + (st.cache_images === 1 ? '' : 's'))),
      h('div', { class: 'cell' }, h('span', { class: 'k' }, 'Location'), h('span', { class: 'v mono' }, st.images_dir)),
      h('div', { class: 'cell' }, h('button', {
        class: 'btn small', onclick: async () => {
          try { const r = await api('POST', '/api/images/prune'); toast('Freed ' + fmtBytes(r.freed)); load(); } catch (e) { toast(e.message, 'err'); }
        },
      }, icon('trash'), 'Remove Unused Disks'))),
    h('p', { class: 't-foot group-foot' }, 'Disks are rebuilt automatically the next time an image is used.'));
  }
  load();
  every(2000, () => { if (pollImporting) load(); });
}

// An image's desktop: ready for display runs, being installed, or not yet.
function desktopLine(img) {
  const line = (cls, text) => h('span', { class: 't-foot desktop-state ' + cls }, icon('monitor'), text);
  switch (img.desktop) {
    case 'ready': return line('ok', 'Desktop ready: display runs start in seconds');
    case 'preparing': return h('div', { class: 'stack', style: 'gap:0.3rem; margin-top:0.25rem' },
      h('span', { class: 't-foot ai-status' }, h('span', { class: 'spinner' }), 'Installing a desktop, in its own VM that reaches only the distribution\'s mirrors…'),
      img.desktop_note ? h('span', { class: 'mono t-foot desktop-log' }, img.desktop_note) : null);
    case 'failed': return line('warn', 'Desktop install failed: ' + (img.desktop_note || 'unknown error'));
    default: return line('', 'No desktop yet: the first display run installs one (a minute or more), or prepare it now');
  }
}

// ---------- Settings ----------

// The built-in command assistant: a tiny model that picks how to run an
// uploaded project. Downloaded once, runs on this machine's CPU.
function assistantSection() {
  const box = h('div', { class: 'section' });
  let timer = 0;
  onLeave(() => clearTimeout(timer));
  const render = st => {
    clear(box);
    const cell = (ok, title, text) => h('div', { class: 'cell ' + (ok ? 'ok' : 'warn') }, icon(ok ? 'checkCircle' : 'alert', 'state'),
      h('div', { class: 'text' }, h('span', { class: 'k' }, title), h('span', { class: 't-foot' }, text)));
    const rows = [];
    if (st.state === 'ready') {
      rows.push(cell(true, st.model + ' installed', 'Picks the command when you drop a project. About 1–2 s per project, on the CPU; nothing leaves this machine.'),
        h('div', { class: 'cell' }, h('button', {
          class: 'btn small danger', onclick: async () => { try { render(await api('DELETE', '/api/assistant')); toast('Assistant removed'); } catch (e) { toast(e.message, 'err'); } },
        }, icon('trash'), 'Remove (' + fmtBytes(st.size) + ')')));
    } else if (st.state === 'downloading') {
      rows.push(h('div', { class: 'cell stacked' }, h('span', { class: 'k' }, 'Downloading ' + st.model + '… ' + Math.floor(100 * st.done / st.size) + '%'),
        h('div', { class: 'progress' }, h('div', { style: 'width:' + (100 * st.done / st.size).toFixed(1) + '%' }))));
      timer = setTimeout(load, 1000);
    } else {
      rows.push(cell(false, 'Not installed', st.state === 'error' ? 'The last download failed: ' + st.error : 'Projects are set up by rules only.'),
        h('div', { class: 'cell' }, h('button', {
          class: 'btn small primary', onclick: async () => { try { render(await api('POST', '/api/assistant/download')); load(); } catch (e) { toast(e.message, 'err'); } },
        }, icon('download'), 'Download ' + st.model + ' (' + fmtBytes(st.size) + ')')));
    }
    put(box, h('div', { class: 't-caps group-title' }, 'Command Assistant'), h('div', { class: 'group' }, ...rows),
      h('p', { class: 't-foot group-foot' }, 'A 135-million-parameter language model built into Ksar (no GPU, no other software). ' +
        'It only chooses among commands Ksar builds from the project\'s files, so a project can\'t talk it into anything else, and it never decides network access.'));
  };
  const load = async () => { try { render(await api('GET', '/api/assistant')); } catch { /* shown on retry */ } };
  load();
  return box;
}

// Run history: finished runs are kept on disk across restarts of ksar ui.
function historySection() {
  const box = h('div', { class: 'section' });
  const render = hi => {
    const rows = [];
    if (!hi.dir) rows.push(h('div', { class: 'cell' }, h('span', { class: 't-sub' }, 'Not saved: finished runs are forgotten when Ksar stops (started with -history "").')));
    else rows.push(h('div', { class: 'cell stacked', style: 'gap:0.15rem' }, h('span', { class: 'k' }, 'Saved in'), h('span', { class: 'mono t-foot', style: 'overflow-wrap:anywhere' }, hi.dir)));
    rows.push(h('div', { class: 'cell' }, h('span', { class: 'k' }, 'Finished runs'),
      h('span', { class: 'v' }, hi.runs + (hi.bytes ? ' · ' + fmtBytes(hi.bytes) : '') + (hi.since ? ' · since ' + fmtAgo(hi.since).toLowerCase() : ''))));
    rows.push(h('div', { class: 'cell' }, h('button', {
      class: 'btn small danger', disabled: hi.runs === 0, onclick: async () => {
        if (!confirm('Delete every finished run and its output? Running ones stay.')) return;
        try { const r = await api('DELETE', '/api/runs'); toast(r.deleted + ' run' + (r.deleted === 1 ? '' : 's') + ' deleted'); load(); } catch (e) { toast(e.message, 'err'); }
      },
    }, icon('trash'), 'Clear History')));
    put(box, h('div', { class: 't-caps group-title' }, 'Run History'), h('div', { class: 'group' }, ...rows),
      h('p', { class: 't-foot group-foot' }, 'The latest 200 runs, with their output, stay after Ksar restarts. Only your user can read them; output and environment variables may contain secrets.'));
  };
  const load = async () => { try { render(await api('GET', '/api/history')); } catch { /* shown on retry */ } };
  load();
  return box;
}

async function viewSettings(view) {
  pageChrome(view, { title: 'Settings', subtitle: 'Defaults for new runs, and what Ksar found on this machine.' });
  let st, s;
  try { [st, s] = await Promise.all([api('GET', '/api/status'), api('GET', '/api/settings')]); } catch (e) { toast(e.message, 'err'); return; }
  let memMB = s.mem_mb, timeoutS = s.timeout_s, netV = { mode: s.net || 'none', allow: s.allow || [] };
  const net = networkControl({ mode: netV.mode, allow: netV.allow, onChange: v => { netV = v; } });
  const mem = memoryPicker(s.mem_mb, v => { memMB = v; }, 256);
  const tmo = timeoutPicker(s.timeout_s, v => { timeoutS = v; }, 60);
  const check = (ok, label, detail) => h('div', { class: 'cell ' + (ok ? 'ok' : 'warn') }, icon(ok ? 'checkCircle' : 'alert', 'state'),
    h('div', { class: 'text' }, h('span', { class: 'k' }, label), h('span', { class: 't-foot' }, detail)));
  const kv = (k, v, mono) => mono
    ? h('div', { class: 'cell stacked', style: 'gap:0.15rem' }, h('span', { class: 'k' }, k), h('span', { class: 'mono t-foot', style: 'overflow-wrap:anywhere' }, v))
    : h('div', { class: 'cell' }, h('span', { class: 'k' }, k), h('span', { class: 'v' }, v));

  view.append(h('div', { class: 'split' },
    h('div', { class: 'stack', style: 'gap:1.75rem' },
      h('div', { class: 'section' }, h('div', { class: 't-caps group-title' }, 'Defaults for New Runs'),
        h('div', { class: 'group' },
          h('div', { class: 'cell stacked' }, h('span', { class: 'field-label' }, 'Memory'), mem.el),
          h('div', { class: 'cell stacked' }, h('span', { class: 'field-label' }, 'Timeout'), tmo.el),
          h('div', { class: 'cell stacked' }, h('span', { class: 'field-label' }, 'Network'), net.el),
          h('div', { class: 'cell' },
            h('button', {
              class: 'btn primary', onclick: async () => {
                try { app.settings = await api('PUT', '/api/settings', { mem_mb: memMB, timeout_s: timeoutS, net: netV.mode, allow: netV.allow }); toast('Defaults saved'); } catch (e) { toast(e.message, 'err'); }
              },
            }, 'Save'),
            h('button', { class: 'btn', onclick: () => { mem.set(256); tmo.set(60); net.set({ mode: 'none', allow: netV.allow }); } }, 'Use Secure Defaults')))),
      assistantSection(),
      historySection(),
      h('div', { class: 'section' }, h('div', { class: 't-caps group-title' }, 'How This Page Is Protected'),
        h('div', { class: 'group' },
          kv('Listening on', st.addr, true),
          h('div', { class: 'cell' }, h('span', { class: 't-sub' }, 'Only reachable from this machine, and only through the one-time link ksar ui printed, which sets a session cookie this page can\'t read.')),
          h('div', { class: 'cell' }, h('span', { class: 't-sub' }, 'Requests from other websites, or from pages on other local ports, are refused.')),
          h('div', { class: 'cell' }, h('span', { class: 't-sub' }, 'Guest output is never shown as HTML, and nothing is loaded from the internet.')),
          h('div', { class: 'cell' }, h('span', { class: 't-sub' }, 'Each run is its own ksar process, locked down with seccomp and confined to a cgroup.'))),
        h('p', { class: 't-foot group-foot' }, 'To sign in again or from another browser, restart ksar ui and open the new link.'))),
    h('div', { class: 'stack', style: 'gap:1.75rem' },
      h('div', { class: 'section' }, h('div', { class: 't-caps group-title' }, 'System'),
        h('div', { class: 'group' },
          check(st.kvm, '/dev/kvm', st.kvm ? 'Readable and writable' : 'Not accessible: add your user to the kvm group'),
          check(st.vhost_vsock, '/dev/vhost-vsock', st.vhost_vsock ? 'Available for the agent channel' : 'Missing: sudo modprobe vhost_vsock'),
          check(st.ksar_kernel, 'Guest kernel', st.kernel_version ? st.kernel_version + (st.ksar_kernel ? '' : ': not the Ksar kernel, legacy mode (make kernel)') : 'Not found'),
          check(st.mkfs_erofs, 'mkfs.erofs', st.mkfs_erofs ? 'Images can be converted to disks' : 'Install erofs-utils to convert images'),
          check(st.cgroups, 'Cgroup limits', st.cgroups ? 'systemd user session found' : 'No systemd user session: no limits'),
          check(st.docker, 'Docker', st.docker ? 'Images can be imported' : 'Optional: only needed to import images'))),
      h('div', { class: 'section' }, h('div', { class: 't-caps group-title' }, 'Storage'),
        h('div', { class: 'group' },
          kv('Kernel', st.kernel || '—', true), kv('Image library', st.images_dir, true), kv('Disk cache', st.cache_dir, true),
          kv('Cache size', fmtBytes(st.cache_bytes) + ' in ' + st.cache_images + ' disk' + (st.cache_images === 1 ? '' : 's')),
          h('div', { class: 'cell' }, h('button', {
            class: 'btn small danger', onclick: async () => {
              if (!confirm('Delete all cached image disks? They are rebuilt on next use.')) return;
              try { const r = await api('POST', '/api/cache/clear'); toast('Freed ' + fmtBytes(r.freed)); } catch (e) { toast(e.message, 'err'); }
            },
          }, icon('trash'), 'Clear Disk Cache')))))));
}

// ---------- Boot ----------

function init() {
  for (const el of document.querySelectorAll('[data-icon]')) el.replaceWith(icon(el.dataset.icon));
  api('GET', '/api/settings').then(s => { app.settings = s; }, () => { });
  window.addEventListener('hashchange', route);
  document.addEventListener('keydown', e => {
    const typing = /^(INPUT|TEXTAREA|SELECT)$/.test(document.activeElement?.tagName || '');
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'n' && !e.shiftKey) {
      e.preventDefault();
      if (location.hash !== '#/new') location.hash = '#/new';
    } else if (e.key === 'Escape' && !typing && !app.sheet && location.hash.startsWith('#/run/')) {
      location.hash = '#/runs';
    }
  });
  watchRuns();
  route();
}

init();
