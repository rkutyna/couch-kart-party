// Party controller. Flow: Join -> lobby (pick character, vehicle, course vote,
// READY) -> "get ready" while the navigator drives the emulator -> video and
// controls. The WebRTC connection is set up at Join, so video is ready when
// the navigator shows it. Controller input goes over the unordered "input"
// data channel; a reliable "ping" channel measures round trip time; network
// stats go to POST /api/stats once a second. Lobby state arrives by long-poll.
(() => {
  'use strict';

  const SEND_HZ = 60;
  const DEAD_DEG = 2;                 // tilt dead zone
  const SENS_CHOICES = [25, 35, 45];  // tilt angle that gives full lock
  const DEFAULT_SENS = 35;
  const MOTION_WAIT_MS = 1500;        // no motion events this long after enabling: use slider
  const TILT_STALE_MS = 1000;         // motion events older than this count as no signal
  const RECENTRE_DELAY_MS = 400;      // wait for the phone to settle after a rotation
  const WEIGHTS = ['light', 'medium', 'heavy'];

  const $ = (id) => document.getElementById(id);
  const video = $('video');
  const statusEl = $('status');
  const netOverlay = $('netOverlay');
  const overlay = $('overlay');
  const joinBtn = $('joinBtn');
  const joinMsg = $('joinMsg');
  const httpsHint = $('httpsHint');
  const httpsLink = $('httpsLink');
  const lobbyEl = $('lobby');
  const lobbyMsg = $('lobbyMsg');
  const nameIn = $('nameIn');
  const charGrid = $('charGrid');
  const vehGrid = $('vehGrid');
  const driftAuto = $('driftAuto');
  const driftManual = $('driftManual');
  const cupTabs = $('cupTabs');
  const courseGrid = $('courseGrid');
  const playerList = $('playerList');
  const readyBtn = $('readyBtn');
  const startBtn = $('startBtn');
  const lobbyHint = $('lobbyHint');
  const startingEl = $('starting');
  const waitingEl = $('waiting');
  const waitTitle = $('waitTitle');
  const waitMsg = $('waitMsg');
  const startStatus = $('startStatus');
  const startMsg = $('startMsg');
  const controls = $('controls');
  const steerStrip = $('steerStrip');
  const steerKnob = $('steerKnob');
  const steerToggle = $('steerToggle');
  const recentreBtn = $('recentre');
  const sensBtn = $('sensBtn');
  const menuToggle = $('menuToggle');
  const menuPad = $('menuPad');
  const lobbyBtn = $('lobbyBtn');
  const voteOverlay = $('voteOverlay');
  const voteCountdown = $('voteCountdown');
  const voteRandom = $('voteRandom');
  const voteLock = $('voteLock');
  const voteStatus = $('voteStatus');
  const voteChange = $('voteChange');
  const voteTabs = $('voteTabs');
  const voteCourses = $('voteCourses');

  // ---------- small helpers ----------
  const clamp = (v, lo, hi) => Math.max(lo, Math.min(hi, v));
  const round3 = (v) => Math.round(v * 1000) / 1000;
  const r1 = (v) => (typeof v === 'number' && isFinite(v) ? Math.round(v * 10) / 10 : null);
  const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

  function el(tag, cls, text) {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text != null) e.textContent = text;
    return e;
  }
  function storeGet(key) {
    try { return localStorage.getItem(key); } catch (e) { return null; }
  }
  function storeSet(key, value) {
    try { localStorage.setItem(key, value); } catch (e) { /* private mode: fine */ }
  }
  function randomId() {
    const b = new Uint8Array(16);
    if (window.crypto && crypto.getRandomValues) crypto.getRandomValues(b);
    else for (let i = 0; i < b.length; i++) b[i] = Math.floor(Math.random() * 256);
    return Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('');
  }
  function loadClientId() {
    let id = storeGet('party.clientId');
    if (!id || id.length < 8) {
      id = randomId();
      storeSet('party.clientId', id);
    }
    return id;
  }
  const clientId = loadClientId();

  // ---------- orientation ----------
  function updateOrientation() {
    document.body.classList.toggle('portrait', window.innerHeight > window.innerWidth);
  }
  window.addEventListener('resize', updateOrientation);
  updateOrientation();

  // ---------- buttons ----------
  // Several controls can drive the same bit (TRICK and menu up both send D_UP),
  // so each bit is a counter of pointers holding it.
  const counts = {};
  const ptrs = new Map(); // pointerId -> { bit, el }
  let mask = 0;

  function recalcMask() {
    let m = 0;
    for (const k in counts) if (counts[k] > 0) m |= Number(k);
    if (m !== mask) {
      mask = m;
      sendNow();
    }
  }
  function pressPtr(bit, el, pid) {
    ptrs.set(pid, { bit, el });
    counts[bit] = (counts[bit] || 0) + 1;
    el.classList.add('on');
    recalcMask();
  }
  function releasePtr(pid) {
    const p = ptrs.get(pid);
    if (!p) return;
    ptrs.delete(pid);
    counts[p.bit] = Math.max(0, (counts[p.bit] || 1) - 1);
    let stillOn = false;
    for (const v of ptrs.values()) if (v.el === p.el) { stillOn = true; break; }
    if (!stillOn) p.el.classList.remove('on');
    recalcMask();
  }
  function bindHold(el, bit) {
    el.addEventListener('pointerdown', (e) => {
      e.preventDefault();
      try { el.setPointerCapture(e.pointerId); } catch (err) { /* ignore */ }
      pressPtr(bit, el, e.pointerId);
    });
    const end = (e) => releasePtr(e.pointerId);
    el.addEventListener('pointerup', end);
    el.addEventListener('pointercancel', end);
    el.addEventListener('lostpointercapture', end);
  }
  document.querySelectorAll('[data-bit]').forEach((el) => bindHold(el, Number(el.dataset.bit)));

  // ---------- steering: slider ----------
  let sliderX = 0;
  let sliderPtr = null;
  function setSliderFromEvent(e) {
    const r = steerStrip.getBoundingClientRect();
    sliderX = clamp(((e.clientX - r.left) / r.width) * 2 - 1, -1, 1);
    steerKnob.style.left = ((sliderX + 1) / 2) * 100 + '%';
  }
  steerStrip.addEventListener('pointerdown', (e) => {
    e.preventDefault();
    sliderPtr = e.pointerId;
    try { steerStrip.setPointerCapture(e.pointerId); } catch (err) { /* ignore */ }
    setSliderFromEvent(e);
    sendNow();
  });
  steerStrip.addEventListener('pointermove', (e) => {
    if (e.pointerId !== sliderPtr) return;
    setSliderFromEvent(e);
    sendNow();
  });
  const sliderEnd = (e) => {
    if (e.pointerId !== sliderPtr) return;
    sliderPtr = null;
    sliderX = 0;
    steerKnob.style.left = '50%';
    sendNow();
  };
  steerStrip.addEventListener('pointerup', sliderEnd);
  steerStrip.addEventListener('pointercancel', sliderEnd);
  steerStrip.addEventListener('lostpointercapture', sliderEnd);

  // ---------- steering: tilt ----------
  // The wheel angle is the signed angle between the neutral gravity direction
  // (captured at join, CENTRE, or a rotation) and the current one, both in the
  // device's own plane. A clockwise turn as seen from the screen is positive
  // (steer right). The angle does not depend on which landscape the phone is
  // in, so no screen.orientation correction is needed. Re-capturing the neutral
  // after a rotation keeps it valid if a browser reports values in screen axes.
  let sensDeg = DEFAULT_SENS;
  {
    const saved = Number(storeGet('party.sensDeg'));
    if (SENS_CHOICES.includes(saved)) sensDeg = saved;
  }
  function showSens() { sensBtn.textContent = 'SENS ' + sensDeg; }
  showSens();
  sensBtn.addEventListener('click', () => {
    const i = SENS_CHOICES.indexOf(sensDeg);
    sensDeg = SENS_CHOICES[(i + 1) % SENS_CHOICES.length];
    storeSet('party.sensDeg', String(sensDeg));
    showSens();
    updateSteerUI();
  });

  const tilt = {
    neutral: null,
    recapture: true,
    recaptureAfter: 0,
    lpx: null,
    lpy: null,
    lastEvent: 0,
    gotEvent: false,
    listenStart: 0,
    x: 0,
    lastSentX: 0,
  };
  let motionListening = false;
  let motionDenied = false;

  function onMotion(e) {
    const a = e.accelerationIncludingGravity;
    if (!a || typeof a.x !== 'number' || typeof a.y !== 'number') return;
    const now = performance.now();
    tilt.lastEvent = now;
    tilt.gotEvent = true;
    const k = 0.35; // low-pass: smooths hand jitter
    tilt.lpx = tilt.lpx == null ? a.x : tilt.lpx + k * (a.x - tilt.lpx);
    tilt.lpy = tilt.lpy == null ? a.y : tilt.lpy + k * (a.y - tilt.lpy);
    if (tilt.neutral == null || (tilt.recapture && now >= tilt.recaptureAfter)) {
      tilt.neutral = { x: tilt.lpx, y: tilt.lpy };
      tilt.recapture = false;
    }
    const nx = tilt.neutral.x, ny = tilt.neutral.y;
    const cx = tilt.lpx, cy = tilt.lpy;
    let x = 0;
    if (Math.hypot(nx, ny) > 3 && Math.hypot(cx, cy) > 3) {
      // Signed angle from neutral to current, positive = turned clockwise (right).
      const deg = Math.atan2(nx * cy - ny * cx, nx * cx + ny * cy) * 180 / Math.PI;
      const mag = Math.abs(deg);
      if (mag > DEAD_DEG) {
        x = Math.sign(deg) * Math.min(1, (mag - DEAD_DEG) / (sensDeg - DEAD_DEG));
      }
    }
    tilt.x = x;
    if (Math.abs(x - tilt.lastSentX) > 0.02) {
      tilt.lastSentX = x;
      sendNow();
    }
  }

  // Re-capture the neutral pose on the next motion event after this delay.
  function recentreTilt(delayMs) {
    tilt.recapture = true;
    tilt.recaptureAfter = performance.now() + delayMs;
  }
  window.addEventListener('orientationchange', () => recentreTilt(RECENTRE_DELAY_MS));
  if (window.screen && screen.orientation && screen.orientation.addEventListener) {
    screen.orientation.addEventListener('change', () => recentreTilt(RECENTRE_DELAY_MS));
  }

  function tiltFresh() {
    return motionListening && tilt.gotEvent && performance.now() - tilt.lastEvent < TILT_STALE_MS;
  }
  function tiltNoEvents() {
    return motionListening && !tilt.gotEvent && performance.now() - tilt.listenStart > MOTION_WAIT_MS;
  }

  // Must be called straight from a tap (iOS requires a user gesture). The
  // permission request is the first thing that runs, before any await.
  async function ensureMotion() {
    if (motionListening) return true;
    if (!window.isSecureContext || typeof window.DeviceMotionEvent === 'undefined') return false;
    try {
      let perm = null;
      if (typeof DeviceOrientationEvent !== 'undefined' && typeof DeviceOrientationEvent.requestPermission === 'function') {
        perm = await DeviceOrientationEvent.requestPermission();
      } else if (typeof DeviceMotionEvent.requestPermission === 'function') {
        perm = await DeviceMotionEvent.requestPermission();
      }
      if (perm && perm !== 'granted') { motionDenied = true; return false; }
    } catch (err) {
      motionDenied = true;
      return false;
    }
    window.addEventListener('devicemotion', onMotion);
    motionListening = true;
    tilt.listenStart = performance.now();
    recentreTilt(0);
    return true;
  }

  // ---------- steering mode ----------
  let steerMode = 'slider'; // 'tilt' | 'slider'

  function currentSteer() {
    if (steerMode === 'tilt' && tiltFresh()) return tilt.x;
    return sliderX;
  }

  function steerLabel() {
    if (steerMode === 'tilt') {
      if (!window.isSecureContext) return 'SLIDER (tilt needs https)';
      if (motionDenied) return 'SLIDER (motion denied)';
      if (tiltNoEvents()) return 'SLIDER (no motion events)';
      if (!tiltFresh()) return 'TILT (no signal, slider)';
      return 'TILT ON';
    }
    return 'SLIDER';
  }

  function updateSteerUI() {
    steerToggle.textContent = 'STEER: ' + steerLabel();
    const active = steerMode === 'tilt' && tiltFresh();
    steerStrip.classList.toggle('inactive', active);
    steerStrip.parentElement.classList.toggle('tiltOn', active);
    updateStatus();
  }
  setInterval(updateSteerUI, 500);

  steerToggle.addEventListener('click', async () => {
    if (steerMode === 'tilt') {
      steerMode = 'slider';
    } else {
      steerMode = 'tilt';
      await ensureMotion();
    }
    updateSteerUI();
  });
  recentreBtn.addEventListener('click', () => {
    recentreTilt(0);
    sliderX = 0;
    steerKnob.style.left = '50%';
    sendNow();
  });
  menuToggle.addEventListener('click', () => {
    menuPad.hidden = !menuPad.hidden;
  });

  // ---------- data channel send loop ----------
  let seq = 0;
  let dc = null;
  let pingDc = null;
  let inputSent = 0; // messages sent since the last stats sample

  function sendNow() {
    if (!dc || dc.readyState !== 'open') return;
    seq += 1;
    const msg = { s: seq, x: round3(currentSteer()), y: 0, b: mask };
    try {
      dc.send(JSON.stringify(msg));
      inputSent += 1;
    } catch (e) { /* channel closing */ }
  }
  setInterval(sendNow, Math.round(1000 / SEND_HZ));

  // ---------- ping (input round trip) ----------
  let pingMs = null;
  function pingTick() {
    if (pingDc && pingDc.readyState === 'open') {
      try { pingDc.send(JSON.stringify({ t: performance.now() })); } catch (e) { /* closing */ }
    }
  }
  function onPong(e) {
    try {
      const t = JSON.parse(e.data).t;
      if (typeof t === 'number') pingMs = Math.round(performance.now() - t);
    } catch (err) { /* ignore */ }
  }
  setInterval(pingTick, 1000);

  // ---------- WebRTC ----------
  let pc = null;
  let slot = 0;
  let joining = false;
  let joinedOnce = false;
  let rejoinTimer = null;
  let discTimer = null;
  let attempts = 0;
  let lastSlot = Number(storeGet('party.lastSlot')) || 0;

  // Video stall watchdog state (see "video stall watchdog" below).
  const STALL_MS = 4000;             // no new frames this long while connected: rebuild
  const STALL_GRACE_MS = 5000;       // no checks this long after connect
  const STALL_STREAK_MS = 30000;     // a stall this soon after the last one backs off further
  const STALL_MAX_DELAY_MS = 8000;
  const STALL_BRAKE_COUNT = 3;       // this many stall reconnects ...
  const STALL_BRAKE_WINDOW_MS = 120000; // ... within this long ...
  const STALL_BRAKE_MS = 120000;     // ... stop auto-reconnecting for stalls this long
  const stall = {
    quality: null,      // video element totalVideoFrames at the last tick
    decoded: null,      // inbound-rtp framesDecoded at the last tick
    packets: null,      // inbound-rtp packetsReceived at the last tick
    bytes: null,        // inbound-rtp bytesReceived at the last tick
    progressAt: 0,      // performance.now() of the last progress (or the last reset)
    connectedAt: 0,     // performance.now() of the last connect
    streak: 0,          // stall reconnects in a row
    lastAt: -Infinity,  // performance.now() of the last stall reconnect
    reconnects: [],     // performance.now() of recent stall reconnects (for the brake)
    brakeUntil: -Infinity, // performance.now() until which stalls do not reconnect
    busy: false,        // a watchdog getStats call is in flight
  };

  class RoomFull extends Error {}
  class GameInProgress extends Error {}

  function waitIce(p) {
    return new Promise((resolve) => {
      if (p.iceGatheringState === 'complete') return resolve();
      let timer = null;
      const done = () => {
        if (p.iceGatheringState !== 'complete') return;
        p.removeEventListener('icegatheringstatechange', done);
        clearTimeout(timer);
        resolve();
      };
      timer = setTimeout(() => {
        p.removeEventListener('icegatheringstatechange', done);
        resolve();
      }, 4000);
      p.addEventListener('icegatheringstatechange', done);
    });
  }

  function lowLatency(receiver) {
    try { if ('jitterBufferTarget' in receiver) receiver.jitterBufferTarget = 0; } catch (e) { /* ignore */ }
    try { if ('playoutDelayHint' in receiver) receiver.playoutDelayHint = 0; } catch (e) { /* ignore */ }
  }

  function teardown() {
    clearTimeout(discTimer);
    discTimer = null;
    if (pc) {
      const p = pc;
      pc = null;
      p.onconnectionstatechange = null;
      p.ontrack = null;
      try { p.close(); } catch (e) { /* ignore */ }
    }
    dc = null;
    pingDc = null;
  }

  // delayOverride (ms) replaces the normal backoff; the stall watchdog uses it.
  function scheduleRejoin(delayOverride) {
    if (rejoinTimer) return;
    teardown();
    attempts += 1;
    const delay = delayOverride != null ? delayOverride : Math.min(5000, 500 * Math.pow(2, Math.min(attempts, 4)));
    rejoinTimer = setTimeout(() => {
      rejoinTimer = null;
      join();
    }, delay);
    updateStatus('reconnecting in ' + (delay / 1000).toFixed(1) + 's');
  }

  function onPcState(p) {
    if (p !== pc) return;
    const st = p.connectionState;
    if (st === 'connected') {
      attempts = 0;
      clearTimeout(discTimer);
      discTimer = null;
      stall.connectedAt = performance.now();
      stall.progressAt = stall.connectedAt;
      stall.quality = null;
      stall.decoded = null;
      stall.packets = null;
      stall.bytes = null;
      recentreTilt(0);
      updateStatus('connected');
    } else if (st === 'disconnected') {
      updateStatus('disconnected');
      clearTimeout(discTimer);
      discTimer = setTimeout(() => {
        if (pc === p && p.connectionState !== 'connected') scheduleRejoin();
      }, 4000);
    } else if (st === 'failed' || st === 'closed') {
      scheduleRejoin();
    } else {
      updateStatus(st);
    }
  }

  async function join() {
    if (joining) return;
    joining = true;
    teardown();
    updateStatus('joining');
    const p = new RTCPeerConnection({ iceServers: [] });
    pc = p;
    try {
      p.addTransceiver('video', { direction: 'recvonly' });
      dc = p.createDataChannel('input', { ordered: false, maxRetransmits: 0 });
      dc.onopen = () => sendNow();
      pingDc = p.createDataChannel('ping', { ordered: true });
      pingDc.onopen = () => pingTick();
      pingDc.onmessage = onPong;
      p.ontrack = (e) => {
        lowLatency(e.receiver);
        video.srcObject = e.streams[0] || new MediaStream([e.track]);
        video.play().catch(() => {});
      };
      p.onconnectionstatechange = () => onPcState(p);

      const offer = await p.createOffer();
      await p.setLocalDescription(offer);
      await waitIce(p);
      if (p !== pc) return;

      const res = await fetch('/api/join', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        cache: 'no-store',
        body: JSON.stringify({ clientId, wantSlot: lastSlot || 0, sdp: p.localDescription.sdp }),
      });
      if (p !== pc) return;
      if (res.status === 409) {
        const data = await res.json().catch(() => ({}));
        if (/game in progress/.test(data.error || '')) throw new GameInProgress(data.error);
        throw new RoomFull('room full');
      }
      if (!res.ok) throw new Error('join failed (HTTP ' + res.status + ')');
      const body = await res.json();
      slot = body.slot;
      lastSlot = slot;
      storeSet('party.lastSlot', String(slot));
      await p.setRemoteDescription({ type: 'answer', sdp: body.sdp });
      p.getReceivers().forEach((r) => lowLatency(r));
      updateStatus('connecting');
      resendChoices();
    } catch (err) {
      if (p === pc) {
        if (err instanceof RoomFull) {
          updateStatus('room full, retrying');
          joinMsg.textContent = 'All player slots are taken. Retrying...';
        } else if (err instanceof GameInProgress) {
          updateStatus('game in progress, retrying');
          joinMsg.textContent = 'Game in progress. You can join the next round.';
        } else {
          updateStatus('join error: ' + err.message);
        }
        scheduleRejoin();
      }
    } finally {
      joining = false;
      renderLobby();
    }
  }

  function updateStatus(detail) {
    if (detail !== undefined) statusEl.dataset.detail = detail;
    const d = statusEl.dataset.detail || '';
    statusEl.textContent = (slot ? 'P' + slot : 'No slot') + ' | ' + d + ' | steer ' + steerLabel();
  }

  // ---------- network stats (phone -> POST /api/stats, once a second) ----------
  const net = { prev: null, prevAt: 0, seq: 0, inFlight: false, uaSent: false, last: null };

  // Counter delta over the last sample. A counter that went down means the
  // peer connection was rebuilt, so the new value is the delta.
  function delta(cur, prev) {
    if (typeof cur !== 'number') return null;
    if (prev == null || cur < prev) return cur;
    return cur - prev;
  }
  function addrFamily(c) {
    const addr = c && (c.address || c.ip);
    if (!addr) return null;
    if (addr.endsWith('.local')) return 'mdns';
    return addr.indexOf(':') >= 0 ? 'v6' : 'v4';
  }

  async function sampleStats() {
    if (!pc || !slot || pc.connectionState !== 'connected' || net.inFlight) return;
    net.inFlight = true;
    try {
      const rep = await pc.getStats();
      const byId = new Map();
      let inb = null;
      let transport = null;
      let pair = null;
      rep.forEach((r) => {
        byId.set(r.id, r);
        if (!inb && r.type === 'inbound-rtp' && (r.kind === 'video' || r.mediaType === 'video')) inb = r;
        if (!transport && r.type === 'transport') transport = r;
      });
      if (transport && transport.selectedCandidatePairId) pair = byId.get(transport.selectedCandidatePairId);
      if (!pair) {
        rep.forEach((r) => {
          if (!pair && r.type === 'candidate-pair' && r.nominated && r.state === 'succeeded') pair = r;
        });
      }
      const lc = pair ? byId.get(pair.localCandidateId) : null;
      const rc = pair ? byId.get(pair.remoteCandidateId) : null;
      const now = performance.now();
      const dt = net.prevAt ? Math.max(0.2, (now - net.prevAt) / 1000) : 1;
      const cur = inb ? {
        recv: inb.packetsReceived, lost: inb.packetsLost, bytes: inb.bytesReceived,
        jbd: inb.jitterBufferDelay, jbn: inb.jitterBufferEmittedCount,
        dect: inb.totalDecodeTime, decn: inb.framesDecoded,
      } : null;
      const p = net.prev;
      const payload = {
        seq: ++net.seq,
        clientT: Date.now(),
        clientId,
        fps: inb ? r1(inb.framesPerSecond) : null,
        framesDecoded: inb ? inb.framesDecoded : null,
        framesDropped: inb ? inb.framesDropped : null,
        freezeCount: inb ? inb.freezeCount : null,
        freezeSec: inb ? r1(inb.totalFreezesDuration) : null,
        pauseCount: inb ? inb.pauseCount : null,
        packetsLost: inb ? inb.packetsLost : null,
        packetsReceived: inb ? inb.packetsReceived : null,
        nackCount: inb ? inb.nackCount : null,
        pliCount: inb ? inb.pliCount : null,
        firCount: inb ? inb.firCount : null,
        keyFrames: inb ? inb.keyFramesDecoded : null,
        width: inb ? inb.frameWidth : null,
        height: inb ? inb.frameHeight : null,
        jitterMs: inb && typeof inb.jitter === 'number' ? r1(inb.jitter * 1000) : null,
        rttMs: pair && typeof pair.currentRoundTripTime === 'number' ? r1(pair.currentRoundTripTime * 1000) : null,
        availKbps: pair && typeof pair.availableIncomingBitrate === 'number' ? Math.round(pair.availableIncomingBitrate / 1000) : null,
        localType: lc ? lc.candidateType : null,
        remoteType: rc ? rc.candidateType : null,
        localFamily: addrFamily(lc),
        remoteFamily: addrFamily(rc),
        pingMs,
        inputBufferedBytes: dc ? dc.bufferedAmount : null,
        inputSent,
        netType: navigator.connection ? navigator.connection.effectiveType || null : null,
        downlinkMbps: navigator.connection ? navigator.connection.downlink || null : null,
      };
      inputSent = 0;
      if (!net.uaSent) {
        payload.ua = navigator.userAgent;
        net.uaSent = true;
      }
      // Per-second deltas, computed against the previous sample.
      if (cur && p) {
        const dl = delta(cur.lost, p.lost);
        const dr = delta(cur.recv, p.recv);
        payload.lossPct = dl + dr > 0 ? r1((dl / (dl + dr)) * 100) : 0;
        const db = delta(cur.bytes, p.bytes);
        payload.kbps = db != null ? r1((db * 8) / dt / 1000) : null;
        const djn = delta(cur.jbn, p.jbn);
        payload.jbMs = djn > 0 ? r1((delta(cur.jbd, p.jbd) / djn) * 1000) : null;
        const dn = delta(cur.decn, p.decn);
        payload.decodeMs = dn > 0 ? r1((delta(cur.dect, p.dect) / dn) * 1000) : null;
      }
      if (cur) {
        net.prev = cur;
        net.prevAt = now;
      }
      net.last = payload;
      updateNetOverlay();
      await fetch('/api/stats', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        cache: 'no-store',
        body: JSON.stringify(payload),
      });
    } catch (e) {
      /* no stats this second: fine */
    } finally {
      net.inFlight = false;
    }
  }
  setInterval(sampleStats, 1000);

  function updateNetOverlay() {
    if (netOverlay.hidden || !net.last) return;
    const s = net.last;
    const v = (x, unit) => (x == null ? '-' : x + unit);
    netOverlay.textContent = 'fps ' + v(s.fps, '') + '  loss ' + v(s.lossPct, '%') +
      '  rtt ' + v(s.rttMs, 'ms') + '  jitter buf ' + v(s.jbMs, 'ms');
  }
  statusEl.addEventListener('click', () => {
    netOverlay.hidden = !netOverlay.hidden;
    updateNetOverlay();
  });

  // ---------- video stall watchdog ----------
  // A connected phone whose video has stopped decoding is rebuilt: the
  // PeerConnection is torn down and the phone rejoins with the same clientId
  // (scheduleRejoin). Progress is any of these advancing since the last tick: the
  // video element's totalVideoFrames, the inbound-rtp framesDecoded, and (only in a
  // browser with neither frame counter) inbound-rtp packetsReceived or bytesReceived. Some
  // iPhone browsers never advance totalVideoFrames for a live WebRTC stream, so no
  // single counter is trusted. No progress for STALL_MS is a stall. The watchdog reads
  // its own getStats each tick, so it does not depend on the once-a-second stats
  // sample. Hidden pages, the first STALL_GRACE_MS after a connect and phones that do
  // not show the video (lobby, starting: their slot may have no video source yet) are
  // not checked.
  function playbackFrames() {
    try {
      if (video.getVideoPlaybackQuality) return video.getVideoPlaybackQuality().totalVideoFrames;
      if (typeof video.webkitDecodedFrameCount === 'number') return video.webkitDecodedFrameCount;
    } catch (e) { /* not supported: the stats counters below still count */ }
    return null;
  }

  // Inbound video counters from a getStats report, or null when there is none.
  async function inboundCounters(p) {
    try {
      const rep = await p.getStats();
      let inb = null;
      rep.forEach((r) => {
        if (!inb && r.type === 'inbound-rtp' && (r.kind === 'video' || r.mediaType === 'video')) inb = r;
      });
      return inb ? { decoded: inb.framesDecoded, packets: inb.packetsReceived, bytes: inb.bytesReceived } : null;
    } catch (e) {
      return null;
    }
  }

  // Progress between two ticks. prev and cur hold quality, decoded, packets and bytes;
  // a null (unavailable) value never counts. A changed counter counts, whichever way it
  // moved, since a rebuilt PeerConnection starts its counters again. The packet and byte
  // counters are the last resort for a browser with no frame counter at all: packets
  // still arriving into a frozen decoder must not hide the stall.
  function stallProgress(prev, cur) {
    const num = (a) => typeof a === 'number';
    const moved = (a, b) => num(a) && num(b) && a !== b;
    if (moved(cur.quality, prev.quality) || moved(cur.decoded, prev.decoded)) return true;
    if (num(cur.quality) || num(cur.decoded)) return false;
    return moved(cur.packets, prev.packets) || moved(cur.bytes, prev.bytes);
  }

  // One-off event for the phone's stats log. It goes through the same POST
  // /api/stats as the once-a-second sample, so the server writes it to the jsonl.
  function logEvent(fields) {
    const payload = Object.assign({ clientId, clientT: Date.now() }, fields);
    fetch('/api/stats', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      cache: 'no-store',
      body: JSON.stringify(payload),
    }).catch(() => { /* logging only */ });
  }

  async function stallTick() {
    if (stall.busy) return;
    stall.busy = true;
    try {
      const p = pc;
      const inb = p && p.connectionState === 'connected' ? await inboundCounters(p) : null;
      if (p !== pc) return; // the connection was rebuilt while the stats were read
      const now = performance.now();
      const cur = Object.assign({ quality: playbackFrames(), decoded: null, packets: null, bytes: null }, inb);
      const progressed = stallProgress(stall, cur);
      stall.quality = cur.quality;
      stall.decoded = cur.decoded;
      stall.packets = cur.packets;
      stall.bytes = cur.bytes;
      const checking = !document.hidden && !!p && p.connectionState === 'connected' && slot > 0 &&
        now - stall.connectedAt >= STALL_GRACE_MS && currentView() === 'play';
      if (progressed || !checking) {
        stall.progressAt = now;
        return;
      }
      if (now - stall.progressAt >= STALL_MS) stallReconnect(now);
    } finally {
      stall.busy = false;
    }
  }

  // Backoff: 1 s, 2 s, 4 s, then 8 s. A stall within STALL_STREAK_MS of the
  // previous one continues the streak; a longer gap starts it again at 1 s.
  // STALL_BRAKE_COUNT stall reconnects within STALL_BRAKE_WINDOW_MS switch stall
  // reconnects off for STALL_BRAKE_MS, so a phone that cannot recover stops cycling.
  function stallReconnect(now) {
    if (rejoinTimer) return;
    if (now < stall.brakeUntil) {
      stall.progressAt = now;
      return;
    }
    stall.streak = now - stall.lastAt < STALL_STREAK_MS ? stall.streak + 1 : 1;
    stall.lastAt = now;
    stall.progressAt = now;
    const delay = Math.min(STALL_MAX_DELAY_MS, 1000 * Math.pow(2, Math.min(stall.streak - 1, 3)));
    logEvent({
      event: 'video_stall_reconnect',
      streak: stall.streak,
      delayMs: delay,
      quality: stall.quality,
      decoded: stall.decoded,
      packets: stall.packets,
      bytes: stall.bytes,
      readyState: video.readyState,
      pcState: pc ? pc.connectionState : null,
    });
    stall.reconnects = stall.reconnects.filter((t) => now - t < STALL_BRAKE_WINDOW_MS);
    stall.reconnects.push(now);
    if (stall.reconnects.length >= STALL_BRAKE_COUNT) {
      stall.brakeUntil = now + STALL_BRAKE_MS;
      stall.reconnects = [];
      logEvent({ event: 'video_stall_brake', untilMs: STALL_BRAKE_MS, count: STALL_BRAKE_COUNT });
    }
    updateStatus('video stalled, rejoining');
    scheduleRejoin(delay);
  }
  setInterval(stallTick, 1000);

  // ---------- lobby ----------
  let lobby = null;         // last server view (see GET /api/lobby)
  let lobbyRecvAt = 0;      // performance.now() when `lobby` arrived
  let lobbyVer = 0;         // version to long-poll from
  let catalog = null;       // { characters, vehicles, cups } from /catalog.json
  let catalogVer = '';      // catalogVersion the loaded catalog belongs to
  let catalogLoading = false;
  let vehWeightBuilt = null;
  let voteBuilt = null;
  let cupTab = '';          // cup shown in the vote picker (user choice)
  let errText = '';         // last error from a lobby POST
  const persisted = {
    name: storeGet('party.name') || '',
    character: storeGet('party.character') || '',
    vehicle: storeGet('party.vehicle') || '',
    drift: storeGet('party.drift') || '',
  };

  function catalogChar(id) { return catalog ? catalog.characters.find((c) => c.id === id) || null : null; }
  function catalogVeh(id) { return catalog ? catalog.vehicles.find((v) => v.id === id) || null : null; }
  function cupOfCourse(id) {
    if (!catalog) return null;
    return catalog.cups.find((cup) => cup.courses.some((c) => c.id === id)) || null;
  }
  function courseName(id) {
    if (id === 'random') return 'Random';
    const cup = cupOfCourse(id);
    if (!cup) return id;
    return cup.courses.find((c) => c.id === id).name;
  }
  function myRec() {
    if (!lobby || !slot) return null;
    return lobby.players.find((p) => p.slot === slot) || null;
  }
  function isHost() {
    return !!lobby && slot > 0 && lobby.hostSlot === slot;
  }
  // Players who will race if the host starts now: connected and ready.
  function readyCount() {
    if (!lobby) return 0;
    return lobby.players.filter((p) => p.occupied && p.connected && p.ready).length;
  }

  async function loadCatalog(ver) {
    if (catalogLoading) return;
    catalogLoading = true;
    try {
      const res = await fetch('/catalog.json', { cache: 'no-store' });
      if (!res.ok) throw new Error('HTTP ' + res.status);
      catalog = await res.json();
      catalogVer = ver;
      buildCatalogUI();
    } catch (e) {
      errText = 'Could not load the catalog; retrying';
    } finally {
      catalogLoading = false;
      renderLobby();
    }
  }

  // Tiles are built once per catalog and updated in place, so a tap is not
  // lost when the long-poll brings a change.
  function tileButton(cls, title, sub) {
    const b = el('button', 'pTile ' + cls);
    b.appendChild(document.createTextNode(title));
    if (sub) b.appendChild(el('small', '', sub));
    return b;
  }

  function buildCatalogUI() {
    charGrid.textContent = '';
    for (const w of WEIGHTS) {
      charGrid.appendChild(el('div', 'weightLabel', w.toUpperCase()));
      for (const ch of catalog.characters) {
        if (ch.weight !== w || !ch.unlocked) continue;
        const b = tileButton('w-' + w, ch.name);
        b.dataset.id = ch.id;
        charGrid.appendChild(b);
      }
    }
    cupTabs.textContent = '';
    for (const cup of catalog.cups) {
      if (!cup.unlocked) continue;
      const b = el('button', 'tab', cup.name);
      b.dataset.cup = cup.id;
      cupTabs.appendChild(b);
    }
    vehWeightBuilt = null;
    voteBuilt = null;
  }

  function buildVehicleTiles(weight) {
    vehGrid.textContent = '';
    if (!weight) {
      vehGrid.appendChild(el('div', 'gridHint', 'Pick a character first.'));
    } else {
      for (const v of catalog.vehicles) {
        if (v.weight !== weight || !v.unlocked) continue;
        const b = tileButton('', v.name, v.type === 'bike' ? 'bike' : 'kart');
        b.dataset.id = v.id;
        vehGrid.appendChild(b);
      }
    }
    vehWeightBuilt = weight;
  }

  function buildCourseTiles(cupId) {
    courseGrid.textContent = '';
    const r = tileButton('', 'Random');
    r.dataset.id = 'random';
    courseGrid.appendChild(r);
    const cup = catalog.cups.find((c) => c.id === cupId);
    if (cup) {
      for (const c of cup.courses) {
        const b = tileButton('', c.name);
        b.dataset.id = c.id;
        courseGrid.appendChild(b);
      }
    }
    voteBuilt = cupId;
  }

  function firstUnlockedCup() {
    return catalog.cups.find((c) => c.unlocked) || catalog.cups[0];
  }

  function renderVote(me) {
    const voteCup = me ? cupOfCourse(me.vote) : null;
    let active = cupTab && catalog.cups.some((c) => c.id === cupTab) ? cupTab : '';
    if (!active) active = voteCup ? voteCup.id : firstUnlockedCup().id;
    if (voteBuilt !== active) buildCourseTiles(active);
    for (const b of cupTabs.querySelectorAll('.tab')) b.classList.toggle('sel', b.dataset.cup === active);
    for (const b of courseGrid.querySelectorAll('.pTile')) b.classList.toggle('sel', !!me && b.dataset.id === me.vote);
  }

  function renderPlayers() {
    playerList.textContent = '';
    let any = false;
    for (const p of lobby.players) {
      if (!p.occupied) continue;
      any = true;
      const mine = p.slot === slot;
      const li = el('li', 'player' + (mine ? ' me' : ''));
      const ch = catalogChar(p.character);
      const state = !p.connected ? 'offline' : p.ready ? 'READY' : 'not ready';
      li.append(
        el('span', 'dot' + (p.connected ? ' on' : '')),
        el('span', 'pname', (p.name || '(no name)') + (mine ? ' (you)' : '')),
        el('span', 'pchar', ch ? ch.name : ''),
        el('span', 'prdy' + (p.ready ? ' yes' : ''), state),
      );
      playerList.appendChild(li);
    }
    if (!any) playerList.appendChild(el('li', 'pchar', 'Nobody here yet.'));
  }

  function defaultHint(me) {
    if (!slot) return 'Connecting...';
    if (!me || !me.name || !me.character || !me.vehicle) return 'Pick a name, character and vehicle, then tap READY.';
    if (!me.ready) return 'Tap READY to race. Players who are not ready sit out.';
    const n = readyCount();
    if (isHost()) return n > 0 ? 'Tap START when you are set.' : 'START unlocks when at least one player is ready.';
    return n > 0 ? 'Waiting for the host to start.' : 'Waiting for someone to be ready.';
  }

  function renderLobby() {
    if (!lobby) return;
    const me = myRec();
    lobbyMsg.textContent = lobby.message || '';
    if (document.activeElement !== nameIn) nameIn.value = me ? me.name : persisted.name;

    for (const b of charGrid.querySelectorAll('.pTile')) {
      b.classList.toggle('sel', !!me && me.character === b.dataset.id);
    }
    const ch = me && me.character ? catalogChar(me.character) : null;
    const weight = ch ? ch.weight : '';
    if (catalog && vehWeightBuilt !== weight) buildVehicleTiles(weight);
    for (const b of vehGrid.querySelectorAll('.pTile')) {
      b.classList.toggle('sel', !!me && me.vehicle === b.dataset.id);
    }
    driftAuto.classList.toggle('sel', !!me && me.drift === 'auto');
    driftManual.classList.toggle('sel', !!me && me.drift === 'manual');
    if (catalog) renderVote(me);

    readyBtn.classList.toggle('on', !!me && me.ready);
    readyBtn.textContent = me && me.ready ? 'READY (tap to undo)' : 'READY';
    renderPlayers();

    const host = isHost();
    const n = readyCount();
    startBtn.hidden = !host;
    startBtn.textContent = 'Start (' + n + (n === 1 ? ' player)' : ' players)');
    startBtn.disabled = !(host && lobby.phase === 'lobby' && n > 0);
    lobbyBtn.hidden = !host;
    renderWaiting(me);

    lobbyHint.textContent = errText || defaultHint(me);
    startStatus.textContent = me && me.status ? me.status : 'Waiting for the navigator...';
    startMsg.textContent = lobby.message || '';
    updateSteerUI();
    showView();
  }

  function remember(body) {
    for (const k of ['name', 'character', 'vehicle', 'drift']) {
      if (typeof body[k] === 'string' && body[k] !== '') {
        persisted[k] = body[k];
        storeSet('party.' + k, body[k]);
      }
    }
  }

  // POST a lobby change for this phone. The response is the new lobby view.
  async function lobbyPost(path, body) {
    if (!slot) {
      errText = 'Still connecting. Try again in a moment.';
      renderLobby();
      return null;
    }
    const payload = Object.assign({ clientId }, body);
    try {
      const res = await fetch(path, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        cache: 'no-store',
        body: JSON.stringify(payload),
      });
      const data = await res.json().catch(() => ({}));
      if (!res.ok) {
        errText = data.error || ('HTTP ' + res.status);
        renderLobby();
        return null;
      }
      errText = '';
      remember(body);
      applyLobby(data);
      return data;
    } catch (e) {
      errText = 'Network error. Trying again.';
      renderLobby();
      return null;
    }
  }

  // Sent after every (re)join so the server has this phone's choices again
  // (for example after a server restart). Values the server already holds are
  // accepted as no-ops.
  function resendChoices() {
    const body = {};
    for (const k of ['name', 'character', 'vehicle', 'drift']) {
      if (persisted[k]) body[k] = persisted[k];
    }
    if (Object.keys(body).length) lobbyPost('/api/lobby/player', body);
  }

  function applyLobby(v) {
    lobby = v;
    lobbyRecvAt = performance.now();
    lobbyVer = v.version;
    if (!catalog || catalogVer !== v.catalogVersion) {
      if (!catalogLoading) loadCatalog(v.catalogVersion);
    }
    renderLobby();
  }

  // Long-poll GET /api/lobby?since=N. The server answers at once on a change
  // and otherwise holds the request for about 25 s.
  async function lobbyLoop() {
    let failures = 0;
    for (;;) {
      const ctl = new AbortController();
      const timer = setTimeout(() => ctl.abort(), 35000);
      try {
        const res = await fetch('/api/lobby?since=' + lobbyVer, { cache: 'no-store', signal: ctl.signal });
        if (!res.ok) throw new Error('HTTP ' + res.status);
        const v = await res.json();
        failures = 0;
        applyLobby(v);
      } catch (e) {
        failures += 1;
        await sleep(Math.min(5000, 300 * failures));
      } finally {
        clearTimeout(timer);
      }
    }
  }

  // Lobby controls. Each tap posts one change.
  charGrid.addEventListener('click', (e) => {
    const b = e.target.closest('.pTile');
    if (!b || !b.dataset.id) return;
    const ch = catalogChar(b.dataset.id);
    if (!ch) return;
    const me = myRec();
    const curVeh = me && me.vehicle ? catalogVeh(me.vehicle) : null;
    const body = { character: ch.id };
    // A vehicle must match the character's weight class, so send one along
    // when the weight class changes.
    if (!curVeh || curVeh.weight !== ch.weight) {
      const first = catalog.vehicles.find((v) => v.weight === ch.weight && v.type === 'kart' && v.unlocked);
      if (first) body.vehicle = first.id;
    }
    lobbyPost('/api/lobby/player', body);
  });
  vehGrid.addEventListener('click', (e) => {
    const b = e.target.closest('.pTile');
    if (b && b.dataset.id) lobbyPost('/api/lobby/player', { vehicle: b.dataset.id });
  });
  driftAuto.addEventListener('click', () => lobbyPost('/api/lobby/player', { drift: 'auto' }));
  driftManual.addEventListener('click', () => lobbyPost('/api/lobby/player', { drift: 'manual' }));
  cupTabs.addEventListener('click', (e) => {
    const b = e.target.closest('.tab');
    if (!b) return;
    cupTab = b.dataset.cup;
    renderLobby();
  });
  courseGrid.addEventListener('click', (e) => {
    const b = e.target.closest('.pTile');
    if (b && b.dataset.id) lobbyPost('/api/lobby/player', { vote: b.dataset.id });
  });
  nameIn.addEventListener('change', () => {
    const v = nameIn.value.trim();
    if (!v) {
      errText = 'Name must not be empty.';
      renderLobby();
      return;
    }
    lobbyPost('/api/lobby/player', { name: v });
  });
  nameIn.addEventListener('keydown', (e) => {
    if (e.key === 'Enter') nameIn.blur();
  });
  readyBtn.addEventListener('click', () => {
    const me = myRec();
    lobbyPost('/api/lobby/player', { ready: !(me && me.ready) });
  });
  startBtn.addEventListener('click', () => lobbyPost('/api/lobby/start', {}));
  lobbyBtn.addEventListener('click', () => {
    if (window.confirm('Send everyone back to the lobby?')) lobbyPost('/api/lobby/reset', {});
  });

  // ---------- invite (QR code and URL for a friend) ----------
  const inviteBtn = $('inviteBtn');
  const inviteEl = $('invite');
  const inviteQr = $('inviteQr');
  const inviteUrl = $('inviteUrl');
  const inviteNone = $('inviteNone');
  const inviteOthers = $('inviteOthers');
  const inviteNote = $('inviteNote');
  const inviteClose = $('inviteClose');

  // Asks the server for the current URLs each time, since addresses change.
  async function openInvite() {
    inviteEl.hidden = false;
    inviteQr.hidden = true;
    inviteNone.hidden = true;
    inviteNote.hidden = true;
    inviteOthers.textContent = '';
    inviteUrl.textContent = 'Finding the address...';
    let urls;
    try {
      const res = await fetch('/api/joininfo', { cache: 'no-store' });
      if (!res.ok) throw new Error('HTTP ' + res.status);
      urls = (await res.json()).urls || [];
    } catch (e) {
      inviteUrl.textContent = 'Could not load the address. Try again.';
      return;
    }
    const pref = urls.find((u) => u.secure) || urls[0] || null;
    inviteNone.hidden = !!pref;
    inviteQr.hidden = !pref;
    inviteUrl.textContent = pref ? pref.url : '';
    if (pref) inviteQr.src = '/api/joinqr.png?u=' + encodeURIComponent(pref.url);
    inviteNote.hidden = !(pref && pref.secure);
    for (const u of urls) {
      if (u === pref) continue;
      inviteOthers.appendChild(el('div', '', u.label + ': ' + u.url));
    }
  }
  inviteBtn.addEventListener('click', openInvite);
  inviteClose.addEventListener('click', () => { inviteEl.hidden = true; });
  inviteEl.addEventListener('click', (e) => { if (e.target === inviteEl) inviteEl.hidden = true; });

  // ---------- next-race vote overlay ----------
  // Shown over the live video while my slot has voteOpen (the play view only).
  // Every course tap is saved at once (vote); Lock in sends voteLocked:true.
  // While locked, the picks are disabled until Change vote sends voteLocked:false.
  let voteCupSel = '';        // cup tab picked in the overlay; '' follows my vote
  let voteTabsCat = null;     // catalogVersion the vote tabs were built for
  let voteCoursesCup = null;  // cup id the vote course tiles show
  let voteOpenShown = false;  // overlay is up

  // Lets go of every held button and centres the slider, so nothing stays
  // pressed once the controls are hidden.
  function dropInput() {
    for (const pid of [...ptrs.keys()]) releasePtr(pid);
    sliderPtr = null;
    sliderX = 0;
    steerKnob.style.left = '50%';
    sendNow();
  }

  function voteCupId(me) {
    const open = catalog.cups.filter((c) => c.unlocked);
    if (voteCupSel && open.some((c) => c.id === voteCupSel)) return voteCupSel;
    const mine = me ? cupOfCourse(me.vote) : null;
    if (mine && mine.unlocked) return mine.id;
    return open.length ? open[0].id : '';
  }

  function buildVoteTabs() {
    voteTabs.textContent = '';
    for (const cup of catalog.cups) {
      if (!cup.unlocked) continue;
      const b = el('button', 'tab', cup.name);
      b.dataset.cup = cup.id;
      voteTabs.appendChild(b);
    }
    voteTabsCat = catalogVer;
  }

  function buildVoteCourses(cupId) {
    voteCourses.textContent = '';
    const cup = catalog.cups.find((c) => c.id === cupId);
    if (cup) {
      for (const c of cup.courses) {
        const b = tileButton('', c.name);
        b.dataset.id = c.id;
        voteCourses.appendChild(b);
      }
    }
    voteCoursesCup = cupId;
  }

  // The server sends voteRemainingMs (its own clock); counting starts from the
  // moment this response arrived, so the phone clock is never compared with the
  // navigator's deadline.
  function renderVoteCountdown(me) {
    const hasDeadline = !!me && !!me.voteDeadlineMs;
    voteCountdown.hidden = !hasDeadline;
    if (!hasDeadline) return;
    const left = me.voteRemainingMs - (performance.now() - lobbyRecvAt);
    const secs = Math.ceil(left / 1000);
    voteCountdown.textContent = secs > 0 ? 'Closes in ' + secs + 's' : 'Closing…';
  }
  setInterval(() => { if (voteOpenShown) renderVoteCountdown(myRec()); }, 250);

  function renderVoteOverlay(me) {
    if (!catalog) {
      voteTabs.textContent = '';
      voteCourses.textContent = '';
      voteStatus.textContent = 'Loading courses…';
      voteLock.disabled = true;
      return;
    }
    if (voteTabsCat !== catalogVer) buildVoteTabs();
    const cupId = voteCupId(me);
    if (voteCoursesCup !== cupId) buildVoteCourses(cupId);
    for (const b of voteTabs.querySelectorAll('.tab')) b.classList.toggle('sel', b.dataset.cup === cupId);
    const locked = !!me.voteLocked;
    for (const b of voteCourses.querySelectorAll('.pTile')) {
      b.classList.toggle('sel', b.dataset.id === me.vote);
      b.disabled = locked;
    }
    voteRandom.classList.toggle('sel', me.vote === 'random');
    voteRandom.disabled = locked;
    voteLock.disabled = locked;
    voteLock.textContent = locked ? 'Locked in' : 'Lock in';
    voteChange.hidden = !locked;
    voteStatus.classList.toggle('err', !!errText);
    voteStatus.textContent = errText || (locked ? 'Locked in — waiting for others' : 'Tap a course, then Lock in');
    renderVoteCountdown(me);
  }

  voteTabs.addEventListener('click', (e) => {
    const b = e.target.closest('.tab');
    if (!b) return;
    voteCupSel = b.dataset.cup;
    renderLobby();
  });
  voteCourses.addEventListener('click', (e) => {
    const b = e.target.closest('.pTile');
    if (b && b.dataset.id && !b.disabled) lobbyPost('/api/lobby/player', { vote: b.dataset.id });
  });
  voteRandom.addEventListener('click', () => {
    if (!voteRandom.disabled) lobbyPost('/api/lobby/player', { vote: 'random' });
  });
  voteLock.addEventListener('click', () => lobbyPost('/api/lobby/player', { voteLocked: true }));
  voteChange.addEventListener('click', () => lobbyPost('/api/lobby/player', { voteLocked: false }));

  // ---------- which screen ----------
  // join: the Join card. lobby: roster and choices. starting: "Get ready" while
  // the navigator drives the emulator. play: video and controls (once showVideo).
  // waiting: a race is on and this phone is not in it (it sat out, or it
  // tried to join mid-race). starting: in the race, video not up yet.
  let shownView = '';
  function currentView() {
    if (!joinedOnce) return 'join';
    const me = myRec();
    if (me && me.showVideo && me.locked) return 'play';
    if (lobby && lobby.phase !== 'lobby') return me && me.locked ? 'starting' : 'waiting';
    return 'lobby';
  }
  function renderWaiting(me) {
    if (me) {
      waitTitle.textContent = 'Waiting for the next round';
      waitMsg.textContent = 'The race is on without you this time. Your controls are paused; you can race next round.';
    } else {
      waitTitle.textContent = 'Game in progress';
      waitMsg.textContent = 'You can join the next round. This page will connect when the room is back in the lobby.';
    }
  }
  function showView() {
    const v = currentView();
    const me = myRec();
    // The vote overlay replaces the controller buttons; the navigator drives the pad meanwhile.
    const voting = v === 'play' && !!me && !!me.voteOpen;
    overlay.hidden = v !== 'join';
    lobbyEl.hidden = v !== 'lobby';
    startingEl.hidden = v !== 'starting';
    waitingEl.hidden = v !== 'waiting';
    controls.hidden = v !== 'play' || voting;
    document.body.classList.toggle('playing', v === 'play');
    if (voting && !voteOpenShown) {
      voteCupSel = '';
      voteCoursesCup = null;
      dropInput();
    }
    voteOpenShown = voting;
    voteOverlay.hidden = !voting;
    if (voting) renderVoteOverlay(me);
    if (v === 'play' && shownView !== 'play') {
      recentreTilt(0);
      keepAwake();
    }
    if (v !== 'play') menuPad.hidden = true;
    shownView = v;
  }

  // ---------- start ----------
  let wakeLock = null;
  async function keepAwake() {
    try {
      if (navigator.wakeLock && !wakeLock) wakeLock = await navigator.wakeLock.request('screen');
    } catch (e) { /* not supported or denied: fine */ }
  }

  function updateHttpsHint() {
    const need = !window.isSecureContext && location.protocol === 'http:';
    httpsHint.hidden = !need;
    if (need) {
      const host = location.hostname.indexOf(':') >= 0 ? '[' + location.hostname + ']' : location.hostname;
      const url = 'https://' + host + ':8443/';
      httpsLink.href = url;
      httpsLink.textContent = url;
    }
  }
  updateHttpsHint();

  joinBtn.addEventListener('click', async () => {
    joinBtn.disabled = true;
    joinMsg.textContent = 'Starting...';
    // Motion permission first (it needs this tap, so nothing above awaits),
    // then join.
    const tiltOk = await ensureMotion();
    steerMode = tiltOk ? 'tilt' : 'slider';
    joinedOnce = true;
    keepAwake();
    showView();
    await join();
    joinBtn.disabled = false;
  });

  // ---------- page-level gesture suppression ----------
  document.addEventListener('contextmenu', (e) => e.preventDefault());
  ['gesturestart', 'gesturechange', 'gestureend'].forEach((t) =>
    document.addEventListener(t, (e) => e.preventDefault()));
  // The lobby scrolls; everywhere else touch movement is suppressed.
  document.addEventListener('touchmove', (e) => {
    const t = e.target;
    if (t && t.closest && t.closest('#lobby, #invite')) return;
    e.preventDefault();
  }, { passive: false });
  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState === 'visible' && joinedOnce && !pc) join();
  });

  window.addEventListener('pagehide', () => { sendNow(); teardown(); });
  updateStatus('not joined');
  showView();
  lobbyLoop();
})();
