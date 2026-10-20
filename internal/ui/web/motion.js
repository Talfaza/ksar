// Ksar motion: a tiny spring engine for interruptible, velocity-aware UI
// motion, after Apple's "Designing Fluid Interfaces". No dependencies.
//
// Springs are described the way Apple does: a damping ratio (1 = no
// overshoot, lower = bouncier) and a response in seconds (how quickly the
// value gets there; not a duration). Every spring animates from its
// current value and keeps its velocity when retargeted, so any motion can
// be grabbed or reversed mid-flight without a jump or a "brick wall".
'use strict';

const KsarMotion = (() => {
  const reduceMotionQuery = matchMedia('(prefers-reduced-motion: reduce)');
  const reducedMotion = () => reduceMotionQuery.matches;

  // ---- Shared display-synced clock ----
  const active = new Set();
  let last = 0;
  let frame = 0;
  function tick(now) {
    const dt = Math.min(0.064, last ? (now - last) / 1000 : 1 / 60);
    last = now;
    for (const s of Array.from(active)) s.step(dt);
    frame = active.size ? requestAnimationFrame(tick) : 0;
    if (!frame) last = 0;
  }
  function schedule(s) {
    active.add(s);
    if (!frame) frame = requestAnimationFrame(tick);
  }

  class Spring {
    // value: start value. damping, response: Apple-style parameters.
    // precision: how close counts as "arrived" (px for positions).
    constructor({ value = 0, damping = 1, response = 0.35, precision = 0.05, onUpdate, onRest } = {}) {
      this.value = value;
      this.velocity = 0;
      this.target = value;
      this.damping = damping;
      this.response = response;
      this.precision = precision;
      this.onUpdate = onUpdate;
      this.onRest = onRest;
    }
    // Retarget. Keeps the current value and velocity unless a velocity is
    // given (velocity handoff from a gesture, in units per second).
    to(target, { velocity, damping, response } = {}) {
      this.target = target;
      if (velocity !== undefined) this.velocity = velocity;
      if (damping !== undefined) this.damping = damping;
      if (response !== undefined) this.response = response;
      if (reducedMotion() && this.instantWhenReduced) { this.jump(target); return this; }
      schedule(this);
      return this;
    }
    // Set the value immediately (direct manipulation), stopping motion.
    jump(value) {
      this.value = this.target = value;
      this.velocity = 0;
      active.delete(this);
      this.onUpdate && this.onUpdate(value);
      return this;
    }
    stop() { active.delete(this); this.velocity = 0; }
    get moving() { return active.has(this); }
    step(dt) {
      // Unit mass: stiffness from response, damping from the ratio.
      const k = Math.pow((2 * Math.PI) / this.response, 2);
      const c = (4 * Math.PI * this.damping) / this.response;
      // Fixed substeps keep stiff springs stable at any frame rate.
      const n = Math.ceil(dt / (1 / 480));
      const h = dt / n;
      for (let i = 0; i < n; i++) {
        const a = -k * (this.value - this.target) - c * this.velocity;
        this.velocity += a * h;
        this.value += this.velocity * h;
      }
      if (Math.abs(this.value - this.target) < this.precision && Math.abs(this.velocity) < this.precision * 10) {
        this.value = this.target;
        this.velocity = 0;
        active.delete(this);
        this.onUpdate && this.onUpdate(this.value);
        this.onRest && this.onRest(this.value);
        return;
      }
      this.onUpdate && this.onUpdate(this.value);
    }
  }

  // motion(el): independent springs for x, y, scale and opacity, composed
  // into transform/opacity only (compositor-friendly). X and Y are separate
  // springs so 2D motion never desyncs.
  const registry = new WeakMap();
  function motion(el) {
    if (registry.has(el)) return registry.get(el);
    const state = { x: 0, y: 0, scale: 1, opacity: 1 };
    let api;
    const apply = () => {
      el.style.transform = `translate3d(${state.x}px, ${state.y}px, 0) scale(${state.scale})`;
      el.style.opacity = String(state.opacity);
      if (api && api.onChange) api.onChange(state); // e.g. a sheet driving its scrim
    };
    const springs = {};
    for (const key of Object.keys(state)) {
      springs[key] = new Spring({
        value: state[key], precision: key === 'x' || key === 'y' ? 0.1 : 0.001,
        onUpdate: v => { state[key] = v; apply(); },
      });
      // Under reduced motion, movement and scaling happen instantly;
      // only opacity animates (a gentle cross-fade).
      springs[key].instantWhenReduced = key !== 'opacity';
    }
    api = {
      el, springs, state, onChange: null,
      set(values) { for (const [k, v] of Object.entries(values)) springs[k].jump(v); return api; },
      to(values, opts = {}) {
        let pending = 0;
        for (const [k, v] of Object.entries(values)) {
          const o = Object.assign({}, opts, { velocity: opts.velocity && typeof opts.velocity === 'object' ? opts.velocity[k] : undefined });
          if (o.velocity === undefined) delete o.velocity;
          springs[k].to(v, o);
          if (springs[k].moving) pending++;
        }
        return new Promise(resolve => {
          if (!pending) { resolve(); return; }
          for (const [k] of Object.entries(values)) {
            const s = springs[k];
            const prev = s.onRest;
            s.onRest = v => { s.onRest = prev; prev && prev(v); if (--pending === 0) resolve(); };
          }
        });
      },
    };
    registry.set(el, api);
    return api;
  }

  // Where a flick is going: Apple's projection (exponential decay, as
  // scrolling decelerates). velocity in px/s.
  function project(velocity, decelerationRate = 0.998) {
    return ((velocity / 1000) * decelerationRate) / (1 - decelerationRate);
  }

  // Soft boundary: the further past the edge, the less it follows.
  function rubberband(overshoot, dimension, constant = 0.55) {
    if (!dimension) return 0;
    return (overshoot * dimension * constant) / (dimension + constant * Math.abs(overshoot));
  }

  // Velocity from recent pointer samples (px/s), so a release hands the
  // gesture's real speed to the spring.
  class VelocityTracker {
    constructor() { this.samples = []; }
    add(x, y, t = performance.now()) {
      this.samples.push({ x, y, t });
      while (this.samples.length > 2 && t - this.samples[0].t > 100) this.samples.shift();
    }
    velocity() {
      const s = this.samples;
      if (s.length < 2) return { x: 0, y: 0 };
      const a = s[0], b = s[s.length - 1];
      const dt = (b.t - a.t) / 1000;
      if (dt <= 0) return { x: 0, y: 0 };
      return { x: (b.x - a.x) / dt, y: (b.y - a.y) / dt };
    }
  }

  return { Spring, motion, project, rubberband, VelocityTracker, reducedMotion };
})();
