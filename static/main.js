(function () {
  const MSG_DATA = 0x00;
  const MSG_RESIZE = 0x01;

  const term = new Terminal({
    cursorBlink: true,
    cursorStyle: 'bar',
    cursorWidth: 2,
    fontSize: 14,
    fontFamily: '"JetBrains Mono", "Fira Code", "Cascadia Code", monospace',
    theme: {
      background: '#1a1a1a',
      foreground: '#d4d4d4',
      cursor: '#d4d4d4',
      scrollbar: '#333',
      black: '#1a1a1a',
      brightBlack: '#555',
      red: '#f48771',
      brightRed: '#f48771',
      green: '#89d185',
      brightGreen: '#89d185',
      yellow: '#cca700',
      brightYellow: '#cca700',
      blue: '#6796e6',
      brightBlue: '#6796e6',
      magenta: '#b267e6',
      brightMagenta: '#b267e6',
      cyan: '#56b6c2',
      brightCyan: '#56b6c2',
      white: '#d4d4d4',
      brightWhite: '#ffffff',
    },
  });

  const fitAddon = new FitAddon.FitAddon();
  term.loadAddon(fitAddon);

  const container = document.getElementById('terminal-container');
  const status = document.getElementById('status');
  term.open(container);

  let ws = null;

  // Persistent input handler: forwards keystrokes to WS when open.
  term.onData(function (data) {
    if (!ws || ws.readyState !== WebSocket.OPEN) return;
    const encoded = new TextEncoder().encode(data);
    const msg = new Uint8Array(1 + encoded.length);
    msg[0] = MSG_DATA;
    msg.set(encoded, 1);
    ws.send(msg);
  });

  function sendResize() {
    if (!ws || ws.readyState !== WebSocket.OPEN) return;
    const size = { cols: term.cols, rows: term.rows };
    const encoded = new TextEncoder().encode(JSON.stringify(size));
    const msg = new Uint8Array(1 + encoded.length);
    msg[0] = MSG_RESIZE;
    msg.set(encoded, 1);
    ws.send(msg);
  }

  function connect() {
    if (ws) {
      ws.onclose = null;
      ws.onerror = null;
      ws.close();
    }

    const wsProto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    ws = new WebSocket(`${wsProto}//${location.host}/terminal`);
    ws.binaryType = 'arraybuffer';

    ws.onopen = function () {
      if (status) status.remove();
      fitAddon.fit();
      sendResize();
    };

    ws.onmessage = function (evt) {
      if (evt.data instanceof ArrayBuffer) {
        const arr = new Uint8Array(evt.data);
        if (arr[0] === MSG_DATA) {
          term.write(arr.slice(1));
        }
      } else {
        // Plain text status messages from server
        term.write(evt.data);
      }
    };

    ws.onclose = function () {
      term.write('\r\n\x1b[31mConnection closed.\x1b[0m \x1b[33mPress any key to reconnect...\x1b[0m\r\n');
      // One-shot listener: first keypress reconnects, then removes itself.
      const disposable = term.onData(function () {
        disposable.dispose();
        term.write('\x1b[33mReconnecting...\x1b[0m\r\n');
        connect();
      });
    };

    ws.onerror = function () {
      term.write('\r\n\x1b[31mWebSocket error.\x1b[0m\r\n');
    };
  }

  const restartBtn = document.getElementById('restart-btn');
  restartBtn.addEventListener('click', function () {
    restartBtn.disabled = true;
    restartBtn.textContent = 'restarting...';
    fetch('/api/restart', { method: 'POST' }).finally(function () {
      restartBtn.disabled = false;
      restartBtn.textContent = 'restart';
      term.write('\r\n\x1b[33mRestarting...\x1b[0m\r\n');
      connect();
    });
  });

  const usageEl = document.getElementById('usage');

  function updateUsage() {
    fetch('/api/usage').then(function (r) {
      if (!r.ok) return;
      return r.json();
    }).then(function (d) {
      if (!d || !usageEl) return;
      usageEl.textContent = 'cpu ' + d.cpu_percent.toFixed(1) + '%  mem ' + d.memory_percent.toFixed(1) + '%';
    }).catch(function () {});
  }

  updateUsage();
  setInterval(updateUsage, 5000);

  window.addEventListener('resize', function () {
    fitAddon.fit();
    sendResize();
  });

  connect();
})();
