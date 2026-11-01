"use strict";
(() => {
  const $ = (s) => document.querySelector(s);
  const VERSION = new URL(document.currentScript.src).searchParams.get("v") || "dev";
  const store = {
    get(k) { try { return localStorage.getItem(k); } catch { return null; } },
    set(k, v) { try { localStorage.setItem(k, v); } catch { /* private mode */ } },
  };

  // ---------------------------------------------------------------- editor
  const editor = CodeMirror($("#editor"), {
    mode: "python",
    lineNumbers: true,
    indentUnit: 4,
    tabSize: 4,
    matchBrackets: true,
    autofocus: true,
    extraKeys: {
      "Ctrl-Enter": () => run(),
      "Cmd-Enter": () => run(),
      Tab: (cm) => cm.somethingSelected() ? cm.indentSelection("add") : cm.replaceSelection("    "),
      "Shift-Tab": (cm) => cm.indentSelection("subtract"),
    },
  });

  // ---------------------------------------------------------------- state
  let ws = null;
  let state = "idle"; // idle | waiting | booting | ready
  let running = false;
  let deadline = 0;
  let queueInfo = null;
  let globalStatus = null;
  let reconnectDelay = 1000;
  let term = null, fit = null, termOpen = false;
  let lastActive = 0;
  let version = null; // chosen playground version id (null until the server lists them)
  let wantedVersion = null; // from a share link or localStorage, applied once the list arrives

  // ---------------------------------------------------------------- output
  const out = $("#output");
  const OUT_LIMIT = 512 * 1024;
  let outLen = 0;

  // Program output is buffered and written once per animation frame, so a
  // chatty program can never make the page stutter.
  let pending = "", frameQueued = false;
  function flush() {
    frameQueued = false;
    if (!pending) return;
    const pane = $("#pane-output");
    const stick = pane.scrollHeight - pane.scrollTop - pane.clientHeight < 80;
    out.appendChild(document.createTextNode(pending));
    pending = "";
    if (stick) pane.scrollTop = pane.scrollHeight;
  }
  function clearOutput() { pending = ""; out.textContent = ""; outLen = 0; }
  function append(text) {
    if (outLen > OUT_LIMIT) return;
    outLen += text.length;
    if (outLen > OUT_LIMIT) text += "\n[output truncated in the browser]\n";
    pending += text;
    if (!frameQueued) { frameQueued = true; requestAnimationFrame(flush); }
  }
  function sys(text, cls = "sys") {
    flush();
    const node = Object.assign(document.createElement("span"), { className: cls, textContent: text + "\n" });
    out.appendChild(node);
    const pane = $("#pane-output");
    pane.scrollTop = pane.scrollHeight;
  }

  let noticeEl = null; // single, replaceable status line (queue position etc.)
  function notice(text) {
    flush();
    if (!noticeEl || !noticeEl.isConnected) {
      noticeEl = document.createElement("span");
      noticeEl.className = "notice";
      out.appendChild(noticeEl);
    }
    noticeEl.textContent = text + "\n";
  }
  function dropNotice() { if (noticeEl) noticeEl.remove(); noticeEl = null; }

  // ---------------------------------------------------------------- tabs
  function showTab(name) {
    document.querySelectorAll(".tab").forEach((t) => t.classList.toggle("active", t.dataset.tab === name));
    document.querySelectorAll(".pane").forEach((p) => p.classList.toggle("active", p.id === "pane-" + name));
    if (name === "terminal") openTerminal();
    if (name === "output") editor.refresh();
  }
  document.querySelectorAll(".tab").forEach((t) => t.addEventListener("click", () => showTab(t.dataset.tab)));

  // ---------------------------------------------------------------- status chip
  function fmtDur(s) {
    s = Math.max(0, Math.round(s));
    if (s < 60) return `${s}s`;
    return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}`;
  }
  function renderChip() {
    const chip = $("#chip");
    chip.className = "chip";
    let text = "";
    if (!ws || ws.readyState !== WebSocket.OPEN) {
      text = "offline, reconnecting…"; chip.classList.add("off");
    } else if (state === "waiting" && queueInfo) {
      text = `#${queueInfo.position} in queue · ~${fmtDur(queueInfo.eta_s)}`; chip.classList.add("busy");
    } else if (state === "booting") {
      text = "starting VM…"; chip.classList.add("busy");
    } else if (state === "ready") {
      text = deadline ? `VM ready · ${fmtDur((deadline - Date.now()) / 1000)} left` : "VM ready";
      chip.classList.add(deadline ? "busy" : "ok");
    } else if (globalStatus) {
      const free = globalStatus.slots - globalStatus.busy;
      text = free > 0 && globalStatus.waiting === 0
        ? `${free} of ${globalStatus.slots} VMs free`
        : `all VMs busy · ${globalStatus.waiting} waiting`;
      chip.classList.add(free > 0 && globalStatus.waiting === 0 ? "ok" : "busy");
    }
    chip.textContent = text;
    $("#end").hidden = state === "idle";
  }
  setInterval(renderChip, 1000);
  setInterval(() => send({ op: "ping" }), 30000); // keep the WebSocket alive through Cloudflare

  function setRunning(r) {
    running = r;
    $("#run").classList.toggle("running", r);
    $("#run-label").textContent = r ? "Stop" : "Run";
  }

  // ---------------------------------------------------------------- websocket
  // Messages sent while (re)connecting are held and flushed on open, so a
  // click right after page load or during a reconnect is never lost.
  let outbox = [];
  function flushOutbox() {
    if (!ws || ws.readyState !== WebSocket.OPEN) return;
    const pending = outbox;
    outbox = [];
    pending.forEach((m) => ws.send(JSON.stringify(m)));
  }
  function send(obj) {
    const holdable = obj.op === "run" || obj.op === "term";
    if (ws && ws.readyState === WebSocket.OPEN && (version || !holdable)) { ws.send(JSON.stringify(obj)); return true; }
    if (holdable) {
      outbox = outbox.filter((m) => m.op !== obj.op);
      outbox.push(obj);
      return true;
    }
    return false;
  }

  function connect() {
    ws = new WebSocket((location.protocol === "https:" ? "wss://" : "ws://") + location.host + "/ws");
    ws.binaryType = "arraybuffer";
    ws.onopen = () => {
      reconnectDelay = 1000;
      // Version first, so a held Run boots the right image. On the first
      // connection the version list hasn't arrived yet: setVersions flushes.
      if (version) { ws.send(JSON.stringify({ op: "version", id: version })); flushOutbox(); }
      renderChip();
    };
    ws.onclose = () => {
      if (state !== "idle") sys("[connection lost: your session has ended]");
      state = "idle"; termOpen = false; setRunning(false); renderChip();
      setTimeout(connect, reconnectDelay);
      reconnectDelay = Math.min(reconnectDelay * 2, 15000);
    };
    ws.onmessage = (ev) => {
      if (typeof ev.data !== "string") {
        if (term) term.write(new Uint8Array(ev.data));
        return;
      }
      let m;
      try { m = JSON.parse(ev.data); } catch { return; }
      handle(m);
      renderChip();
    };
  }

  function handle(m) {
    switch (m.t) {
      case "status":
        globalStatus = m;
        break;
      case "images":
        setVersions(m.images, m.default);
        break;
      case "queued":
        state = "waiting";
        queueInfo = m;
        notice(`Waiting for a free VM: you are #${m.position} in line (about ${fmtDur(m.eta_s)}). Keep this tab open.`);
        break;
      case "booting":
        state = "booting";
        notice(`Starting your microVM (${labelOf(m.image)})…`);
        break;
      case "ready":
        state = "ready";
        deadline = m.deadline_ms || 0;
        dropNotice();
        if (m.hello) {
          // Prefer the build's exact refs (e.g. master@b076ef4): a git build can
          // report the same package version as the last release.
          const v = versionList.find((x) => x.id === m.image) || {};
          $("#versions").textContent =
            `pythonbpf ${v.pythonbpf || m.hello.pythonbpf} · pylibbpf ${v.pylibbpf || m.hello.pylibbpf} · Linux ${m.hello.kernel}`;
        }
        break;
      case "lease":
        deadline = m.deadline_ms || 0;
        if (deadline) sys(`[others are waiting: your session ends in ${fmtDur((deadline - Date.now()) / 1000)}]`, "notice");
        break;
      case "idle_warning":
        sys(`[still there? your session ends in ${m.seconds}s unless you do something]`, "notice");
        break;
      case "vm_restart":
        state = "booting";
        termOpen = false;
        if (term) term.write("\r\n\x1b[33m[VM restarting]\x1b[0m\r\n");
        sys(`[${m.reason} Restarting the VM…]`, "notice");
        break;
      case "ended":
        state = "idle"; deadline = 0; termOpen = false; setRunning(false);
        dropNotice();
        sys(`[${m.reason}]`, "notice");
        if (term) term.write("\r\n\x1b[2m[session ended]\x1b[0m\r\n");
        break;
      case "error":
        setRunning(false);
        dropNotice();
        sys(m.msg, "err");
        break;
      case "start":
        break;
      case "out":
        append(m.data);
        break;
      case "ir":
        $("#ir").classList.remove("muted-empty");
        $("#ir").textContent = m.data;
        break;
      case "exit": {
        setRunning(false);
        const secs = m.ms ? ` in ${(m.ms / 1000).toFixed(2)}s` : "";
        if (m.reason && m.reason !== "exited") sys(`[stopped: ${m.reason}${secs}]`, "notice");
        else sys(`[exited with code ${m.code}${secs}]`);
        break;
      }
      case "term_open":
        termOpen = true;
        if (term) { term.focus(); sendResize(); }
        break;
      case "term_closed":
        termOpen = false;
        break;
    }
  }

  // ---------------------------------------------------------------- versions
  let versionList = [];
  const labelOf = (id) => (versionList.find((v) => v.id === id) || { label: id || "default" }).label;
  function setVersions(list, def) {
    versionList = list;
    const sel = $("#version");
    sel.textContent = "";
    for (const v of list) {
      const detail = [v.pythonbpf && `pythonbpf ${v.pythonbpf}`, v.pylibbpf && `pylibbpf ${v.pylibbpf}`].filter(Boolean).join(" · ");
      sel.appendChild(Object.assign(document.createElement("option"), { value: v.id, textContent: v.label, title: detail }));
    }
    const pick = [wantedVersion, store.get("pg-version"), def].find((id) => id && list.some((v) => v.id === id));
    sel.value = pick;
    sel.hidden = list.length < 2;
    if (pick !== version) { version = pick; send({ op: "version", id: pick }); }
    flushOutbox();
  }
  $("#version").addEventListener("change", (e) => {
    version = e.target.value;
    store.set("pg-version", version);
    if (state === "ready" || state === "booting") sys(`[switching to ${labelOf(version)}: restarting your VM]`, "notice");
    send({ op: "version", id: version });
  });

  // ---------------------------------------------------------------- actions
  let initDone;
  const initialized = new Promise((r) => { initDone = r; });
  async function run() {
    if (running) { send({ op: "kill" }); return; }
    setRunning(true);
    await initialized; // never run an editor that is still loading its example
    send({ op: "run", code: editor.getValue() });
    clearOutput();
    $("#ir").textContent = "";
    showTab("output");
    sys(state === "ready" ? "Running…" : "Running (a VM will be started for you first)…");
  }
  $("#run").addEventListener("click", run);
  $("#end").addEventListener("click", () => send({ op: "release" }));

  editor.on("change", () => {
    store.set("pg-draft", editor.getValue());
    if (state === "ready" && Date.now() - lastActive > 15000) { lastActive = Date.now(); send({ op: "active" }); }
  });

  // ---------------------------------------------------------------- terminal
  function sendResize() {
    if (!term || !termOpen) return;
    send({ op: "resize", cols: term.cols, rows: term.rows });
  }
  function termTheme() {
    const cs = getComputedStyle(document.documentElement);
    return {
      background: cs.getPropertyValue("--bg").trim(),
      foreground: cs.getPropertyValue("--text").trim(),
      cursor: cs.getPropertyValue("--text").trim(),
      selectionBackground: cs.getPropertyValue("--sel").trim(),
    };
  }
  function loadScript(src) {
    return new Promise((ok, fail) => {
      const el = Object.assign(document.createElement("script"), { src, onload: ok, onerror: fail });
      document.head.appendChild(el);
    });
  }
  let xtermLoading = null;
  function loadXterm() {
    if (!xtermLoading) {
      document.head.appendChild(Object.assign(document.createElement("link"), { rel: "stylesheet", href: `vendor/xterm.css?v=${VERSION}` }));
      xtermLoading = loadScript(`vendor/xterm.js?v=${VERSION}`).then(() => loadScript(`vendor/addon-fit.js?v=${VERSION}`));
    }
    return xtermLoading;
  }
  async function openTerminal() {
    if (!term) {
      try { await loadXterm(); } catch { toast("Could not load the terminal"); return; }
      if (term) return openTerminal();
      term = new Terminal({
        fontFamily: getComputedStyle(document.documentElement).getPropertyValue("--mono"),
        fontSize: 13,
        cursorBlink: true,
        theme: termTheme(),
        scrollback: 2000,
      });
      fit = new FitAddon.FitAddon();
      term.loadAddon(fit);
      term.open($("#terminal"));
      term.onData((d) => {
        if (termOpen && ws && ws.readyState === WebSocket.OPEN) ws.send(new TextEncoder().encode(d));
      });
      matchMedia("(prefers-color-scheme: dark)").addEventListener("change", () => { term.options.theme = termTheme(); });
      new ResizeObserver(() => { try { fit.fit(); } catch {} sendResize(); }).observe($("#terminal"));
      term.write("\x1b[2mA root shell inside your microVM. Try: bpftool prog list, ls /opt/pg/examples\x1b[0m\r\n");
    }
    setTimeout(() => { try { fit.fit(); } catch {} }, 0);
    if (!termOpen) {
      if (send({ op: "term", cols: term.cols, rows: term.rows }) && state !== "ready") {
        term.write("\x1b[2m[waiting for your VM…]\x1b[0m\r\n");
      }
    }
    term.focus();
  }

  // ---------------------------------------------------------------- splitter
  (() => {
    const main = $("#main"), split = $("#splitter");
    const saved = store.get("pg-panel-h");
    if (saved) main.style.setProperty("--panel-h", saved);
    split.addEventListener("pointerdown", (e) => {
      split.setPointerCapture(e.pointerId);
      const move = (ev) => {
        const r = main.getBoundingClientRect();
        const h = Math.min(Math.max(r.bottom - ev.clientY, 90), r.height - 80);
        main.style.setProperty("--panel-h", `${h}px`);
      };
      const up = () => {
        split.removeEventListener("pointermove", move);
        store.set("pg-panel-h", main.style.getPropertyValue("--panel-h"));
        editor.refresh();
      };
      split.addEventListener("pointermove", move);
      split.addEventListener("pointerup", up, { once: true });
    });
  })();

  // ---------------------------------------------------------------- share
  const b64url = (bytes) => {
    let s = "";
    for (let i = 0; i < bytes.length; i += 0x8000) s += String.fromCharCode(...bytes.subarray(i, i + 0x8000));
    return btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  };
  const unb64url = (s) => Uint8Array.from(atob(s.replace(/-/g, "+").replace(/_/g, "/")), (c) => c.charCodeAt(0));
  async function pipe(bytes, stream) {
    return new Uint8Array(await new Response(new Blob([bytes]).stream().pipeThrough(stream)).arrayBuffer());
  }
  $("#share").addEventListener("click", async () => {
    try {
      const packed = await pipe(new TextEncoder().encode(editor.getValue()), new CompressionStream("deflate-raw"));
      const v = version ? `v=${encodeURIComponent(version)}&` : "";
      const url = `${location.origin}/#${v}code=${b64url(packed)}`;
      history.replaceState(null, "", url);
      await navigator.clipboard.writeText(url);
      toast("Link copied to clipboard");
    } catch {
      toast("Copy the link from the address bar");
    }
  });
  async function codeFromHash() {
    const params = new URLSearchParams(location.hash.slice(1));
    if (params.get("v")) wantedVersion = params.get("v");
    const code = params.get("code");
    const m = code && code.match(/^([\w-]+)$/);
    if (!m) return null;
    try { return new TextDecoder().decode(await pipe(unb64url(m[1]), new DecompressionStream("deflate-raw"))); }
    catch { return null; }
  }

  let toastTimer;
  function toast(msg) {
    const t = $("#toast");
    t.textContent = msg;
    t.classList.add("show");
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => t.classList.remove("show"), 1800);
  }

  // ---------------------------------------------------------------- examples
  async function loadExample(name) {
    const r = await fetch(`examples/${encodeURIComponent(name)}`);
    if (!r.ok) return;
    editor.setValue(await r.text());
    editor.clearHistory();
    history.replaceState(null, "", "/");
  }
  async function init() {
    let list = [];
    try { list = await (await fetch("examples.json")).json(); } catch {}
    const sel = $("#examples");
    for (const name of list) {
      const label = name.replace(/^\d+_/, "").replace(/\.py$/, "").replace(/_/g, " ");
      sel.appendChild(Object.assign(document.createElement("option"), { value: name, textContent: label }));
    }
    sel.addEventListener("change", () => { loadExample(sel.value); sel.selectedIndex = 0; });

    const shared = await codeFromHash();
    const draft = store.get("pg-draft");
    if (shared !== null) editor.setValue(shared);
    else if (draft) editor.setValue(draft);
    else if (list.length) await loadExample(list[0]);
    editor.clearHistory();
    initDone();
    connect();
    renderChip();
  }
  init();
})();
