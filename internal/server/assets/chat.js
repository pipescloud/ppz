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
  const handleSel = document.getElementById("chat-handle");

  // The handle the viewer is acting as (the "send as" identity — the web
  // analog of the CLI current handle). "" when the user owns none, in which
  // case the composer stays disabled. Stamped as the sender on send, and
  // passed as ?as= so our own messages read back as "you".
  function currentHandle() {
    return handleSel ? handleSel.value : "";
  }

  let ws = null;
  let current = null; // { kind, target, entryEl }
  let seen = new Set();

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

    const meta = document.createElement("span");
    meta.className = "chat-msg-meta";
    const time = document.createElement("span");
    time.className = "chat-msg-time";
    time.textContent = hm(m.created_at);
    const who = document.createElement("span");
    who.className = "chat-msg-sender";
    who.textContent = mine ? "you" : (m.sender || "(unknown)");
    meta.appendChild(time);
    meta.appendChild(who);

    const body = document.createElement("span");
    body.className = "chat-msg-body";
    body.textContent = m.payload || "";

    row.appendChild(meta);
    row.appendChild(body);
    logEl.appendChild(row);

    // Stick to the bottom only when the reader is already there, so a
    // scrolled-up reader isn't yanked down by an incoming message.
    const atBottom = logEl.scrollHeight - logEl.scrollTop - logEl.clientHeight < 40;
    if (atBottom) logEl.scrollTop = logEl.scrollHeight;
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
  }

  shell.querySelectorAll(".chat-entry").forEach((entryEl) => {
    entryEl.addEventListener("click", () => selectEntry(entryEl));
  });

  // Changing the acting handle re-opens the current window so history re-labels
  // (and the live follow re-tags) under the new "you".
  if (handleSel) {
    handleSel.addEventListener("change", () => {
      if (current) selectEntry(current.entryEl);
    });
  }

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
})();
