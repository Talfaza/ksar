// Ksar web UI. Plain JavaScript, no dependencies, nothing loaded from
// outside. Untrusted data (guest output, file names, commands, errors) is
// only ever inserted with textContent: there is no innerHTML anywhere.
'use strict';

// ---------- DOM helpers ----------

const SVG_NS = 'http://www.w3.org/2000/svg';
const ICONS = {
  terminal: 'M4 17l6-6-6-6M12 19h8',
  plus: 'M12 5v14M5 12h14',
  layers: 'M12 2l10 5-10 5L2 7l10-5zM2 17l10 5 10-5M2 12l10 5 10-5',
  sliders: 'M4 21v-7M4 10V3M12 21v-9M12 8V3M20 21v-5M20 12V3M1 14h6M9 8h6M17 16h6',
  shield: 'M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10zM9 12l2 2 4-4',
  shieldOff: 'M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10zM12 8v5M12 16v.01',
  stop: 'M6 6h12v12H6z',
  play: 'M6 4l14 8-14 8V4z',
  trash: 'M3 6h18M8 6V4h8v2M19 6l-1 14H6L5 6M10 11v6M14 11v6',
  copy: 'M9 9h11v11H9zM5 15H4V4h11v1',
  check: 'M20 6L9 17l-5-5',
  x: 'M18 6L6 18M6 6l12 12',
  upload: 'M12 16V4M7 9l5-5 5 5M4 20h16',
  download: 'M12 4v12M7 11l5 5 5-5M4 20h16',
  clock: 'M12 22a10 10 0 1 0 0-20 10 10 0 0 0 0 20zM12 6v6l4 2',
  cpu: 'M6 6h12v12H6zM10 10h4v4h-4zM10 2v4M14 2v4M10 18v4M14 18v4M2 10h4M2 14h4M18 10h4M18 14h4',
  arrowLeft: 'M19 12H5M12 19l-7-7 7-7',
  refresh: 'M21 12a9 9 0 1 1-3-6.7L21 8M21 3v5h-5',
  lock: 'M5 11h14v10H5zM8 11V7a4 4 0 0 1 8 0v4',
  alert: 'M12 3L2 20h20L12 3zM12 10v4M12 17v.01',
  bolt: 'M13 2L4 14h7l-1 8 9-12h-7l1-8z',
  netOff: 'M2 2l20 20M8.5 16.5a5 5 0 0 1 7 0M2 8.8a15 15 0 0 1 4.2-2.7M10.7 5.1A15 15 0 0 1 22 8.8M5 12.9a10 10 0 0 1 5.2-2.7M12 20h.01',
  disk: 'M4 4h16v16H4zM4 15h16M8 18h.01',
  replay: 'M3 12a9 9 0 1 0 3-6.7L3 8M3 3v5h5',
  info: 'M12 22a10 10 0 1 0 0-20 10 10 0 0 0 0 20zM12 11v6M12 7v.01',
  wrap: 'M3 6h18M3 12h15a3 3 0 1 1 0 6h-4M16 16l-2 2 2 2M3 18h7',
};

