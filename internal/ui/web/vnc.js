// Ksar VNC: a minimal RFB 3.8 client for the run page. It speaks to the
// run's VNC server through the UI's WebSocket bridge, asks for pixels in
// the canvas's own byte order (R, G, B, x) with Raw encoding, and sends
// keyboard and pointer input. The server never gets the clipboard.
'use strict';

const KsarVNC = (() => {
  // KeyboardEvent.key names to X keysyms; printable characters use their
  // code point (Latin-1) or 0x01000000 + code point.
  const KEYS = {
    Backspace: 0xff08, Tab: 0xff09, Enter: 0xff0d, Escape: 0xff1b, Delete: 0xffff,
    Home: 0xff50, ArrowLeft: 0xff51, ArrowUp: 0xff52, ArrowRight: 0xff53, ArrowDown: 0xff54,
    PageUp: 0xff55, PageDown: 0xff56, End: 0xff57, Insert: 0xff63, ContextMenu: 0xff67,
    Pause: 0xff13, ScrollLock: 0xff14, PrintScreen: 0xff61, NumLock: 0xff7f, CapsLock: 0xffe5,
    F1: 0xffbe, F2: 0xffbf, F3: 0xffc0, F4: 0xffc1, F5: 0xffc2, F6: 0xffc3,
    F7: 0xffc4, F8: 0xffc5, F9: 0xffc6, F10: 0xffc7, F11: 0xffc8, F12: 0xffc9,
    AltGraph: 0xfe03,
  };
  const CODES = { // modifiers by physical key, so left and right stay distinct
    ShiftLeft: 0xffe1, ShiftRight: 0xffe2, ControlLeft: 0xffe3, ControlRight: 0xffe4,
    AltLeft: 0xffe9, AltRight: 0xffea, MetaLeft: 0xffeb, MetaRight: 0xffec,
  };
  function keysym(e) {
    if (CODES[e.code]) return CODES[e.code];
    if (KEYS[e.key]) return KEYS[e.key];
    if ([...e.key].length === 1) {
      const cp = e.key.codePointAt(0);
      return cp < 0x100 ? cp : 0x01000000 + cp;
    }
    return 0;
  }

  function connect(url, canvas, onState) {
    const ctx = canvas.getContext('2d', { alpha: false });
    const ws = new WebSocket(url);
    ws.binaryType = 'arraybuffer';
    let buf = new Uint8Array(0);
    let state = 'version';
    let width = 0, height = 0;
    let closed = false;
    const down = new Map(); // code -> keysym, to release everything on blur
    let buttons = 0;
    let painted = false; // has anything but black reached the screen yet?

    const send = bytes => { if (ws.readyState === 1) ws.send(new Uint8Array(bytes)); };
    const u16 = v => [v >> 8 & 255, v & 255];
    const u32 = v => [v >>> 24 & 255, v >>> 16 & 255, v >>> 8 & 255, v & 255];
    const take = n => { const out = buf.subarray(0, n); buf = buf.subarray(n); return out; };
    const dv = (b, o) => new DataView(b.buffer, b.byteOffset + o);
    const fail = msg => { onState('error', msg); ws.close(); };

    function request(incremental) {
      send([3, incremental ? 1 : 0, ...u16(0), ...u16(0), ...u16(width), ...u16(height)]);
    }

    // Each step consumes what it can and reports whether it made progress.
    let rect = null, rects = 0;
    function step() {
      switch (state) {
        case 'version':
          if (buf.length < 12) return false;
          if (!new TextDecoder().decode(take(12)).startsWith('RFB 003.')) { fail('Not a VNC server'); return false; }
          send([...new TextEncoder().encode('RFB 003.008\n')]);
          state = 'security';
          return true;
        case 'security': {
          if (buf.length < 1) return false;
          const n = buf[0];
          if (buf.length < 1 + n) return false;
          take(1);
          const types = [...take(n)];
          if (!types.includes(1)) { fail('The display requires a password'); return false; }
          send([1]);
          state = 'result';
          return true;
        }
        case 'result':
          if (buf.length < 4) return false;
          if (dv(take(4), 0).getUint32(0) !== 0) { fail('The display refused the connection'); return false; }
          send([1]); // shared
          state = 'init';
          return true;
        case 'init': {
          if (buf.length < 24) return false;
          const nameLen = dv(buf, 20).getUint32(0);
          if (buf.length < 24 + nameLen) return false;
          const h = take(24 + nameLen);
          width = dv(h, 0).getUint16(0); height = dv(h, 2).getUint16(0);
          canvas.width = width; canvas.height = height;
          // 32 bpp, depth 24, little endian, true color, R at bit 0: bytes
          // arrive as R, G, B, x, which is the canvas's ImageData order.
          send([0, 0, 0, 0, 32, 24, 0, 1, ...u16(255), ...u16(255), ...u16(255), 0, 8, 16, 0, 0, 0]);
          send([2, 0, ...u16(1), ...u32(0)]); // encodings: Raw
          request(false);
          state = 'msg';
          onState('connected', width + '×' + height);
          return true;
        }
        case 'msg': {
          if (buf.length < 1) return false;
          const t = buf[0];
          if (t === 0) { if (buf.length < 4) return false; rects = dv(take(4), 2).getUint16(0); state = rects ? 'rect' : 'msg'; if (!rects) request(true); return true; }
          if (t === 2) { take(1); return true; } // bell
          if (t === 3) { // server clipboard: read and drop
            if (buf.length < 8) return false;
            const n = dv(buf, 4).getUint32(0);
            if (buf.length < 8 + n) return false;
            take(8 + n); return true;
          }
          fail('Unexpected message from the display'); return false;
        }
        case 'rect': {
          if (!rect) {
            if (buf.length < 12) return false;
            const h = take(12), d = dv(h, 0);
            rect = { x: d.getUint16(0), y: d.getUint16(2), w: d.getUint16(4), h: d.getUint16(6), enc: d.getInt32(8) };
            if (rect.enc !== 0 || rect.x + rect.w > width || rect.y + rect.h > height) { fail('Unsupported update from the display'); return false; }
          }
          const n = rect.w * rect.h * 4;
          if (buf.length < n) return false;
          const px = take(n);
          if (rect.w && rect.h) {
            const img = new ImageData(rect.w, rect.h);
            img.data.set(px);
            if (!painted) {
              for (let i = 0; i < n; i += 4) if (px[i] | px[i + 1] | px[i + 2]) { painted = true; onState('painted'); break; }
            }
            for (let i = 3; i < n; i += 4) img.data[i] = 255;
            ctx.putImageData(img, rect.x, rect.y);
          }
          rect = null;
          if (--rects === 0) { state = 'msg'; request(true); }
          return true;
        }
      }
      return false;
    }

    ws.onmessage = e => {
      const chunk = new Uint8Array(e.data);
      const merged = new Uint8Array(buf.length + chunk.length);
      merged.set(buf); merged.set(chunk, buf.length);
      buf = merged;
      while (!closed && step()) { /* parse everything buffered */ }
    };
    ws.onopen = () => onState('connecting', 'Connecting…');
    ws.onclose = () => { if (!closed) onState('closed', 'Display closed'); closed = true; };
    ws.onerror = () => onState('error', 'Couldn\'t connect to the display');

    // Input: coordinates are scaled from the canvas's on-screen size.
    const pos = e => {
      const r = canvas.getBoundingClientRect();
      return [Math.max(0, Math.min(width - 1, Math.round((e.clientX - r.left) * width / r.width))),
        Math.max(0, Math.min(height - 1, Math.round((e.clientY - r.top) * height / r.height)))];
    };
    const pointer = (mask, x, y) => { if (state === 'msg' || state === 'rect') send([5, mask, ...u16(x), ...u16(y)]); };
    const bit = b => (b === 0 ? 1 : b === 1 ? 2 : b === 2 ? 4 : 0);
    const onMove = e => { const [x, y] = pos(e); pointer(buttons, x, y); };
    const onDown = e => { e.preventDefault(); canvas.focus(); buttons |= bit(e.button); const [x, y] = pos(e); pointer(buttons, x, y); };
    const onUp = e => { buttons &= ~bit(e.button); const [x, y] = pos(e); pointer(buttons, x, y); };
    const onWheel = e => {
      e.preventDefault();
      const [x, y] = pos(e);
      const b = e.deltaY < 0 ? 8 : e.deltaY > 0 ? 16 : e.deltaX < 0 ? 32 : e.deltaX > 0 ? 64 : 0;
      if (b) { pointer(buttons | b, x, y); pointer(buttons, x, y); }
    };
    const key = (isDown, sym) => { if (sym && (state === 'msg' || state === 'rect')) send([4, isDown ? 1 : 0, 0, 0, ...u32(sym)]); };
    const onKeyDown = e => {
      const sym = keysym(e);
      if (!sym) return;
      e.preventDefault(); // the guest gets the key, not the browser
      down.set(e.code, sym);
      key(true, sym);
    };
    const onKeyUp = e => {
      const sym = down.get(e.code) || keysym(e);
      down.delete(e.code);
      e.preventDefault();
      key(false, sym);
    };
    const releaseAll = () => { for (const sym of down.values()) key(false, sym); down.clear(); };
    canvas.addEventListener('pointermove', onMove);
    canvas.addEventListener('pointerdown', onDown);
    canvas.addEventListener('pointerup', onUp);
    canvas.addEventListener('wheel', onWheel, { passive: false });
    canvas.addEventListener('contextmenu', e => e.preventDefault());
    canvas.addEventListener('keydown', onKeyDown);
    canvas.addEventListener('keyup', onKeyUp);
    canvas.addEventListener('blur', releaseAll);

    return {
      close() { releaseAll(); closed = true; ws.close(); },
      // Send a key combination the browser would otherwise keep, e.g.
      // Ctrl+Alt+Delete.
      combo(...syms) { for (const s of syms) key(true, s); for (const s of syms.slice().reverse()) key(false, s); },
    };
  }

  return { connect };
})();
