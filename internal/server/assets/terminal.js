// Live terminal viewer with optional control.
//
// Mounts xterm.js on #terminal, opens a binary WebSocket against the path
// in data-ws-path, and writes every received frame straight into the
// emulator. If the server says this viewer may write the terminal's stdin
// ({"type":"control","writable":true}), "Take control" asks the server to
// acquire the terminal's write-lease; while it's held, keystrokes go back
// up the socket as binary frames. See internal/server/terminal_control.go.

(function () {
  const root = document.getElementById("terminal");
  if (!root) return;

  const wsPath = root.getAttribute("data-ws-path");
  if (!wsPath) return;

  // Pick xterm.js colours so the empty viewport (rows beyond rendered
  // content — a 105×57 pty with 6 lines of content has ~50 blank rows)
  // blends into the page rather than reading as a giant black slab.
  // Match style.css's --bg / --text via prefers-color-scheme so the
  // terminal follows the site's light/dark mode.
  const dark = window.matchMedia &&
    window.matchMedia("(prefers-color-scheme: dark)").matches;
  const theme = dark
    ? { background: "#1a1a18", foreground: "#ececea", cursor: "#ececea" }
    : { background: "#fafaf9", foreground: "#1c1c1a", cursor: "#1c1c1a" };

  const term = new Terminal({
    convertEol: false,            // keep raw \r\n from the pty
    cursorBlink: false,
    disableStdin: true,           // until this viewer holds control
    fontFamily: 'Menlo, Monaco, "Courier New", monospace',
    fontSize: 13,
    scrollback: 5000,
    theme,
  });
  term.open(root);

  const status = document.getElementById("terminal-status");
  // state: connecting | live | error — drives the toolbar's status dot.
  function setStatus(text, state) {
    if (!status) return;
    status.textContent = text;
    status.setAttribute("data-state", state);
  }

  const proto = location.protocol === "https:" ? "wss:" : "ws:";
  const url = proto + "//" + location.host + wsPath;
  const ws = new WebSocket(url);
  ws.binaryType = "arraybuffer";

  ws.onopen = () => setStatus("live", "live");
  ws.onerror = () => setStatus("connection error", "error");
  ws.onclose = (ev) => {
    setStatus("disconnected (" + ev.code + ")", "error");
    // The server releases a held lease when the socket goes; stop
    // offering controls the page can no longer exercise.
    term.options.disableStdin = true;
    root.classList.remove("controlling");
    if (toggle) toggle.hidden = true;
    if (fitBtn) fitBtn.hidden = true;
    if (stateEl) stateEl.textContent = "";
  };
  // --- control ---
  const toggle = document.querySelector("[data-control-toggle]");
  const fitBtn = document.querySelector("[data-fit]");
  const stateEl = document.querySelector("[data-control-state]");
  const control = { writable: false, resizable: false, you: "", holder: "", error: "" };

  function renderControl() {
    if (!toggle) return;
    const mine = control.holder !== "" && control.holder === control.you;
    term.options.disableStdin = !mine;
    root.classList.toggle("controlling", mine);
    toggle.hidden = !control.writable;
    if (fitBtn) fitBtn.hidden = !control.resizable;
    toggle.textContent = mine ? "Release control" : "Take control";
    let text;
    if (control.error) text = control.error;
    else if (mine) text = "you're in control — keystrokes go to the terminal";
    else if (control.holder) text = "controlled by " + control.holder;
    else if (control.writable) text = "view only";
    else text = "view only — you don't have write access to this terminal";
    stateEl.textContent = text;
    stateEl.className = "term-control-state" +
      (control.error ? " err" : mine ? " mine" : "");
    if (mine) term.focus();
  }

  if (toggle) {
    toggle.addEventListener("click", () => {
      if (ws.readyState !== WebSocket.OPEN) return;
      const mine = control.holder !== "" && control.holder === control.you;
      control.error = "";
      ws.send(JSON.stringify({ type: mine ? "release" : "take" }));
    });
  }

  // Fit to window: work out how many cells fit the space below the
  // toolbar from the terminal's own rendered cell size, and ask the host
  // to resize to that. The host's "resize" echo then resizes every
  // viewer, this one included — nothing is resized locally first.
  function fitSize() {
    const screen = root.querySelector(".xterm-screen");
    const row = root.querySelector(".xterm-rows > div");
    if (!screen || !row || !term.cols) return null;
    const cellW = screen.getBoundingClientRect().width / term.cols;
    const cellH = row.getBoundingClientRect().height;
    if (!(cellW > 0) || !(cellH > 0)) return null;
    const availW = document.documentElement.clientWidth - 48; // page gutters + border
    const availH = window.innerHeight - root.getBoundingClientRect().top - 36;
    return {
      cols: Math.max(20, Math.min(1000, Math.floor(availW / cellW))),
      rows: Math.max(5, Math.min(1000, Math.floor(availH / cellH))),
    };
  }
  if (fitBtn) {
    fitBtn.addEventListener("click", () => {
      const size = fitSize();
      if (!size || ws.readyState !== WebSocket.OPEN) return;
      control.error = "";
      ws.send(JSON.stringify({ type: "setsize", cols: size.cols, rows: size.rows }));
    });
  }

  const encoder = new TextEncoder();
  term.onData((data) => {
    if (ws.readyState === WebSocket.OPEN && control.holder === control.you) {
      ws.send(encoder.encode(data));
    }
  });

  ws.onmessage = (ev) => {
    if (typeof ev.data === "string") {
      // Text frames are JSON: resize events relayed from <handle>.stdctrl,
      // and terminal-control state from the server. Dispatch by type;
      // ignore unknown types so the wire can grow without breaking the
      // viewer.
      try {
        const ctrl = JSON.parse(ev.data);
        if (!ctrl) return;
        if (ctrl.type === "resize" &&
            Number.isFinite(ctrl.cols) && Number.isFinite(ctrl.rows) &&
            ctrl.cols > 0 && ctrl.rows > 0) {
          term.resize(ctrl.cols, ctrl.rows);
        } else if (ctrl.type === "control") {
          control.writable = !!ctrl.writable;
          control.resizable = !!ctrl.resizable;
          control.you = ctrl.you || "";
          renderControl();
        } else if (ctrl.type === "lease") {
          control.holder = ctrl.holder || "";
          control.error = "";
          renderControl();
        } else if (ctrl.type === "control-error") {
          control.error = ctrl.error || "control failed";
          renderControl();
        }
      } catch (_) { /* malformed json — drop */ }
    } else {
      term.write(new Uint8Array(ev.data));
    }
  };
})();