function icon(name) {
  const span = document.createElement('span');
  span.className = 'ico';
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

// h(tag, props, ...children). Strings become text nodes. props: class,
// on* handlers, dataset, and plain properties/attributes.
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
  for (const c of children.flat()) {
    if (c === null || c === undefined || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}

function clear(el) { while (el.firstChild) el.firstChild.remove(); }

// put appends children, skipping null/undefined/false (plain append would
// insert them as the text "null").
function put(el, ...kids) {
  el.append(...kids.flat().filter(k => k !== null && k !== undefined && k !== false));
}

function toast(msg, kind) {
  const t = h('div', { class: 'toast' + (kind === 'err' ? ' err' : '') },
    icon(kind === 'err' ? 'alert' : 'check'), h('span', {}, msg));
  document.getElementById('toasts').append(t);
  setTimeout(() => t.remove(), kind === 'err' ? 6000 : 3000);
}

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
  if (ms < 1000) return ms + ' ms';
  const s = ms / 1000;
  if (s < 60) return s.toFixed(1) + ' s';
  const m = Math.floor(s / 60);
  return m + 'm ' + Math.floor(s % 60) + 's';
}
function fmtClock(ms) {
  const s = Math.max(0, ms) / 1000;
  return String(Math.floor(s / 60)).padStart(2, '0') + ':' + (s % 60).toFixed(1).padStart(4, '0');
}
function fmtAgo(iso) {
  const d = (Date.now() - new Date(iso).getTime()) / 1000;
  if (d < 5) return 'just now';
  if (d < 60) return Math.floor(d) + 's ago';
  if (d < 3600) return Math.floor(d / 60) + 'm ago';
  if (d < 86400) return Math.floor(d / 3600) + 'h ago';
  return Math.floor(d / 86400) + 'd ago';
}
const STATUS_LABEL = {
  booting: 'Booting', running: 'Running', succeeded: 'Succeeded', failed: 'Failed',
  timed_out: 'Timed out', stopped: 'Stopped', error: 'Ksar error',
};
function statusPill(status, extra) {
  return h('span', { class: 'pill ' + status }, h('span', { class: 'dot' }),
    STATUS_LABEL[status] || status, extra ? ' (' + extra + ')' : '');
}
const isLive = s => s === 'booting' || s === 'running';
function runDuration(r) {
  const end = r.ended ? new Date(r.ended).getTime() : Date.now();
  return end - new Date(r.created).getTime();
}
function imageLabel(name) { return name ? name.replace(/\.tar$/, '') : 'agent only'; }

// ---------- App state & routing ----------

const app = { status: null, settings: null, timers: [], cleanup: [], prefill: null };

function onLeave(fn) { app.cleanup.push(fn); }
function every(ms, fn) { const t = setInterval(fn, ms); onLeave(() => clearInterval(t)); }

function route() {
  for (const fn of app.cleanup.splice(0)) fn();
  const parts = (location.hash.replace(/^#\/?/, '') || 'runs').split('/');
  const main = document.getElementById('main');
  clear(main);
  const page = parts[0];
  for (const a of document.querySelectorAll('#nav a')) {
    a.classList.toggle('active', a.dataset.route === (page === 'run' ? 'runs' : page));
  }
  const views = { runs: viewRuns, new: viewNew, run: m => viewRun(m, parts[1]), images: viewImages, settings: viewSettings };
  (views[page] || viewRuns)(main);
  main.focus({ preventScroll: true });
  window.scrollTo(0, 0);
}

async function refreshStatus() {
  try {
    app.status = await api('GET', '/api/status');
  } catch { return; }
  const st = app.status;
  const box = document.getElementById('side-status');
  clear(box);
  const ready = st.kvm && st.vhost_vsock;
  box.append(
    h('div', { class: 'status-card' },
      h('div', { class: 'status-row' }, h('span', { class: ready ? 'dot-ok' : 'dot-err' }),
        h('span', {}, ready ? 'KVM ready' : 'KVM unavailable'),
        st.kernel_version ? h('span', { class: 'faint' }, '· ' + st.kernel_version.replace(/^(\d+\.\d+)\.\d+/, '$1')) : null),
      h('div', { class: 'status-row' }, icon('lock'),
        h('span', { class: 'eyebrow' }, st.lockdown ? 'seccomp lockdown on' : 'lockdown off'))),
    h('div', { class: 'status-addr' }, st.addr));
}

async function refreshActiveBadge() {
  try {
    const runs = await api('GET', '/api/runs');
    const n = runs.filter(r => isLive(r.status)).length;
    document.getElementById('nav-active').textContent = n ? n + ' active' : '';
  } catch { /* ignore */ }
}

// ---------- Runs ----------

function viewRuns(main) {
  const filterState = { tab: 'all', q: '' };
  const tbodyWrap = h('div');
  const tabs = h('div', { class: 'tabs', role: 'tablist' });
  const search = h('input', {
    class: 'input', type: 'search', placeholder: 'Filter by command, image or run id', style: 'max-width:320px',
    oninput: e => { filterState.q = e.target.value.toLowerCase(); render(); },
  });
  main.append(
    h('div', { class: 'page-head' },
      h('div', {}, h('h1', {}, 'Runs'),
        h('p', { class: 'sub' }, 'Each run boots a fresh microVM and destroys it when the command ends. Nothing it writes survives.')),
      h('a', { class: 'btn primary', href: '#/new' }, icon('plus'), 'New run')),
    h('div', { class: 'row wrap', style: 'margin-bottom:14px' }, tabs, h('span', { class: 'spacer' }), search),
    tbodyWrap);

  let runs = [];
  function render() {
    const counts = { all: runs.length, active: 0, succeeded: 0, failed: 0 };
    for (const r of runs) {
      if (isLive(r.status)) counts.active++;
      else if (r.status === 'succeeded') counts.succeeded++;
      else counts.failed++;
    }
    clear(tabs);
    for (const [id, label] of [['all', 'All'], ['active', 'Active'], ['succeeded', 'Succeeded'], ['failed', 'Failed / stopped']]) {
      tabs.append(h('button', {
        class: 'tab' + (filterState.tab === id ? ' active' : ''), role: 'tab',
        onclick: () => { filterState.tab = id; render(); },
      }, label, h('span', { class: 'count' }, String(counts[id]))));
    }
    const shown = runs.filter(r => {
      if (filterState.tab === 'active' && !isLive(r.status)) return false;
      if (filterState.tab === 'succeeded' && r.status !== 'succeeded') return false;
      if (filterState.tab === 'failed' && (isLive(r.status) || r.status === 'succeeded')) return false;
      if (!filterState.q) return true;
      return (r.spec.command + ' ' + r.spec.image + ' ' + r.id).toLowerCase().includes(filterState.q);
    });
    clear(tbodyWrap);
    if (!runs.length) {
      tbodyWrap.append(h('div', { class: 'empty' },
        h('img', { src: '/mascot.png', alt: '' }),
        h('h2', {}, 'No sandboxes yet'),
        h('p', {}, 'Every run starts clean inside its own microVM and is destroyed when it ends.'),
        h('a', { class: 'btn primary', href: '#/new' }, icon('bolt'), 'Start your first run')));
      return;
    }
    const rows = shown.map(r => {
      const exit = r.exit_code === undefined ? '--' : String(r.exit_code);
      const mem = r.mem_peak || r.mem_current;
      return h('tr', { class: 'clickable', onclick: () => { location.hash = '#/run/' + r.id; } },
        h('td', {}, h('div', { class: 'row' }, statusPill(r.status), h('span', { class: 'faint' }, '#' + r.id))),
        h('td', {}, h('span', { class: 'cmdcell' }, h('span', { style: 'color:var(--primary)' }, '$'), h('span', {}, r.spec.command))),
        h('td', {}, h('span', { class: 'code' }, imageLabel(r.spec.image))),
        h('td', { style: 'text-align:center' }, exit),
        h('td', {}, fmtDuration(runDuration(r))),
        h('td', { title: 'Peak memory of the run\'s cgroup (guest RAM in use plus ksar itself)' }, mem ? fmtBytes(mem) : '--'),
        h('td', { class: 'faint' }, fmtAgo(r.created)),
        h('td', { style: 'text-align:right' }, isLive(r.status)
          ? h('button', { class: 'btn danger sm', onclick: e => { e.stopPropagation(); stopRun(r.id); } }, icon('stop'), 'Stop')
          : h('span', { class: 'muted' }, 'Inspect →')));
    });
    tbodyWrap.append(h('div', { class: 'table-wrap' }, h('div', { class: 'table-scroll' },
      h('table', {},
        h('thead', {}, h('tr', {}, ['Status', 'Command', 'Image', 'Exit', 'Duration', 'Memory', 'Started', ''].map(t => h('th', {}, t)))),
        h('tbody', {}, rows.length ? rows : h('tr', {}, h('td', { colSpan: 8, class: 'muted' }, 'No runs match this filter.')))))));
  }
  async function load() {
    try { runs = await api('GET', '/api/runs'); render(); } catch (e) { toast(e.message, 'err'); }
  }
  load();
  every(1000, load);
}

async function stopRun(id) {
  try { await api('POST', '/api/runs/' + id + '/stop'); toast('Stopping run #' + id); } catch (e) { toast(e.message, 'err'); }
}

// ---------- Memory picker ----------

const MEM_MIN = 64;
const MEM_MAX = 3072;
const MEM_PRESETS = [
  [128, '128 MB', 'Enough for shell scripts and small tools.'],
  [256, '256 MB', 'Most command-line tools and short scripts.'],
  [512, '512 MB', 'Python, Node and package managers.'],
  [1024, '1 GB', 'Builds, test suites, larger interpreters.'],
  [2048, '2 GB', 'Compilers and bigger datasets.'],
  [3072, '3 GB', 'The maximum for one VM.'],
];

function fmtMem(mb) { return mb >= 1024 && mb % 1024 === 0 ? mb / 1024 + ' GB' : mb + ' MB'; }

// memoryPicker: preset buttons plus an exact custom size in MB. Calls
// onChange with valid values only; an out-of-range entry is shown as an
// error while typing and clamped when the field loses focus.
function memoryPicker(initial, onChange, defaultMB) {
  let value = initial;
  const presets = h('div', { class: 'seg', role: 'radiogroup', 'aria-label': 'Guest memory' });
  const input = h('input', {
    class: 'input', type: 'number', min: MEM_MIN, max: MEM_MAX, step: 1, inputMode: 'numeric',
    'aria-label': 'Guest memory in MB', 'aria-describedby': 'mem-hint',
  });
  const hint = h('p', { class: 'hint', id: 'mem-hint' });
  const err = h('p', { class: 'field-error', role: 'alert' });

  const valid = v => Number.isInteger(v) && v >= MEM_MIN && v <= MEM_MAX;
  function render(keepInput) {
    clear(presets);
    for (const [mb, label] of MEM_PRESETS) {
      const on = mb === value;
      presets.append(h('button', {
        type: 'button', class: 'seg-btn' + (on ? ' on' : ''), role: 'radio', 'aria-checked': String(on),
        tabIndex: on || (!MEM_PRESETS.some(p => p[0] === value) && mb === 256) ? 0 : -1,
        onclick: () => set(mb), onkeydown: arrowKeys,
      }, label, mb === defaultMB ? h('span', { class: 'seg-tag' }, 'default') : null));
    }
    if (!keepInput) input.value = value;
    input.classList.remove('invalid');
    err.textContent = '';
    const preset = MEM_PRESETS.find(p => p[0] === value);
    hint.textContent = (preset ? preset[2] : 'Custom size: ' + fmtMem(value) + '.') +
      ' Files the command writes are kept in this memory too.';
  }
  function set(v, keepInput) {
    value = v;
    render(keepInput);
    onChange(v);
  }
  function arrowKeys(e) {
    const i = MEM_PRESETS.findIndex(p => p[0] === value);
    let j = null;
    if (e.key === 'ArrowRight' || e.key === 'ArrowDown') j = Math.min(MEM_PRESETS.length - 1, i + 1);
    if (e.key === 'ArrowLeft' || e.key === 'ArrowUp') j = Math.max(0, i < 0 ? 0 : i - 1);
    if (j === null) return;
    e.preventDefault();
    set(MEM_PRESETS[j][0]);
    presets.children[j].focus();
  }
  input.addEventListener('input', () => {
    const v = Number(input.value);
    if (valid(v)) { set(v, true); return; }
    input.classList.add('invalid');
    err.textContent = input.value === '' ? 'Enter a size in MB.' : 'Choose between ' + MEM_MIN + ' MB and ' + fmtMem(MEM_MAX) + ', in whole MB.';
  });
  // Leaving the field (blur) or committing it (Enter, which fires
  // change) snaps an out-of-range entry to the nearest allowed size.
  const commit = () => {
    const v = Math.round(Number(input.value));
    set(Number.isFinite(v) && input.value !== '' ? Math.min(MEM_MAX, Math.max(MEM_MIN, v)) : value);
  };
  input.addEventListener('blur', commit);
  input.addEventListener('change', commit);
  const step = d => () => set(Math.min(MEM_MAX, Math.max(MEM_MIN, value + d)));

  const el = h('div', { class: 'mem-picker' },
    presets,
    h('div', { class: 'mem-custom' },
      h('span', { class: 'small muted' }, 'Exact size'),
      h('div', { class: 'stepper' },
        h('button', { type: 'button', class: 'btn sm', 'aria-label': 'Decrease by 64 MB', onclick: step(-64) }, '−'),
        input, h('span', { class: 'unit' }, 'MB'),
        h('button', { type: 'button', class: 'btn sm', 'aria-label': 'Increase by 64 MB', onclick: step(64) }, '+'))),
    err, hint);
  render();
  return { el, set, get value() { return value; } };
}

// ---------- New run ----------

async function viewNew(main) {
  const s = app.settings || (app.settings = await api('GET', '/api/settings').catch(() => ({ mem_mb: 256, timeout_s: 60 })));
  const pre = app.prefill || {};
  app.prefill = null;
  const form = {
    command: pre.command || '', image: pre.image ?? null, mem: pre.mem_mb || s.mem_mb,
    timeout: pre.timeout_s || s.timeout_s, workdir: pre.workdir || '', env: (pre.env || []).slice(),
    files: [], interactive: !!pre.interactive,
  };
  let images = [];
  try { images = (await api('GET', '/api/images')).filter(i => !i.importing && !i.error); } catch { /* none */ }
  if (form.image === null) form.image = images.length ? images[0].name : '';
  if (form.image && !images.some(i => i.name === form.image)) form.image = '';

  const summary = h('div', { class: 'stack' });
  const cmdInput = h('input', {
    value: form.command, placeholder: "sh -c 'echo hello from the sandbox'", spellcheck: false, autocomplete: 'off',
    'aria-label': 'Command', oninput: e => { form.command = e.target.value; renderSummary(); },
  });

  // Image picker
  const imageGrid = h('div', { class: 'grid-cards' });
  function renderImages() {
    clear(imageGrid);
    const choices = [{ name: '', label: 'Agent only', detail: 'No image: only the agent binary' }]
      .concat(images.map(i => ({ name: i.name, label: imageLabel(i.name), detail: fmtBytes(i.size) + (i.cached ? ' · disk ready' : ' · converted on first run') })));
    for (const c of choices) {
      imageGrid.append(h('button', {
        type: 'button', class: 'image-card' + (form.image === c.name ? ' selected' : ''), 'aria-pressed': String(form.image === c.name),
        onclick: () => { form.image = c.name; renderImages(); renderSummary(); },
      }, h('div', { class: 'row' }, icon(c.name ? 'layers' : 'terminal'), h('span', { class: 'name' }, c.label)),
      h('span', { class: 'small muted' }, c.detail)));
    }
    imageGrid.append(h('a', { class: 'image-card', href: '#/images', style: 'border-style:dashed;justify-content:center;align-items:center' },
      icon('download'), h('span', { class: 'muted' }, 'Import an image…')));
  }

  // Files
  const fileList = h('div', { class: 'stack', style: 'gap:6px' });
  const fileInput = h('input', { type: 'file', multiple: true, hidden: true, onchange: e => uploadFiles(e.target.files) });
  const drop = h('div', {
    class: 'dropzone', tabIndex: 0, role: 'button',
    onclick: () => fileInput.click(),
    onkeydown: e => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); fileInput.click(); } },
    ondragover: e => { e.preventDefault(); drop.classList.add('over'); },
    ondragleave: () => drop.classList.remove('over'),
    ondrop: e => { e.preventDefault(); drop.classList.remove('over'); uploadFiles(e.dataTransfer.files); },
  }, icon('upload'), h('span', { class: 'label' }, 'Drop files or click to add'),
  h('span', { class: 'small muted' }, 'Copied into the guest before the command starts (up to 64 MB each). The host copies stay on the host.'));
  async function uploadFiles(list) {
    for (const f of list) {
      const fd = new FormData();
      fd.append('file', f, f.name);
      try {
        const u = await api('POST', '/api/uploads', fd);
        form.files.push({ upload: u.id, name: u.name, size: u.size, path: '/work/' + u.name });
        renderFiles();
      } catch (e) { toast(f.name + ': ' + e.message, 'err'); }
    }
    fileInput.value = '';
  }
  function renderFiles() {
    clear(fileList);
    for (const f of form.files) {
      fileList.append(h('div', { class: 'file-row' }, icon('disk'),
        h('span', { class: 'name', title: f.name }, f.name),
        h('span', { class: 'faint small' }, fmtBytes(f.size) + ' →'),
        h('input', { class: 'input', value: f.path, 'aria-label': 'Guest path for ' + f.name, oninput: e => { f.path = e.target.value; } }),
        h('button', { type: 'button', class: 'btn ghost sm', 'aria-label': 'Remove ' + f.name, onclick: () => { form.files.splice(form.files.indexOf(f), 1); renderFiles(); } }, icon('x'))));
    }
    renderSummary();
  }

  // Env
  const envList = h('div', { class: 'stack', style: 'gap:6px' });
  function renderEnv() {
    clear(envList);
    form.env.forEach((kv, i) => {
      envList.append(h('div', { class: 'row' },
        h('input', { class: 'input', value: kv, placeholder: 'KEY=value', 'aria-label': 'Environment variable', oninput: e => { form.env[i] = e.target.value; } }),
        h('button', { type: 'button', class: 'btn ghost sm', 'aria-label': 'Remove variable', onclick: () => { form.env.splice(i, 1); renderEnv(); } }, icon('x'))));
    });
  }

  const memPicker = memoryPicker(form.mem, v => { form.mem = v; renderSummary(); }, s.mem_mb);
  const timeoutInput = h('input', {
    class: 'input', type: 'number', min: 1, max: 86400, value: form.timeout, 'aria-label': 'Timeout in seconds',
    oninput: e => { form.timeout = +e.target.value; renderSummary(); },
  });
  const errBox = h('div');
  const runBtn = h('button', { class: 'btn primary big', type: 'button', onclick: submit }, icon('bolt'), 'Run in sandbox', h('kbd', {}, 'Ctrl ↵'));

  function renderSummary() {
    clear(summary);
    const lines = [
      ['Hypervisor', 'KVM, 1 vCPU'],
      ['Guest kernel', app.status?.kernel_version || 'default'],
      ['Root filesystem', form.image ? imageLabel(form.image) + (app.status?.ksar_kernel ? ' (read-only EROFS + RAM overlay)' : ' (unpacked into RAM)') : 'agent only'],
      ['Network', 'none: no network device'],
      ['Memory', fmtMem(form.mem) + ' guest RAM'],
      ['Timeout', form.timeout + ' s, then the VM is killed'],
      ['Files copied in', String(form.files.length)],
      ['Afterwards', 'VM destroyed, nothing kept'],
    ];
    for (const [k, v] of lines) summary.append(h('div', { class: 'kv' }, h('span', {}, k), h('span', {}, v)));
  }

  async function submit() {
    clear(errBox);
    runBtn.disabled = true;
    try {
      const run = await api('POST', '/api/runs', {
        command: form.command, image: form.image, mem_mb: form.mem, timeout_s: form.timeout,
        env: form.env.filter(e => e.trim()), workdir: form.workdir.trim(), interactive: form.interactive,
        files: form.files.map(f => ({ upload: f.upload, name: f.name, path: f.path })),
      });
      location.hash = '#/run/' + run.id;
    } catch (e) {
      errBox.append(h('div', { class: 'error-box' }, e.message));
      runBtn.disabled = false;
    }
  }
  const keyHandler = e => { if ((e.ctrlKey || e.metaKey) && e.key === 'Enter') { e.preventDefault(); submit(); } };
  document.addEventListener('keydown', keyHandler);
  onLeave(() => document.removeEventListener('keydown', keyHandler));

  main.append(
    h('div', { class: 'page-head' }, h('div', {},
      h('div', { class: 'eyebrow' }, 'New sandbox run'),
      h('h1', {}, 'Run untrusted code'),
      h('p', { class: 'sub' }, 'Secure by default: the guest gets no network, no host files except the ones you add, and a fresh filesystem that is thrown away afterwards.'))),
    h('div', { class: 'grid-2' },
      h('div', { class: 'stack' },
        h('section', { class: 'card' },
          h('div', { class: 'card-head' }, icon('terminal'), h('h2', {}, 'Command')),
          h('label', { class: 'cmd' }, h('span', { class: 'prompt' }, '$'), cmdInput),
          h('p', { class: 'hint' }, "Split into words like a shell, without expansion. For pipes or variables, wrap it: sh -c '…'")),
        h('section', { class: 'card' },
          h('div', { class: 'card-head' }, icon('layers'), h('h2', {}, 'Root filesystem')),
          imageGrid),
        h('section', { class: 'card' },
          h('div', { class: 'card-head' }, icon('upload'), h('h2', {}, 'Files to copy in')),
          drop, fileInput, fileList),
        h('section', { class: 'card' },
          h('div', { class: 'card-head' }, icon('cpu'), h('h2', {}, 'Resources')),
          h('div', { class: 'field' }, h('span', { class: 'label' }, 'Guest memory'), memPicker.el),
          h('div', { class: 'field' }, h('span', { class: 'label' }, 'Timeout (seconds)'), timeoutInput,
            h('span', { class: 'hint' }, 'The VM is killed when it runs this long. Exit code 124.'))),
        h('section', { class: 'card' },
          h('div', { class: 'card-head' }, icon('sliders'), h('h2', {}, 'Environment')),
          h('div', { class: 'field' }, h('span', { class: 'label' }, 'Working directory'),
            h('input', { class: 'input', value: form.workdir, placeholder: '/ (default)', oninput: e => { form.workdir = e.target.value; } })),
          h('div', { class: 'field' }, h('span', { class: 'label' }, 'Variables'), envList,
            h('div', {}, h('button', { type: 'button', class: 'btn sm', onclick: () => { form.env.push(''); renderEnv(); } }, icon('plus'), 'Add variable'))),
          h('label', { class: 'check' },
            h('input', { type: 'checkbox', checked: form.interactive, onchange: e => { form.interactive = e.target.checked; } }),
            h('span', {}, h('span', { class: 'label' }, 'Interactive stdin'), h('br'),
              h('span', { class: 'hint' }, 'Type input to the command from the run page. Off: the command gets an empty stdin.')))),
        h('section', { class: 'card' },
          h('div', { class: 'card-head' }, icon('netOff'), h('h2', {}, 'Network'), h('span', { class: 'spacer' }), h('span', { class: 'pill ok' }, 'Off')),
          h('p', { class: 'muted' }, 'The guest has no network device at all, so nothing inside it can reach your network or the internet. Optional networking is planned; it will stay off by default.'))),
      h('div', { class: 'stack sticky' },
        h('section', { class: 'card raised' },
          h('div', { class: 'card-head' }, icon('shield'), h('h2', {}, 'Sandbox summary')),
          summary, errBox, runBtn,
          h('p', { class: 'small faint', style: 'text-align:center' }, 'Runs as a separate locked-down ksar process')))));
  renderImages(); renderFiles(); renderEnv(); renderSummary();
  cmdInput.focus();
}

