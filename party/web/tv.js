// Viewer page. Lobby: a roster of the seated players (votes shown once the race
// starts). Live: one tile per racing player (locked at Start), laid out as a
// grid that fills the screen. Each tile attaches to the slot's track with
// POST /api/watch. Viewers take no slot and open no input channel.
(() => {
  'use strict';

  const grid = document.getElementById('grid');
  const tilesEl = document.getElementById('tiles');
  const roster = document.getElementById('roster');
  const rosterTitle = document.getElementById('rosterTitle');
  const rosterMsg = document.getElementById('rosterMsg');
  const rosterList = document.getElementById('rosterList');
  const rosterFoot = document.getElementById('rosterFoot');
  const fsBtn = document.getElementById('fsBtn');
  const liveNote = document.getElementById('liveNote');
  const joinToggle = document.getElementById('joinToggle');
  // slot -> { slot, label, name, connected, fig, video, cap, pc, retry, frames, lastFrame, stalled }
  const tiles = new Map();
  const TILE_GAP = 6;
  const TILE_PAD = 6;
  let joinCollapsed = false;

  // How to join: the panel sits by the roster in the lobby and in a corner
  // while the race is live.
  const joinPanel = document.getElementById('joinPanel');
  const joinSlot = document.getElementById('joinSlot');
  const joinQr = document.getElementById('joinQr');
  const joinUrl = document.getElementById('joinUrl');
  const joinNone = document.getElementById('joinNone');
  const joinOthers = document.getElementById('joinOthers');
  const joinNote = document.getElementById('joinNote');
  let joinUrls = [];
  let joinLoaded = false;
  let joinQrSrc = '';

  let lobby = null;
  let lobbyVer = 0;
  let catalog = null;
  let catalogVer = '';
  let catalogLoading = false;

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
      timer = setTimeout(() => { p.removeEventListener('icegatheringstatechange', done); resolve(); }, 4000);
      p.addEventListener('icegatheringstatechange', done);
    });
  }

  // A tile per racing player. New tiles go in slot order, inserted before the
  // next higher slot so existing <video> elements are never moved (a moved
  // video pauses).
  function makeTile(p) {
    const fig = document.createElement('figure');
    fig.className = 'tile';
    const video = document.createElement('video');
    video.autoplay = true;
    video.playsInline = true;
    video.muted = true;
    const cap = document.createElement('figcaption');
    const reco = document.createElement('div');
    reco.className = 'reco';
    reco.textContent = 'reconnecting…';
    const badge = document.createElement('div');
    badge.className = 'vbadge';
    badge.hidden = true;
    fig.append(video, cap, reco, badge);
    let before = null;
    for (const o of tiles.values()) {
      if (o.slot > p.slot && (!before || o.slot < before.slot)) before = o;
    }
    tilesEl.insertBefore(fig, before ? before.fig : null);
    const t = {
      slot: p.slot, label: p.label, name: '', connected: true,
      voteOpen: false, voteLocked: false, vote: '',
      fig, video, cap, badge, pc: null, retry: null,
      frames: -1, lastFrame: performance.now(), stalled: false,
    };
    tiles.set(p.slot, t);
    watch(t);
    return t;
  }

  async function watch(t) {
    if (t.pc) { try { t.pc.close(); } catch (e) { /* ignore */ } t.pc = null; }
    const p = new RTCPeerConnection({ iceServers: [] });
    t.pc = p;
    const retry = () => {
      if (t.pc === p) { t.pc = null; }
      try { p.close(); } catch (e) { /* ignore */ }
      clearTimeout(t.retry);
      t.retry = setTimeout(() => watch(t), 2000);
    };
    p.addTransceiver('video', { direction: 'recvonly' });
    p.ontrack = (e) => {
      // Minimum playout delay, as on the phone (app.js).
      try { if ('jitterBufferTarget' in e.receiver) e.receiver.jitterBufferTarget = 0; } catch (err) { /* ignore */ }
      try { if ('playoutDelayHint' in e.receiver) e.receiver.playoutDelayHint = 0; } catch (err) { /* ignore */ }
      t.video.srcObject = e.streams[0] || new MediaStream([e.track]);
      t.video.play().catch(() => {});
    };
    p.onconnectionstatechange = () => {
      if (t.pc !== p) return;
      if (p.connectionState === 'failed' || p.connectionState === 'closed') retry();
    };
    try {
      await p.setLocalDescription(await p.createOffer());
      await waitIce(p);
      const res = await fetch('/api/watch', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        cache: 'no-store',
        body: JSON.stringify({ slot: t.slot, sdp: p.localDescription.sdp }),
      });
      if (!res.ok) throw new Error('HTTP ' + res.status);
      const body = await res.json();
      if (t.pc !== p) return;
      await p.setRemoteDescription({ type: 'answer', sdp: body.sdp });
    } catch (err) {
      if (t.pc === p) retry();
    }
  }

  function removeTile(slot) {
    const t = tiles.get(slot);
    if (!t) return;
    clearTimeout(t.retry);
    if (t.pc) { try { t.pc.close(); } catch (e) { /* ignore */ } t.pc = null; }
    t.fig.remove();
    tiles.delete(slot);
  }

  function frameCount(v) {
    if (v.getVideoPlaybackQuality) return v.getVideoPlaybackQuality().totalVideoFrames;
    return v.webkitDecodedFrameCount || 0;
  }

  function paintTile(t) {
    t.cap.textContent = (t.name || t.label) + ' · slot ' + t.slot;
    t.fig.classList.toggle('reconnecting', !t.connected || t.stalled);
    // Next-race vote: "voting…" until the player locks in, then the course.
    t.badge.hidden = !t.voteOpen;
    if (t.voteOpen) {
      t.badge.textContent = t.voteLocked ? 'locked: ' + courseName(t.vote) : 'voting…';
    }
  }

  // Every 500 ms: a tile is "reconnecting" while its phone is disconnected or
  // no new frame has arrived for more than 2 s.
  function checkTiles() {
    const now = performance.now();
    for (const t of tiles.values()) {
      let n = 0;
      try { n = frameCount(t.video); } catch (e) { /* ignore */ }
      if (n !== t.frames) { t.frames = n; t.lastFrame = now; }
      t.stalled = now - t.lastFrame > 2000;
      paintTile(t);
    }
  }

  // Grid shape for n racing tiles: 1 -> 1x1, 2 -> 2x1, 3-4 -> 2x2, 5-6 -> 3x2,
  // 7-9 -> 3x3, 10-12 -> 4x3. A short last row is centred by the flex wrap.
  function gridShape(n) {
    const cols = Math.ceil(Math.sqrt(n));
    return { cols, rows: Math.ceil(n / cols) };
  }

  // Largest 16:9 tile that fits the screen in that shape; the .tiles box is
  // sized to exactly cols x rows so each row holds exactly cols tiles.
  function layoutTiles() {
    const n = tiles.size;
    if (!n) return;
    const { cols, rows } = gridShape(n);
    const availW = window.innerWidth - 2 * TILE_PAD;
    const availH = window.innerHeight - 2 * TILE_PAD;
    const byW = (availW - (cols - 1) * TILE_GAP) / cols;
    const byH = ((availH - (rows - 1) * TILE_GAP) / rows) * 16 / 9;
    const tw = Math.max(1, Math.floor(Math.min(byW, byH)));
    const th = Math.round(tw * 9 / 16);
    tilesEl.style.setProperty('--tw', tw + 'px');
    tilesEl.style.setProperty('--th', th + 'px');
    tilesEl.style.width = (cols * tw + (cols - 1) * TILE_GAP) + 'px';
    tilesEl.style.height = (rows * th + (rows - 1) * TILE_GAP) + 'px';
  }

  // Racing players (locked at Start) get a tile while the race is live; the
  // rest get none. Only the difference is added or removed.
  function syncTiles() {
    const racing = lobby && lobby.phase === 'live' ? lobby.players.filter((p) => p.locked) : [];
    const want = new Set(racing.map((p) => p.slot));
    for (const slot of [...tiles.keys()]) {
      if (!want.has(slot)) removeTile(slot);
    }
    for (const p of racing) {
      const t = tiles.get(p.slot) || makeTile(p);
      t.label = p.label;
      t.name = p.name;
      t.connected = !!p.connected;
      t.voteOpen = !!p.voteOpen;
      t.voteLocked = !!p.voteLocked;
      t.vote = p.vote;
      paintTile(t);
    }
    layoutTiles();
  }

  // ---------- lobby roster ----------
  async function loadCatalog(ver) {
    if (catalogLoading) return;
    catalogLoading = true;
    try {
      const res = await fetch('/catalog.json', { cache: 'no-store' });
      if (res.ok) {
        catalog = await res.json();
        catalogVer = ver;
      }
    } catch (e) { /* retried on the next lobby change */ } finally {
      catalogLoading = false;
      renderAll();
    }
  }

  function vehicleName(id) {
    const v = catalog && catalog.vehicles.find((x) => x.id === id);
    return v ? v.name : '';
  }
  function characterName(id) {
    const c = catalog && catalog.characters.find((x) => x.id === id);
    return c ? c.name : '';
  }
  function courseName(id) {
    if (id === 'random') return 'Random';
    if (!catalog) return id;
    for (const cup of catalog.cups) {
      const c = cup.courses.find((x) => x.id === id);
      if (c) return c.name;
    }
    return id;
  }

  // One card per seated player (empty slots are not listed).
  function rosterCard(p) {
    const inRace = lobby.phase !== 'lobby';
    const d = document.createElement('div');
    d.className = 'rCard' + (p.ready ? ' ready' : '') + (inRace && p.locked ? ' racing' : '') +
      (inRace && !p.locked ? ' sitout' : '');
    const line = document.createElement('div');
    line.className = 'slotLine';
    const dot = document.createElement('span');
    dot.className = 'dot' + (p.connected ? ' on' : '');
    line.append(dot, document.createTextNode(p.label + (p.connected ? '' : ' (reconnecting)')));
    if (inRace) {
      const badge = document.createElement('span');
      badge.className = 'badge' + (p.locked ? ' go' : '');
      badge.textContent = p.locked ? 'RACING' : 'sitting out';
      line.appendChild(badge);
    }
    d.appendChild(line);
    const who = document.createElement('div');
    who.className = 'who';
    who.textContent = p.name || '(no name)';
    d.appendChild(who);
    const meta = document.createElement('div');
    meta.className = 'meta';
    const parts = [characterName(p.character), vehicleName(p.vehicle), p.drift ? p.drift + ' drift' : ''].filter(Boolean);
    meta.textContent = parts.join(' · ');
    const tag = document.createElement('span');
    tag.className = p.ready ? 'ready-tag' : '';
    tag.textContent = p.ready ? '  READY' : '  not ready';
    meta.appendChild(tag);
    if (inRace && p.locked) {
      const v = document.createElement('div');
      v.className = 'vote';
      v.textContent = 'Vote: ' + courseName(p.vote);
      meta.appendChild(v);
    }
    d.appendChild(meta);
    if (p.status) {
      const st = document.createElement('div');
      st.className = 'rStatus';
      st.textContent = p.status;
      d.appendChild(st);
    }
    return d;
  }

  function renderRoster() {
    if (!lobby) return;
    rosterTitle.textContent = lobby.phase === 'starting' ? 'Get ready' : lobby.phase === 'live' ? 'Racing' : 'Lobby';
    rosterMsg.textContent = lobby.message || '';
    rosterList.textContent = '';
    const seated = lobby.players.filter((p) => p.occupied);
    if (!seated.length) rosterList.appendChild(Object.assign(document.createElement('div'), { className: 'rosterEmpty', textContent: 'Nobody here yet. Scan the code to join.' }));
    for (const p of seated) rosterList.appendChild(rosterCard(p));
    const n = seated.length;
    const parts = [n + (n === 1 ? ' player' : ' players')];
    if (lobby.phase === 'lobby') {
      parts.push(seated.filter((p) => p.connected && p.ready).length + ' ready');
    } else {
      const racers = seated.filter((p) => p.locked).map((p) => p.label);
      parts.push('racing: ' + (racers.length ? racers.join(', ') : 'nobody'));
      const out = seated.length - racers.length;
      if (out) parts.push(out + ' sitting out');
    }
    const host = lobby.players.find((p) => p.slot === lobby.hostSlot);
    if (host && lobby.phase === 'lobby') parts.push('host: ' + host.label);
    rosterFoot.textContent = parts.join(' · ');
  }

  function renderAll() {
    if (!lobby) return;
    const live = lobby.phase === 'live';
    grid.hidden = !live;
    roster.hidden = live;
    fsBtn.hidden = !live;
    liveNote.hidden = !live;
    renderRoster();
    renderJoin();
    syncTiles();
  }

  fsBtn.addEventListener('click', () => {
    try {
      const p = document.fullscreenElement
        ? document.exitFullscreen()
        : document.documentElement.requestFullscreen();
      if (p && p.catch) p.catch(() => {});
    } catch (e) { /* not supported */ }
  });
  document.addEventListener('fullscreenchange', () => {
    fsBtn.textContent = document.fullscreenElement ? 'Exit fullscreen' : 'Fullscreen';
  });
  joinToggle.addEventListener('click', () => {
    joinCollapsed = !joinCollapsed;
    renderJoin();
  });
  window.addEventListener('resize', layoutTiles);

  // ---------- how to join ----------
  async function loadJoinInfo() {
    try {
      const res = await fetch('/api/joininfo', { cache: 'no-store' });
      if (!res.ok) throw new Error('HTTP ' + res.status);
      joinUrls = (await res.json()).urls || [];
      joinLoaded = true;
    } catch (e) { /* keep the last list; retried every 10 s */ }
    renderJoin();
  }

  function renderJoin() {
    const live = !!lobby && lobby.phase === 'live';
    const target = live ? document.body : joinSlot;
    if (joinPanel.parentNode !== target) target.appendChild(joinPanel);
    joinPanel.classList.toggle('corner', live);
    joinPanel.classList.toggle('collapsed', live && joinCollapsed);
    joinToggle.textContent = joinCollapsed ? 'Show QR' : 'Hide QR';
    // Preferred: the first https URL, else the first URL.
    const pref = joinUrls.find((u) => u.secure) || joinUrls[0] || null;
    joinPanel.hidden = live && !pref;
    joinNone.hidden = !joinLoaded || !!pref;
    joinQr.hidden = !pref;
    joinUrl.textContent = pref ? pref.url : (joinLoaded ? '' : 'Finding the address...');
    if (pref) {
      const src = '/api/joinqr.png?u=' + encodeURIComponent(pref.url);
      if (src !== joinQrSrc) {
        joinQrSrc = src;
        joinQr.src = src;
      }
    }
    joinOthers.textContent = '';
    for (const u of joinUrls) {
      if (u === pref) continue;
      const d = document.createElement('div');
      d.textContent = u.label + ': ' + u.url;
      joinOthers.appendChild(d);
    }
    joinNote.hidden = live || !(pref && pref.secure);
  }

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
        lobby = v;
        lobbyVer = v.version;
        if ((!catalog || catalogVer !== v.catalogVersion) && !catalogLoading) loadCatalog(v.catalogVersion);
        renderAll();
      } catch (e) {
        failures += 1;
        await new Promise((r) => setTimeout(r, Math.min(5000, 300 * failures)));
      } finally {
        clearTimeout(timer);
      }
    }
  }

  setInterval(checkTiles, 500);
  loadJoinInfo();
  setInterval(loadJoinInfo, 10000);
  lobbyLoop();
})();
