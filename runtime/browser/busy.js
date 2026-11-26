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

  g.__busy = { slices, label: intern, slot, gcn, gcstat, setReadGC: (f) => { readGC = f; }, MIN, SLOW, RING, written: () => written };
  Object.defineProperty(g, "__busySlow", { get: () => slices(SLOW), configurable: true });
})(globalThis);
