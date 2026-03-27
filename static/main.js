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

  const wsProto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  const ws = new WebSocket(`${wsProto}//${location.host}/terminal`);
  ws.binaryType = 'arraybuffer';

  ws.onopen = function () {
    status.remove();
    fitAddon.fit();
    sendResize();
  };

  ws.onmessage = function (evt) {
    let data;
    if (evt.data instanceof ArrayBuffer) {
      const arr = new Uint8Array(evt.data);
      if (arr[0] === MSG_DATA) {
        term.write(arr.slice(1));
      }
    } else {
      // Plain text (e.g. status messages from server before binary protocol starts)
      term.write(evt.data);
    }
  };

  ws.onclose = function () {
    term.write('\r\n\x1b[31mConnection closed.\x1b[0m Press F5 to reconnect.\r\n');
  };

  ws.onerror = function () {
    term.write('\r\n\x1b[31mWebSocket error.\x1b[0m\r\n');
  };

  term.onData(function (data) {
    if (ws.readyState !== WebSocket.OPEN) return;
    const encoded = new TextEncoder().encode(data);
    const msg = new Uint8Array(1 + encoded.length);
    msg[0] = MSG_DATA;
    msg.set(encoded, 1);
    ws.send(msg);
  });

  function sendResize() {
    if (ws.readyState !== WebSocket.OPEN) return;
    const size = { cols: term.cols, rows: term.rows };
    const json = JSON.stringify(size);
    const encoded = new TextEncoder().encode(json);
    const msg = new Uint8Array(1 + encoded.length);
    msg[0] = MSG_RESIZE;
    msg.set(encoded, 1);
    ws.send(msg);
  }

  window.addEventListener('resize', function () {
    fitAddon.fit();
    sendResize();
  });
})();
