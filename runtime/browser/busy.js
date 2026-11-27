// What occupies the engine worker (runtime/browser, busy_js.go installs this).
//
// The engine is a Go program in a worker: every piece of its work runs inside
// Go's wasm_exec.js Go.prototype._resume(), one "slice" per JS task that
// reaches Go (a message, a timer, a promise callback). A guest's host call is
// a message to this worker, so it waits for the slice in progress. This file
// wraps _resume, times every slice and keeps the ones of MIN ms or more in a
// ring, each with
//   - its trigger: the message type (and the route of an HTTP request), the
//     event type, or "timer" (a goroutine woken by Go's own timer), and
//   - the Go sites that marked themselves while it ran (busy_js.go Mark: the
//     import being served, ...), with the time spent under each; the rest is
//     "unlabelled", and a GC that ran during the slice is counted.
// Slices shorter than MIN that follow each other closely are folded into one
// "short slices" record (see flushRun).
// Two more measurements explain a wait that no slice covers (see the block
// above __busy's export): the intervals this worker's event loop ran no timer
// ("worker not scheduled", self.__busy.lateTicks()) and the intervals a call
// message was in flight ("message in flight", self.__busy.transits()).
// The fast path (a slice shorter than MIN) allocates nothing: two clock reads,
// a few typed-array writes. Allocation happens only when a slice is recorded.
//
// Published beside self.__hostSlow: self.__busySlow (the slices of SLOW ms or
// more) and self.__busy.slices(minMs). Times are ms on the epoch clock
// (performance.timeOrigin + performance.now()) of this worker, the clock the
// engine stamps host calls with.
(function (g) {
  "use strict";
  const proto = g.Go && g.Go.prototype;
  if (g.__busy || !proto || typeof proto._resume !== "function") {
    return;
  }
  const MIN = 1;
  const SLOW = 10;
  const RING = 16384;
  const TOP = 4;
  const W = 3 + 2 * TOP + 2; // at, dur, trigger, (label, ms) x TOP, gc, gcms
  const NAMES = 1024;
  const perf = g.performance;
  const origin = perf.timeOrigin;

  const ring = new Float64Array(RING * W);
  let written = 0;

  const names = ["unlabelled", "other", "timer", "callback"];
  const ids = new Map(names.map((n, i) => [n, i]));
  function intern(name) {
    let id = ids.get(name);
    if (id === undefined) {
      if (names.length >= NAMES) {
        return 1;
      }
      id = names.length;
      names.push(name);
      ids.set(name, id);
    }
    return id;
  }

  const acc = new Float64Array(NAMES);
  const seen = new Uint8Array(NAMES);
  const touched = new Uint16Array(NAMES);
  let nTouched = 0;
  function touch(id, ms) {
    if (seen[id] === 0) {
      seen[id] = 1;
      touched[nTouched++] = id;
    }
    acc[id] += ms;
  }

  // A garbage collection the Go side saw (busy_js.go's finalizer sentinel).
  const gcn = new Int32Array(1);
  // The collector's totals as Go last wrote them (busy_js.go updateGC, after
  // every host call, and when readGC is called): [0] CPU ms spent in GC,
  // [1] cycles finished. A finalizer sentinel (gcn) runs late; these do not.
  const gcstat = new Float64Array(2);
  let readGC = null; // set by Go: refreshes gcstat

  let depth = 0;
  let cur = 0;
  let curStart = 0;

  // Go sites call this through slot[0] = id (a setter: no string, no call
  // frame on the Go side): the time since the last mark is the previous
  // label's.
  const slot = {};
  Object.defineProperty(slot, 0, {
    set(id) {
      if (depth === 0) {
        return;
      }
      const t = perf.now();
      touch(cur, t - curStart);
      cur = id >= 0 && id < NAMES ? id : 1;
      curStart = t;
    },
  });

  // The name of what started a slice, from the event Go was handed: computed
  // only for a slice that is recorded.
  function trigger(ev) {
    if (!ev) {
      return 2;
    }
    const a = ev.args && ev.args[0];
    if (a === undefined || a === null) {
      return 3;
    }
    const d = a.data;
    if (d && typeof d.t === "string") {
      if (typeof d.url === "string") {
        const u = d.url.indexOf("?");
        return intern("msg:" + d.t + " " + (d.method || "") + " " + (u < 0 ? d.url : d.url.slice(0, u)).slice(0, 80));
      }
      return intern("msg:" + d.t);
    }
    if (typeof a.type === "string") {
      return intern("event:" + a.type);
    }
    return typeof a === "number" ? intern("callback(number)") : 3;
  }

  function record(at, dur, ev, gc, gcms) {
    const row = (written % RING) * W;
    written++;
    ring[row] = at;
    ring[row + 1] = dur;
    ring[row + 2] = trigger(ev);
    for (let k = 0; k < TOP; k++) {
      let best = -1;
      for (let i = 0; i < nTouched; i++) {
        const id = touched[i];
        if (acc[id] > 0 && (best < 0 || acc[id] > acc[best])) {
          best = id;
        }
      }
      if (best < 0) {
        ring[row + 3 + 2 * k] = -1;
        ring[row + 4 + 2 * k] = 0;
        continue;
      }
      ring[row + 3 + 2 * k] = best;
      ring[row + 4 + 2 * k] = acc[best];
      acc[best] = -acc[best]; // taken; restored below
    }
    ring[row + W - 2] = gc;
    ring[row + W - 1] = gcms;
    for (let i = 0; i < nTouched; i++) {
      const id = touched[i];
      if (acc[id] < 0) {
        acc[id] = -acc[id];
      }
    }
  }

  // Slices shorter than MIN are not kept one by one, but a worker that runs
  // hundreds of them back to back is busy all the same: a wait that falls in
  // such a stretch would show "no slice" and look like a hole. Short slices that
  // follow each other within GAP ms are therefore folded into one "short
  // slices" record (at most RUNCAP ms of wall time each), kept when it spans MIN
  // ms or more, with the time actually spent in them as its one label. The
  // state is four numbers: the fast path still allocates nothing.
  const GAP = 0.5;
  const RUNCAP = 5;
  const SHORT = intern("short slices");
  let runStart = 0;
  let runEnd = 0;
  let runN = 0;
  let runSum = 0;
  function flushRun() {
    if (runN > 0 && runEnd - runStart >= MIN) {
      const row = (written % RING) * W;
      written++;
      ring[row] = origin + runStart;
      ring[row + 1] = runEnd - runStart;
      ring[row + 2] = SHORT;
      ring[row + 3] = SHORT;
      ring[row + 4] = runSum;
      for (let k = 1; k < TOP; k++) {
        ring[row + 3 + 2 * k] = -1;
        ring[row + 4 + 2 * k] = 0;
      }
      ring[row + W - 2] = 0;
      ring[row + W - 1] = 0;
    }
    runN = 0;
    runSum = 0;
  }

  // Two causes of a wait that no slice of this log can show, both MEASURED here
  // (a wait is only ever credited with the part of it a measurement overlaps):
  //
  // 1. "worker not scheduled": a probe ticks every TICK ms on this worker's own
  //    event loop; a tick that comes LATE ms or more after the previous one
  //    records the interval its loop did not run, [previous + TICK, this tick].
  //    While a Go slice ran, the loop did not run either, so these intervals
  //    overlap the slices; the attribution counts a late tick only where no
  //    slice covers (busy.go ExplainWith).
  // 2. "message in flight": a guest's call and realloc messages carry the epoch
  //    time the guest posted them ("sent", worker.js); _resume reads it when the
  //    message reaches Go and records [sent, begin of the slice] when that took
  //    MIN ms or more. The two clocks are comparable. W3C High Resolution Time,
  //    section 2.1 "Monotonic Clock": one monotonic clock serves all the contexts
  //    that can reach each other, and section 4 gives a worker the time origin
  //    of when it runs, performance.timeOrigin being that origin on the wall
  //    clock (section 7.2): timeOrigin + now() names the same instant in every
  //    worker, up to the resolution (100 us, section 4 "coarsen time") and the
  //    error of each worker's timeOrigin estimate (a few ms: worker.js removes
  //    the offset it measures before stamping "sent").
  const TICK = 2;
  const LATE = 15;
  const LATERING = 4096;
  const TRANSIT = 8192;
  const late = new Float64Array(LATERING * 2);
  let nLate = 0;
  let lastTick = perf.now();
  function tick() {
    const t = perf.now();
    if (t - lastTick >= LATE) {
      const row = (nLate % LATERING) * 2;
      nLate++;
      late[row] = origin + lastTick + TICK;
      late[row + 1] = origin + t;
    }
    lastTick = t;
  }
  const transit = new Float64Array(TRANSIT * 2);
  let nTransit = 0;
  function intervals(a, n, cap) {
    const out = [];
    for (let j = Math.max(0, n - cap); j < n; j++) {
      const row = (j % cap) * 2;
      out.push({ from: a[row], to: a[row + 1] });
    }
    return out;
  }

  const resume = proto._resume;
  proto._resume = function () {
    if (depth > 0) {
      // Go called back into Go while running: part of the slice in progress.
      depth++;
      try {
        return resume.call(this);
      } finally {
        depth--;
      }
    }
    const ev = this._pendingEvent;
    const t0 = perf.now();
    const a0 = ev && ev.args && ev.args[0];
    const d0 = a0 && a0.data;
    if (d0 && typeof d0.sent === "number" && origin + t0 - d0.sent >= MIN) {
      const row = (nTransit % TRANSIT) * 2;
      nTransit++;
      transit[row] = d0.sent;
      transit[row + 1] = origin + t0;
    }
    const gc0 = gcn[0];
    const gcms0 = gcstat[0];
    const cyc0 = gcstat[1];
    depth = 1;
    cur = 0;
    curStart = t0;
    try {
      return resume.call(this);
    } finally {
      depth = 0;
      const t1 = perf.now();
      touch(cur, t1 - curStart);
      if (t1 - t0 < MIN) {
        if (runN > 0 && t0 - runEnd <= GAP && t1 - runStart < RUNCAP) {
          runEnd = t1;
        } else {
          flushRun();
          runStart = t0;
          runEnd = t1;
        }
        runN++;
        runSum += t1 - t0;
      } else {
        flushRun();
        let gcms = 0;
        if (t1 - t0 >= SLOW && readGC) {
          // Ask Go for the collector's totals now (depth is still above 0, so
          // this call is part of the slice, not a new one): only for a slow
          // slice, the fast path stays free of it.
          try {
            readGC();
            gcms = gcstat[0] - gcms0;
          } catch (e) {
            gcms = 0;
          }
        }
        record(origin + t0, t1 - t0, ev, Math.max(gcn[0] - gc0, gcstat[1] - cyc0), gcms);
      }
      for (let i = 0; i < nTouched; i++) {
        const id = touched[i];
        acc[id] = 0;
        seen[id] = 0;
      }
      nTouched = 0;
    }
  };

  function slices(min) {
    flushRun();
    const out = [];
    const n = Math.min(written, RING);
    for (let j = written - n; j < written; j++) {
      const row = (j % RING) * W;
      if (ring[row + 1] < min) {
        continue;
      }
      const labels = [];
      for (let k = 0; k < TOP; k++) {
        const id = ring[row + 3 + 2 * k];
        if (id >= 0) {
          labels.push({ name: names[id], ms: ring[row + 4 + 2 * k] });
        }
      }
      out.push({ at: ring[row], dur: ring[row + 1], trigger: names[ring[row + 2]], labels, gc: ring[row + W - 2], gcms: ring[row + W - 1] });
    }
    return out;
  }

  g.__busy = {
    slices,
    // lateTicks: the intervals this worker's event loop ran no timer ("worker not
    // scheduled"); transits: [posted, received] of the messages that took MIN ms
    // or more to reach Go. Epoch ms, like the slices.
    lateTicks: () => intervals(late, nLate, LATERING),
    transits: () => intervals(transit, nTransit, TRANSIT),
    tick,
    label: intern,
    slot,
    gcn,
    gcstat,
    setReadGC: (f) => { readGC = f; },
    MIN,
    SLOW,
    RING,
    written: () => written,
  };
  if (typeof g.setTimeout === "function") {
    (function loop() {
      tick();
      g.setTimeout(loop, TICK);
    })();
  }
  Object.defineProperty(g, "__busySlow", { get: () => slices(SLOW), configurable: true });
})(globalThis);