// ---------- Terminal rendering ----------

// Term renders filtered terminal output. The server already removed every
// control sequence except SGR colors, so this only interprets colors,
// newlines, carriage returns and backspace; everything else is text.
class Term {
  constructor(el) {
    this.el = el; this.style = {}; this.lines = 0; this.newLine();
  }
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
          if (this.pendingCR) { clear(this.line); }
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
  const base = ['#6f6a66', '#ff7b72', '#45dfa4', '#f2c46d', '#79b8ff', '#d2a8ff', '#56d4dd', '#d1c5b2',
    '#8b847f', '#ffa198', '#7ee8bd', '#ffd88a', '#a5d0ff', '#e2c5ff', '#8be9ef', '#f5efe9'];
  if (n < 16) return base[n];
  if (n >= 232) { const v = 8 + (n - 232) * 10; return 'rgb(' + v + ',' + v + ',' + v + ')'; }
  n -= 16;
  const c = x => (x ? 55 + x * 40 : 0);
  return 'rgb(' + c(Math.floor(n / 36)) + ',' + c(Math.floor(n / 6) % 6) + ',' + c(n % 6) + ')';
}

// ---------- Run detail ----------

function viewRun(main, id) {
  if (!id) { location.hash = '#/runs'; return; }
  let info = null;
  let tab = 'output';
  let wrap = false;
  const outEl = h('pre', { class: 'term', tabIndex: 0, 'aria-label': 'Command output' });
  const conEl = h('pre', { class: 'term', tabIndex: 0, hidden: true, 'aria-label': 'Guest kernel log' });
  const out = new Term(outEl);
  const con = new Term(conEl);
  let outBytes = 0;
  let conBytes = 0;

  const head = h('div', { class: 'run-head' });
  const banner = h('div', { class: 'banner' });
  const side = h('div', { class: 'stack sticky' });
  const tabs = h('div', { class: 'tabs', role: 'tablist' });
  const foot = h('div', { class: 'term-foot' });
  const stdinBar = h('div');

  function renderTabs() {
    clear(tabs);
    for (const [k, label, n] of [['output', 'Output', outBytes], ['console', 'Guest kernel log', conBytes]]) {
      tabs.append(h('button', {
        class: 'tab' + (tab === k ? ' active' : ''), role: 'tab', 'aria-selected': String(tab === k),
        onclick: () => { tab = k; outEl.hidden = k !== 'output'; conEl.hidden = k !== 'console'; renderTabs(); },
      }, label, h('span', { class: 'count' }, fmtBytes(n))));
    }
  }

  main.append(head, banner,
    h('div', { class: 'grid-2' },
      h('div', { class: 'term-card' },
        h('div', { class: 'term-bar' }, tabs,
          h('div', { class: 'row' },
            h('button', { class: 'btn ghost sm', onclick: () => { wrap = !wrap; outEl.classList.toggle('wrap', wrap); conEl.classList.toggle('wrap', wrap); } }, icon('wrap'), 'Wrap'),
            h('button', {
              class: 'btn ghost sm', onclick: () => {
                navigator.clipboard.writeText((tab === 'output' ? out : con).plainText())
                  .then(() => toast('Copied to clipboard'), () => toast('Copy failed', 'err'));
              },
            }, icon('copy'), 'Copy'))),
        outEl, conEl, stdinBar, foot),
      side));
  renderTabs();

  function renderHead() {
    clear(head);
    const live = isLive(info.status);
    put(head,
      h('a', { class: 'btn sm', href: '#/runs' }, icon('arrowLeft'), 'Runs'),
      h('span', { class: 'run-id' }, '#' + info.id),
      h('span', { class: 'code', style: 'max-width:520px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap' }, info.spec.command),
      h('span', { class: 'pill neutral' }, icon('layers'), imageLabel(info.spec.image)),
      h('span', { class: 'spacer' }),
      h('span', { class: 'timer', id: 'run-timer' }, fmtClock(runDuration(info))),
      statusPill(info.status, info.exit_code !== undefined && info.status !== 'succeeded' ? info.exit_code : null),
      live ? h('button', { class: 'btn danger', onclick: () => stopRun(info.id) }, icon('stop'), 'Stop') : null);
  }

  function renderBanner() {
    clear(banner);
    const booting = info.status === 'booting';
    banner.append(h('img', { src: '/mascot.png', alt: '', class: booting ? 'booting' : '' }));
    if (booting) {
      banner.append(h('span', {}, 'Booting microVM…'));
    } else if (info.boot_ms) {
      banner.append(h('span', { class: 'row' }, icon('bolt'), 'Booted in ', h('b', { style: 'color:var(--ok)' }, fmtDuration(info.boot_ms))));
    } else {
      banner.append(h('span', { class: 'muted' }, 'The VM did not finish booting.'));
    }
    if (info.kernel) banner.append(h('span', { class: 'sep' }, '·'), h('span', {}, 'kernel ' + info.kernel));
    banner.append(h('span', { class: 'sep' }, '·'), h('span', {}, info.virtio ? 'virtio + vsock' : 'legacy (serial)'));
    if (info.cid) banner.append(h('span', { class: 'sep' }, '·'), h('span', {}, 'CID ' + info.cid));
  }

  function renderSide() {
    clear(side);
    const live = isLive(info.status);
    const memPct = info.mem_limit ? Math.min(100, 100 * (info.mem_current || 0) / info.mem_limit) : 0;
    side.append(h('section', { class: 'card raised' },
      h('div', { class: 'card-head' }, icon('cpu'), h('h2', {}, 'Live metrics'),
        h('span', { class: 'spacer' }), live ? h('span', { class: 'pill running' }, h('span', { class: 'dot' }), 'live') : null),
      h('div', { class: 'tiles' },
        h('div', { class: 'tile' }, h('span', { class: 'eyebrow' }, 'Boot'), h('span', { class: 'v' }, info.boot_ms ? fmtDuration(info.boot_ms) : '--')),
        h('div', { class: 'tile' }, h('span', { class: 'eyebrow' }, 'Duration'), h('span', { class: 'v' }, fmtDuration(runDuration(info)))),
        h('div', { class: 'tile' }, h('span', { class: 'eyebrow' }, 'vsock CID'), h('span', { class: 'v' }, info.cid ? String(info.cid) : '--')),
        h('div', { class: 'tile' }, h('span', { class: 'eyebrow' }, 'Host PID'), h('span', { class: 'v' }, info.pid ? String(info.pid) : '--'))),
      info.scope ? h('div', { class: 'field' },
        h('div', { class: 'kv' }, h('span', {}, 'Memory (cgroup)'), h('span', {}, fmtBytes(info.mem_current) + ' / ' + fmtBytes(info.mem_limit))),
        h('div', { class: 'bar big' }, h('div', { style: 'width:' + memPct.toFixed(1) + '%' })),
        h('div', { class: 'kv small' }, h('span', {}, 'Peak'), h('span', {}, fmtBytes(info.mem_peak))),
        h('div', { class: 'kv' }, h('span', {}, 'CPU'), h('span', {}, live ? info.cpu_percent.toFixed(0) + '% of one core' : '--')))
        : h('p', { class: 'small muted' }, 'Memory and CPU are measured through the run\'s cgroup, which is unavailable without a systemd user session.')));

    const prot = [];
    const add = (ok, t, d) => prot.push(h('div', { class: 'prot ' + (ok ? 'ok' : 'warn') }, icon(ok ? 'shield' : 'shieldOff'),
      h('div', {}, h('div', { class: 't' }, t), h('div', { class: 'd' }, d))));
    add(true, 'Hardware isolation', 'Own guest kernel under KVM');
    add(info.lockdown, 'Seccomp lockdown', info.lockdown ? 'The VMM process may not open files, create sockets, exec or fork' : 'Disabled for this run');
    add(!!info.scope, 'Cgroup limits', info.scope ? 'Memory capped at ' + fmtBytes(info.mem_limit) + ', no swap, 256 tasks, 1.5 CPUs' : 'No systemd user session: limits not applied');
    add(true, 'No network', 'The guest has no network device');
    if (info.spec.image) {
      add(info.virtio, info.virtio ? 'Read-only image + RAM overlay' : 'Image unpacked into RAM',
        info.virtio ? 'Writes go to a tmpfs layer, destroyed with the VM' : 'Legacy mode: writes stay in guest RAM, destroyed with the VM');
    } else {
      add(true, 'No image', 'Only the agent is in the guest');
    }
    add(true, 'Output filtered', info.filtered ? info.filtered + ' terminal control sequence(s) removed' : 'Control sequences are removed; colors are kept');
    side.append(h('section', { class: 'card raised' },
      h('div', { class: 'card-head' }, icon('shield'), h('h2', {}, 'Active protections')), prot));

    if (!live) {
      const code = info.exit_code;
      const cls = info.status === 'succeeded' ? 'ok' : info.status === 'timed_out' || info.status === 'stopped' ? 'warn' : 'bad';
      const msg = {
        succeeded: 'The command finished successfully.',
        failed: code > 128 ? 'The command was killed by signal ' + (code - 128) + '.' : 'The command exited with an error.',
        timed_out: 'The VM hit its ' + info.spec.timeout_s + ' s timeout and was killed.',
        stopped: 'You stopped this run.',
        error: 'Ksar could not complete the run.',
      }[info.status];
      side.append(h('section', { class: 'card raised' },
        h('div', { class: 'exit-big' },
          info.status === 'succeeded' ? h('img', { src: '/mascot.png', alt: '', width: 40, height: 40 }) : null,
          h('div', {}, h('div', { class: 'eyebrow' }, 'Exit code'), h('div', { class: 'exit-code ' + cls }, code === undefined ? '--' : String(code)))),
        h('p', { class: 'muted' }, msg),
        info.error ? h('div', { class: 'error-box' }, info.error) : null,
        info.truncated ? h('p', { class: 'small muted' }, 'Output beyond 4 MB was not kept.') : null,
        h('button', {
          class: 'btn primary', onclick: () => {
            app.prefill = Object.assign({}, info.spec, { files: [] });
            location.hash = '#/new';
            if (info.spec.files && info.spec.files.length) toast('Files are not kept after a run; add them again.');
          },
        }, icon('replay'), 'Run again with the same settings')));
    }
  }

  function renderStdin() {
    clear(stdinBar);
    if (!info.interactive || !isLive(info.status)) return;
    const input = h('input', { class: 'input', placeholder: 'Type a line for stdin and press Enter', 'aria-label': 'Standard input' });
    const send = async (close) => {
      try {
        await api('POST', '/api/runs/' + id + '/stdin', { data: close ? '' : input.value + '\n', close });
        if (!close) { out.write(input.value + '\n', 'meta'); input.value = ''; }
        else { toast('stdin closed'); renderStdin(); info.interactive = false; }
      } catch (e) { toast(e.message, 'err'); }
    };
    input.addEventListener('keydown', e => { if (e.key === 'Enter') { e.preventDefault(); send(false); } });
    stdinBar.append(h('div', { class: 'stdin' }, h('span', { class: 'prompt mono', style: 'color:var(--primary)' }, '›'), input,
      h('button', { class: 'btn sm', onclick: () => send(false) }, 'Send'),
      h('button', { class: 'btn sm', onclick: () => send(true) }, 'Close stdin (EOF)')));
  }

  function renderFoot() {
    clear(foot);
    foot.append(h('span', {}, 'agent channel: ' + (info.virtio ? 'vsock' : 'serial')),
      h('span', { title: JSON.stringify(info.args) }, 'argv[0] ' + info.args[0] + ' · ' + info.args.length + ' arg(s)'));
  }

  let lastStatus = null;
  function update(next) {
    info = next;
    renderHead();
    renderSide();
    if (info.status !== lastStatus) { renderBanner(); renderStdin(); renderFoot(); lastStatus = info.status; }
  }

  // Live stream. EventSource reconnects by itself and resumes from the
  // last chunk it saw (Last-Event-ID).
  const es = new EventSource('/api/runs/' + encodeURIComponent(id) + '/events');
  onLeave(() => es.close());
  es.addEventListener('state', e => update(JSON.parse(e.data)));
  es.addEventListener('chunk', e => {
    const c = JSON.parse(e.data);
    if (c.stream === 'console') { con.write(c.data); conBytes += c.data.length; }
    else { out.write(c.data, c.stream === 'stderr' ? 'stderr' : ''); outBytes += c.data.length; }
    renderTabs();
  });
  es.addEventListener('end', () => es.close());
  es.onerror = () => {
    if (!info) {
      es.close();
      clear(main);
      main.append(h('div', { class: 'empty' }, h('img', { src: '/mascot.png', alt: '' }), h('h2', {}, 'Run not found'),
        h('p', {}, 'Runs are kept only while ksar ui is running.'), h('a', { class: 'btn', href: '#/runs' }, icon('arrowLeft'), 'Back to runs')));
    }
  };
  every(100, () => {
    if (!info || !isLive(info.status)) return;
    const t = document.getElementById('run-timer');
    if (t) t.textContent = fmtClock(runDuration(info));
  });
}

