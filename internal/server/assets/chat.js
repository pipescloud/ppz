// Web `ppz chat` console.
//
// Server-rendered roster on the left (three sections); this script drives the
// right pane: selecting an entry loads its buffered history over
// /chat/messages, then opens a WebSocket on /chat/ws for the live tail.
// Sending POSTs to /chat/send. Every message carries a stable id; the log
// dedups by id, so the history snapshot and the WS replay can overlap safely.

(function () {
  const shell = document.querySelector(".chat-shell");
  if (!shell) return;

  const org = shell.getAttribute("data-org");

  const titleEl = document.getElementById("chat-title");
  const logEl = document.getElementById("chat-log");
  const composer = document.getElementById("chat-composer");
  const input = document.getElementById("chat-input");
  const sendBtn = document.getElementById("chat-send");
  const statusEl = document.getElementById("chat-status");

  // The handle the viewer is acting as (the "send as" identity — the web
  // analog of the CLI current handle), rendered server-side for this view. ""
  // when the user owns none, in which case the composer stays disabled.
  // Stamped as the sender on send, and passed as ?as= so our own messages read
  // back as "you". Switching identity is a navigation (the picker's ?as= links).
  function currentHandle() {
    return shell.getAttribute("data-acting") || "";
  }

  let ws = null;
  let current = null; // { kind, target, entryEl }
  let seen = new Set();
  let markReadTimer = null;

  // Advance the server-side read cursor for a conversation (clears its unread
  // badge). `as` scopes DM read state to the acting identity.
  function markRead(kind, target) {
    fetch("/orgs/" + encodeURIComponent(org) + "/chat/read", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ kind, target, as: currentHandle() }),
    }).catch(() => {});
  }

  // Debounced mark-read for the open window, so reading a live burst is one
  // cursor write, not one per message.
  function scheduleMarkRead() {
    if (!current) return;
    clearTimeout(markReadTimer);
    const { kind, target } = current;
    markReadTimer = setTimeout(() => markRead(kind, target), 800);
  }

  // Set (or clear) a roster row's unread badge.
  function setUnreadBadge(entryEl, n) {
    let b = entryEl.querySelector(".chat-unread");
    if (n > 0) {
      if (!b) {
        b = document.createElement("span");
        b.className = "chat-unread";
        b.setAttribute("data-unread", "");
        entryEl.appendChild(b);
      }
      b.textContent = String(n);
    } else if (b) {
      b.remove();
    }
  }

  function setStatus(text, kind) {
    if (!statusEl) return;
    if (!text) {
      statusEl.hidden = true;
      return;
    }
    statusEl.hidden = false;
    statusEl.textContent = text;
    statusEl.className = "chat-status " + (kind || "");
  }

  // hh:mm local time from an ISO-8601 UTC timestamp.
  function hm(iso) {
    const d = new Date(iso);
    if (isNaN(d.getTime())) return "";
    const p = (n) => String(n).padStart(2, "0");
    return p(d.getHours()) + ":" + p(d.getMinutes());
  }

  function renderMessage(m) {
    if (m.id && seen.has(m.id)) return;
    if (m.id) seen.add(m.id);

    // Trust our own identity over the server's `you` flag: the live tail
    // may arrive on a transport without the session (empty server-side me),
    // but the browser always knows who it is.
    const h = currentHandle();
    const mine = m.you || (!!h && m.sender === h);

    const row = document.createElement("div");
    row.className = "chat-msg" + (mine ? " chat-msg-you" : "");
    row.setAttribute("data-msg-id", m.id || "");
    row.setAttribute("data-msg", (m.sender || "") + ":" + (m.payload || ""));

    // TUI-style single row: time · sender · body (body hanging-indented).
    const time = document.createElement("span");
    time.className = "chat-msg-time";
    time.textContent = hm(m.created_at);
    const who = document.createElement("span");
    who.className = "chat-msg-sender";
    who.textContent = mine ? "you" : (m.sender || "(unknown)");
    const body = document.createElement("span");
    body.className = "chat-msg-body";
    body.textContent = m.payload || "";

    row.appendChild(time);
    row.appendChild(who);
    row.appendChild(body);
    logEl.appendChild(row);

    // Stick to the bottom only when the reader is already there, so a
    // scrolled-up reader isn't yanked down by an incoming message.
    const atBottom = logEl.scrollHeight - logEl.scrollTop - logEl.clientHeight < 40;
    if (atBottom) logEl.scrollTop = logEl.scrollHeight;

    // Seeing a message in the open window means we've read up to it.
    scheduleMarkRead();
  }

  function clearLog() {
    logEl.innerHTML = "";
    logEl.removeAttribute("data-empty");
    seen = new Set();
  }

  function closeWS() {
    if (ws) {
      try { ws.close(); } catch (_) {}
      ws = null;
    }
  }

  function openWS(kind, target) {
    const proto = location.protocol === "https:" ? "wss:" : "ws:";
    const q = "?kind=" + encodeURIComponent(kind) + "&target=" + encodeURIComponent(target) +
      "&as=" + encodeURIComponent(currentHandle());
    const url = proto + "//" + location.host + "/orgs/" + encodeURIComponent(org) + "/chat/ws" + q;
    const sock = new WebSocket(url);
    ws = sock;
    // Guard every handler on `sock === ws`: a socket we've switched away from
    // fires its close/error asynchronously and must not clobber the status
    // line (or render into) the window that's now current.
    sock.onopen = () => { if (sock === ws) setStatus("live", "ok"); };
    sock.onerror = () => { if (sock === ws) setStatus("connection error", "err"); };
    sock.onclose = () => { if (sock === ws) setStatus("disconnected", "err"); };
    sock.onmessage = (ev) => {
      if (sock !== ws) return;
      try {
        renderMessage(JSON.parse(ev.data));
      } catch (_) { /* drop malformed */ }
    };
  }

  function selectEntry(entryEl) {
    const kind = entryEl.getAttribute("data-chat-kind");
    const target = entryEl.getAttribute("data-chat-target");
    // Server-computed header title (TUI-parity: "claude · dm · online|working",
    // "#backend · pipe (uncollared)", …); fall back to the raw entry key.
    const displayEntry =
      entryEl.getAttribute("data-chat-title") ||
      entryEl.getAttribute("data-chat-entry") ||
      target;

    // Toggle active styling.
    document.querySelectorAll(".chat-entry.active")
      .forEach((e) => e.classList.remove("active"));
    entryEl.classList.add("active");

    current = { kind, target, entryEl };
    closeWS();
    clearLog();

    titleEl.textContent = displayEntry;
    composer.hidden = false;
    // Only composable when acting as a handle you own; otherwise the window is
    // view-only and the no-handle notice explains why.
    const canSend = !!currentHandle();
    input.disabled = !canSend;
    sendBtn.disabled = !canSend;
    if (canSend) input.focus();
    setStatus("loading…", "");

    // The WS both replays retained history and follows live, so there's no
    // separate history fetch — the backlog crosses the wire once. (The
    // /chat/messages JSON endpoint still exists for scripts / the e2e suite.)
    openWS(kind, target);

    // Opening a window reads it: clear its badge now and advance the cursor.
    setUnreadBadge(entryEl, 0);
    markRead(kind, target);

    // Mobile master-detail: swap from the roster to the conversation pane.
    shell.setAttribute("data-view", "chat");
  }

  const backBtn = document.getElementById("chat-back");
  if (backBtn) {
    backBtn.addEventListener("click", () => shell.setAttribute("data-view", "roster"));
  }

  shell.querySelectorAll(".chat-entry").forEach((entryEl) => {
    entryEl.addEventListener("click", () => selectEntry(entryEl));
  });

  // Identity picker: a custom dropdown (native <select> can't be themed). The
  // menu items are ?as=<handle> links, so choosing one navigates and re-scopes
  // the whole view server-side — no JS needed beyond open/close.
  const picker = document.querySelector(".chat-picker");
  const pickerBtn = document.getElementById("chat-handle-btn");
  if (picker && pickerBtn) {
    pickerBtn.addEventListener("click", (ev) => {
      ev.stopPropagation();
      picker.setAttribute("data-open", picker.getAttribute("data-open") === "true" ? "false" : "true");
    });
    document.addEventListener("click", () => picker.setAttribute("data-open", "false"));
    document.addEventListener("keydown", (ev) => {
      if (ev.key === "Escape") picker.setAttribute("data-open", "false");
    });
  }

  // Auto-grow the composer textarea up to a max, then scroll internally, and
  // light up the send button (accent) once there's something to send.
  function autogrow() {
    input.style.height = "auto";
    input.style.height = Math.min(input.scrollHeight, 160) + "px";
    sendBtn.classList.toggle("ready", input.value.trim().length > 0 && !sendBtn.disabled);
  }
  input.addEventListener("input", autogrow);
  // Enter sends; Shift+Enter inserts a newline (Slack-style).
  input.addEventListener("keydown", (ev) => {
    if (ev.key === "Enter" && !ev.shiftKey) {
      ev.preventDefault();
      composer.requestSubmit();
    }
  });

  composer.addEventListener("submit", async (ev) => {
    ev.preventDefault();
    if (!current) return;
    const as = currentHandle();
    if (!as) return; // no handle to send as
    const payload = input.value.trim();
    if (!payload) return;
    // Clear optimistically, but keep the text so we can restore it if the
    // send fails — a dropped message shouldn't cost the user their typing.
    input.value = "";
    autogrow();
    try {
      const res = await fetch(
        "/orgs/" + encodeURIComponent(org) + "/chat/send",
        {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ kind: current.kind, target: current.target, payload, as }),
        });
      if (!res.ok) {
        setStatus("send failed (" + res.status + ")", "err");
        if (!input.value) input.value = payload;
        return;
      }
      // The WS follow echoes our own publish back, so we don't render it
      // here — dedup by id would drop the double anyway. Repaint from the
      // POST response only if the WS isn't connected.
      if (!ws || ws.readyState !== WebSocket.OPEN) {
        try { renderMessage(await res.json()); } catch (_) {}
      }
      setStatus("live", "ok");
    } catch (_) {
      setStatus("send error", "err");
      if (!input.value) input.value = payload;
    }
  });

  // ── Roster mutation: add / remove pipe (TUI `a` / `-` parity) ──────────
  // Both reload the page afterward so the server re-renders the roster,
  // handle picker and counts — no client-side roster surgery to drift.

  const addPipeForm = document.getElementById("chat-add-pipe");
  const addPipeInput = document.getElementById("chat-add-pipe-name");
  if (addPipeForm) {
    addPipeForm.addEventListener("submit", async (ev) => {
      ev.preventDefault();
      const name = addPipeInput.value.trim();
      if (!name) return;
      addPipeInput.disabled = true;
      try {
        const res = await fetch("/orgs/" + encodeURIComponent(org) + "/chat/pipes", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ name }),
        });
        if (res.ok) {
          location.reload();
          return;
        }
        setStatus("add pipe failed (" + res.status + ")", "err");
      } catch (_) {
        setStatus("add pipe error", "err");
      }
      addPipeInput.disabled = false;
    });
  }

  shell.querySelectorAll(".chat-remove").forEach((btn) => {
    btn.addEventListener("click", async (ev) => {
      ev.stopPropagation(); // don't also select the row
      const target = btn.getAttribute("data-remove-target");
      if (!target || !window.confirm('Remove pipe "' + target + '"?')) return;
      btn.disabled = true;
      try {
        const res = await fetch(
          "/orgs/" + encodeURIComponent(org) + "/chat/pipes?target=" + encodeURIComponent(target),
          { method: "DELETE" });
        if (res.ok) {
          location.reload();
          return;
        }
        setStatus("remove pipe failed (" + res.status + ")", "err");
      } catch (_) {
        setStatus("remove pipe error", "err");
      }
      btn.disabled = false;
    });
  });

  // ── Live roster refresh (TUI `who`-poll parity) ───────────────────────
  // Re-poll agent liveness + counts every 2.5s and patch the existing rows
  // in place (dots, state label, header title, top-bar counts). Row add/
  // remove still needs a reload — this only keeps liveness fresh, which is
  // what actually changes second-to-second.
  const countsEl = document.querySelector(".chat-counts");
  async function pollRoster() {
    // Skip while the tab is backgrounded — no point fanning out god's-eye
    // JetStream reads (heartbeats + per-window seq + inbox drain) nobody's
    // watching. The poll resumes on the next tick when the tab is visible.
    if (document.hidden) return;
    let data;
    try {
      const res = await fetch("/orgs/" + encodeURIComponent(org) +
        "/chat/roster?as=" + encodeURIComponent(currentHandle()),
        { headers: { Accept: "application/json" } });
      if (!res.ok) return;
      data = await res.json();
    } catch (_) { return; }

    const byKey = {};
    shell.querySelectorAll(".chat-entry").forEach((e) => {
      byKey[e.getAttribute("data-chat-entry")] = e;
    });
    (data.agents || []).forEach((a) => {
      const el = byKey["agent:" + a.target];
      if (!el) return;
      el.setAttribute("data-chat-status", a.status);
      el.setAttribute("data-chat-state", a.state || "");
      el.setAttribute("data-chat-title", a.title);
      const dot = el.querySelector(".chat-dot");
      if (dot) dot.className = "chat-dot chat-dot-" + a.status;
      let stateEl = el.querySelector(".chat-entry-state");
      if (a.state) {
        if (!stateEl) {
          stateEl = document.createElement("span");
          stateEl.className = "chat-entry-state";
          el.appendChild(stateEl);
        }
        stateEl.textContent = a.state;
      } else if (stateEl) {
        stateEl.remove();
      }
      // Keep the open window's header title fresh too.
      if (current && current.entryEl === el) titleEl.textContent = a.title;
    });
    // Unread badges for every row. The open window stays 0 (we're reading it
    // and the debounced mark-read keeps its cursor current).
    const badges = (arr, prefix) => (arr || []).forEach((e) => {
      const el = byKey[prefix + e.target];
      if (!el) return;
      const n = current && current.entryEl === el ? 0 : (e.unread || 0);
      setUnreadBadge(el, n);
    });
    badges(data.agents, "agent:");
    badges(data.inboxes, "inbox:");
    badges(data.pipes, "pipe:");

    if (countsEl && data.online != null) {
      countsEl.textContent = data.online + " online · " +
        (data.agents || []).length + " agents · " + (data.pipes || []).length + " pipes";
    }
  }
  setInterval(pollRoster, 2500);

  // ── Keyboard navigation (TUI parity: ↑/↓ · j/k move, enter open, esc back)
  // Roster entries are native <button>s, so Enter activates the focused row
  // (→ selectEntry) for free; we just move focus and handle Esc.
  document.addEventListener("keydown", (ev) => {
    const t = ev.target;
    const typing = t && (t.tagName === "INPUT" || t.tagName === "TEXTAREA" || t.tagName === "SELECT");
    if (ev.key === "Escape") {
      if (typing && t === input) {
        input.blur();
        if (current) current.entryEl.focus();
        ev.preventDefault();
      }
      return;
    }
    if (typing) return;
    const down = ev.key === "ArrowDown" || ev.key === "j";
    const up = ev.key === "ArrowUp" || ev.key === "k";
    if (!down && !up) return;
    const entries = Array.from(shell.querySelectorAll(".chat-entry"));
    if (!entries.length) return;
    const idx = entries.indexOf(document.activeElement);
    let next = idx === -1 ? 0 : (down ? idx + 1 : idx - 1);
    next = Math.max(0, Math.min(entries.length - 1, next));
    entries[next].focus();
    ev.preventDefault();
  });
})();
