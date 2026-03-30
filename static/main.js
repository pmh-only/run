(function () {
  const MSG_DATA = 0x00;
  const MSG_RESIZE = 0x01;
  const NUM_TABS = 6;

  const termOptions = {
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
  };

  // Build one Terminal + FitAddon per tab.
  const tabs = Array.from({ length: NUM_TABS }, function (_, i) {
    var container = document.getElementById('tab-' + i);
    var term = new Terminal(termOptions);
    var fitAddon = new FitAddon.FitAddon();
    term.loadAddon(fitAddon);
    term.open(container);

    // Persistent input handler — forwards to WS when open.
    term.onData(function (data) {
      var tab = tabs[i];
      if (!tab.ws || tab.ws.readyState !== WebSocket.OPEN) return;
      var encoded = new TextEncoder().encode(data);
      var msg = new Uint8Array(1 + encoded.length);
      msg[0] = MSG_DATA;
      msg.set(encoded, 1);
      tab.ws.send(msg);
    });

    return { term: term, fitAddon: fitAddon, container: container, ws: null };
  });

  var activeTab = 0;
  var statusEl = document.getElementById('status');
  var tabBtns = Array.from(document.querySelectorAll('.tab-btn'));

  function sendResize(i) {
    var tab = tabs[i];
    if (!tab.ws || tab.ws.readyState !== WebSocket.OPEN) return;
    var size = { cols: tab.term.cols, rows: tab.term.rows };
    var encoded = new TextEncoder().encode(JSON.stringify(size));
    var msg = new Uint8Array(1 + encoded.length);
    msg[0] = MSG_RESIZE;
    msg.set(encoded, 1);
    tab.ws.send(msg);
  }

  function connectTab(i) {
    var tab = tabs[i];
    if (tab.ws) {
      tab.ws.onclose = null;
      tab.ws.onerror = null;
      tab.ws.close();
      tab.ws = null;
    }

    var wsProto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    tab.ws = new WebSocket(wsProto + '//' + location.host + '/terminal?tab=' + i);
    tab.ws.binaryType = 'arraybuffer';

    tab.ws.onopen = function () {
      if (statusEl) { statusEl.remove(); statusEl = null; }
      tab.fitAddon.fit();
      sendResize(i);
    };

    tab.ws.onmessage = function (evt) {
      if (evt.data instanceof ArrayBuffer) {
        var arr = new Uint8Array(evt.data);
        if (arr[0] === MSG_DATA) tab.term.write(arr.slice(1));
      } else {
        tab.term.write(evt.data);
      }
    };

    tab.ws.onclose = function () {
      tab.ws = null;
      tab.term.write('\r\n\x1b[31mConnection closed.\x1b[0m \x1b[33mPress any key to reconnect...\x1b[0m\r\n');
      var disposable = tab.term.onData(function () {
        disposable.dispose();
        tab.term.write('\x1b[33mReconnecting...\x1b[0m\r\n');
        connectTab(i);
      });
    };

    tab.ws.onerror = function () {
      tab.term.write('\r\n\x1b[31mWebSocket error.\x1b[0m\r\n');
    };
  }

  function switchTab(i) {
    if (i === activeTab) return;

    tabs[activeTab].container.classList.remove('active');
    tabBtns[activeTab].classList.remove('active');

    activeTab = i;
    tabs[i].container.classList.add('active');
    tabBtns[i].classList.add('active');
    tabs[i].fitAddon.fit();
    sendResize(i);

    // Lazy connect on first visit.
    if (!tabs[i].ws) connectTab(i);
  }

  tabBtns.forEach(function (btn) {
    btn.addEventListener('click', function () {
      switchTab(parseInt(btn.dataset.tab, 10));
    });
  });

  window.addEventListener('resize', function () {
    tabs[activeTab].fitAddon.fit();
    sendResize(activeTab);
  });

  // Usage polling.
  var usageEl = document.getElementById('usage');
  function updateUsage() {
    fetch('/api/usage').then(function (r) {
      return r.ok ? r.json() : null;
    }).then(function (d) {
      if (d && usageEl) {
        usageEl.textContent = 'cpu ' + d.cpu_percent.toFixed(1) + '%  mem ' + d.memory_percent.toFixed(1) + '%';
      }
    }).catch(function () {});
  }
  updateUsage();
  setInterval(updateUsage, 5000);

  // Restart button — purges sessions + deletes pod, then auto-reconnects.
  var restartBtn = document.getElementById('restart-btn');
  restartBtn.addEventListener('click', function () {
    restartBtn.disabled = true;
    restartBtn.textContent = 'restarting...';

    // Snapshot which tabs were connected before we tear them down.
    var wasConnected = tabs.map(function (t) { return t.ws !== null; });

    fetch('/api/restart', { method: 'POST' }).finally(function () {
      restartBtn.disabled = false;
      restartBtn.textContent = 'restart';
      for (var i = 0; i < NUM_TABS; i++) {
        if (wasConnected[i]) {
          tabs[i].term.write('\r\n\x1b[33mRestarting...\x1b[0m\r\n');
          connectTab(i);
        }
      }
    });
  });

  // Connect tab 0 on load; others connect lazily on first switch.
  connectTab(0);
})();