// ---------- Images ----------

function viewImages(main) {
  const usage = h('section', { class: 'card' });
  const list = h('div', { class: 'stack', style: 'gap:8px' });
  const refInput = h('input', { class: 'input', placeholder: 'alpine:3.20, python:3.12-slim, node:22-alpine…', 'aria-label': 'Docker image', style: 'max-width:360px' });
  const importBtn = h('button', { class: 'btn primary', onclick: doImport }, icon('download'), 'Import from Docker');
  refInput.addEventListener('keydown', e => { if (e.key === 'Enter') doImport(); });

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
    importBtn.disabled = false;
  }

  main.append(
    h('div', { class: 'page-head' },
      h('div', {}, h('div', { class: 'eyebrow' }, 'Root filesystems'), h('h1', {}, 'Images'),
        h('p', { class: 'sub' }, 'Each image is a filesystem tar, for example a Docker export. On first use Ksar converts it to a read-only EROFS disk and caches it, so later runs start instantly.')),
      h('div', { class: 'row wrap' }, refInput, importBtn)),
    usage, h('div', { style: 'height:16px' }), list);

  let pollImporting = false;
  async function load() {
    let images, st;
    try { [images, st] = await Promise.all([api('GET', '/api/images'), api('GET', '/api/status')]); } catch (e) { toast(e.message, 'err'); return; }
    app.status = st;
    importBtn.disabled = !st.docker;
    importBtn.title = st.docker ? '' : 'Docker is not installed';
    clear(usage);
    const libBytes = images.reduce((a, i) => a + (i.size || 0), 0);
    put(usage,
      h('div', { class: 'card-head' }, icon('disk'), h('h2', {}, 'Storage'), h('span', { class: 'spacer' }),
        h('button', {
          class: 'btn sm', onclick: async () => {
            try { const r = await api('POST', '/api/images/prune'); toast('Freed ' + fmtBytes(r.freed)); load(); } catch (e) { toast(e.message, 'err'); }
          },
        }, icon('trash'), 'Prune unused disks')),
      h('div', { class: 'tiles' },
        h('div', { class: 'tile' }, h('span', { class: 'eyebrow' }, 'Library (tars)'), h('span', { class: 'v' }, fmtBytes(libBytes) + ' · ' + images.length + ' image(s)')),
        h('div', { class: 'tile' }, h('span', { class: 'eyebrow' }, 'Disk cache (EROFS)'), h('span', { class: 'v' }, fmtBytes(st.cache_bytes) + ' · ' + st.cache_images + ' disk(s)'))),
      h('div', { class: 'kv small' }, h('span', {}, 'Library'), h('span', {}, st.images_dir)),
      st.docker ? null : h('div', { class: 'note' }, icon('info'), h('span', {}, 'Docker is not installed. You can still add images: put any root filesystem .tar into the library directory.')));

    clear(list);
    if (!images.length) {
      list.append(h('div', { class: 'empty' }, h('img', { src: '/mascot.png', alt: '' }), h('h2', {}, 'No images yet'),
        h('p', {}, 'Import one from Docker above, or copy a root filesystem tar into ' + st.images_dir + '. Without an image, runs contain only the agent.')));
    }
    pollImporting = false;
    for (const img of images) {
      if (img.importing) pollImporting = true;
      const facts = [];
      if (img.importing) facts.push('importing…');
      else if (img.error) facts.push('import failed');
      else {
        facts.push(fmtBytes(img.size) + ' tar');
        facts.push(img.cached ? 'EROFS disk ready (' + fmtBytes(img.cache_size) + ')' : 'not converted yet');
        if (img.last_used) facts.push('last used ' + fmtAgo(img.last_used));
      }
      list.append(h('div', { class: 'img-row' },
        h('div', { class: 'img-ico' }, icon('layers')),
        h('div', { class: 'img-meta' },
          h('div', { class: 'row wrap' }, h('span', { class: 'name' }, imageLabel(img.name)),
            img.cached ? h('span', { class: 'pill ok' }, 'cached') : null,
            img.importing ? h('span', { class: 'pill running' }, h('span', { class: 'dot' }), 'importing') : null,
            img.error ? h('span', { class: 'pill error' }, 'failed') : null),
          h('div', { class: 'facts' }, facts.map(f => h('span', {}, f))),
          img.error ? h('div', { class: 'error-box' }, img.error) : null),
        h('div', { class: 'row' },
          !img.importing && !img.error ? h('button', {
            class: 'btn primary sm', onclick: () => { app.prefill = { image: img.name }; location.hash = '#/new'; },
          }, icon('play'), 'Run') : null,
          img.importing ? null : h('button', {
            class: 'btn danger sm', 'aria-label': 'Delete ' + img.name, onclick: async () => {
              if (!img.error && !confirm('Delete ' + img.name + ' and its cached disk?')) return;
              try { await api('DELETE', '/api/images/' + encodeURIComponent(img.name)); load(); } catch (e) { toast(e.message, 'err'); }
            },
          }, icon('trash'), img.error ? 'Dismiss' : 'Delete'))));
    }
  }
  load();
  every(2000, () => { if (pollImporting) load(); });
}

// ---------- Settings ----------

async function viewSettings(main) {
  let st, s;
  try { [st, s] = await Promise.all([api('GET', '/api/status'), api('GET', '/api/settings')]); } catch (e) { toast(e.message, 'err'); return; }
  let memMB = s.mem_mb;
  const mem = memoryPicker(s.mem_mb, v => { memMB = v; }, 256);
  const timeout = h('input', { class: 'input', type: 'number', min: 1, max: 86400, value: s.timeout_s });
  const check = (ok, label, detail) => h('div', { class: 'prot ' + (ok ? 'ok' : 'warn') }, icon(ok ? 'check' : 'alert'),
    h('div', {}, h('div', { class: 't' }, label), h('div', { class: 'd' }, detail)));

  main.append(
    h('div', { class: 'page-head' }, h('div', {}, h('div', { class: 'eyebrow' }, 'This machine'), h('h1', {}, 'Settings & security'),
      h('p', { class: 'sub' }, 'Defaults for new runs, and what Ksar found on this machine.'))),
    h('div', { class: 'grid-2' },
      h('div', { class: 'stack' },
        h('section', { class: 'card' },
          h('div', { class: 'card-head' }, icon('sliders'), h('h2', {}, 'Defaults for new runs')),
          h('div', { class: 'field' }, h('span', { class: 'label' }, 'Guest memory'), mem.el),
          h('label', { class: 'field', style: 'max-width:260px' }, h('span', { class: 'label' }, 'Timeout (seconds)'), timeout),
          h('div', { class: 'row' },
            h('button', {
              class: 'btn primary', onclick: async () => {
                try {
                  app.settings = await api('PUT', '/api/settings', { mem_mb: memMB, timeout_s: +timeout.value });
                  toast('Defaults saved');
                } catch (e) { toast(e.message, 'err'); }
              },
            }, icon('check'), 'Save'),
            h('button', { class: 'btn', onclick: () => { mem.set(256); timeout.value = 60; } }, 'Reset to secure defaults'))),
        h('section', { class: 'card' },
          h('div', { class: 'card-head' }, icon('lock'), h('h2', {}, 'How this page is protected')),
          h('div', { class: 'kv' }, h('span', {}, 'Listening on'), h('span', {}, st.addr)),
          h('ul', { class: 'muted', style: 'margin:0;padding-left:18px;display:flex;flex-direction:column;gap:4px' },
            h('li', {}, 'Only reachable from this machine (loopback address).'),
            h('li', {}, 'Requires the one-time link printed by ksar ui; it sets a session cookie this page can\'t read.'),
            h('li', {}, 'Refuses requests from other websites and from pages on other local ports.'),
            h('li', {}, 'Never shows guest output as HTML, and loads nothing from the internet.'),
            h('li', {}, 'Each run is its own ksar process, locked down with seccomp and confined to a cgroup.')),
          h('p', { class: 'small faint' }, 'To sign in again or from another browser, restart ksar ui and open the new link.'))),
      h('div', { class: 'stack' },
        h('section', { class: 'card' },
          h('div', { class: 'card-head' }, icon('cpu'), h('h2', {}, 'System checks')),
          check(st.kvm, '/dev/kvm', st.kvm ? 'Readable and writable' : 'Not accessible: add your user to the kvm group'),
          check(st.vhost_vsock, '/dev/vhost-vsock', st.vhost_vsock ? 'Available for the agent channel' : 'Missing: load the vhost_vsock module (sudo modprobe vhost_vsock)'),
          check(st.ksar_kernel, 'Guest kernel', st.kernel_version ? st.kernel_version + (st.ksar_kernel ? '' : ' (not the Ksar kernel: legacy mode, run make kernel)') : 'Not found'),
          check(st.mkfs_erofs, 'mkfs.erofs', st.mkfs_erofs ? 'Images can be converted to disks' : 'Install erofs-utils to convert images'),
          check(st.cgroups, 'Cgroup limits', st.cgroups ? 'systemd user session found' : 'No systemd user session: runs have no cgroup limits'),
          check(st.docker, 'Docker', st.docker ? 'Images can be imported' : 'Optional: needed only to import images')),
        h('section', { class: 'card' },
          h('div', { class: 'card-head' }, icon('disk'), h('h2', {}, 'Storage')),
          h('div', { class: 'kv' }, h('span', {}, 'Kernel'), h('span', {}, st.kernel || '--')),
          h('div', { class: 'kv' }, h('span', {}, 'Image library'), h('span', {}, st.images_dir)),
          h('div', { class: 'kv' }, h('span', {}, 'Disk cache'), h('span', {}, st.cache_dir)),
          h('div', { class: 'kv' }, h('span', {}, 'Cache size'), h('span', {}, fmtBytes(st.cache_bytes) + ' in ' + st.cache_images + ' disk(s)')),
          h('button', {
            class: 'btn danger', onclick: async () => {
              if (!confirm('Delete all cached image disks? They are rebuilt on next use.')) return;
              try { const r = await api('POST', '/api/cache/clear'); toast('Freed ' + fmtBytes(r.freed)); } catch (e) { toast(e.message, 'err'); }
            },
          }, icon('trash'), 'Clear disk cache')))));
}

// ---------- Boot ----------

function init() {
  for (const el of document.querySelectorAll('[data-icon]')) el.replaceWith(icon(el.dataset.icon));
  window.addEventListener('hashchange', route);
  document.addEventListener('keydown', e => {
    const typing = /^(INPUT|TEXTAREA|SELECT)$/.test(document.activeElement?.tagName || '');
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'n' && !e.shiftKey) {
      e.preventDefault();
      location.hash = '#/new';
    } else if (e.key === 'Escape' && !typing && location.hash.startsWith('#/run/')) {
      location.hash = '#/runs';
    }
  });
  refreshStatus();
  refreshActiveBadge();
  setInterval(refreshActiveBadge, 3000);
  setInterval(refreshStatus, 15000);
  route();
}

init();
