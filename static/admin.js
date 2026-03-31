(function () {
  var MSG_DATA   = 0x00;
  var MSG_RESIZE = 0x01;

  var data = [];
  var sortCol = 'username';
  var sortAsc = true;
  var selectedUser = null; // { user_sub, username, pod_name }

  // ── Inspector state ─────────────────────────────────────────────────

  var activePane = 'watch'; // 'watch' | 'root'

  // Watch: one xterm per user tab (0-5), lazily created
  var watchTerms = [];     // xterm instances
  var watchFits  = [];     // FitAddon instances
  var watchWS    = [];     // WebSocket connections
  var watchTabIdx = 0;

  // Root exec: single xterm
  var rootTerm, rootFit, rootWS;

  // ── xterm helpers ──────────────────────────────────────────────────

  var termOptions = {
    cursorBlink: false,
    fontSize: 13,
    fontFamily: '"JetBrains Mono", "Fira Code", monospace',
    theme: { background: '#0a0a0a', foreground: '#c8c8c8' },
  };

  function makeTerminal(container) {
    var term = new Terminal(termOptions);
    var fit  = new FitAddon.FitAddon();
    term.loadAddon(fit);
    term.open(container);
    fit.fit();
    return { term: term, fit: fit };
  }

  // ── Watch pane ──────────────────────────────────────────────────────

  function buildWatchTabBar() {
    var bar = document.getElementById('watch-tab-bar');
    bar.innerHTML = '';
    for (var t = 0; t < 6; t++) {
      (function (idx) {
        var btn = document.createElement('button');
        btn.className = 'watch-tab-btn' + (idx === watchTabIdx ? ' active' : '');
        btn.textContent = idx + 1;
        btn.addEventListener('click', function () { switchWatchTab(idx); });
        bar.appendChild(btn);
      })(t);
    }
  }

  function switchWatchTab(idx) {
    watchTabIdx = idx;
    document.querySelectorAll('.watch-tab-btn').forEach(function (b, i) {
      b.classList.toggle('active', i === idx);
    });
    if (selectedUser) connectWatch(selectedUser, idx);
  }

  function disconnectWatch() {
    for (var i = 0; i < 6; i++) {
      if (watchWS[i]) {
        watchWS[i].onclose = null;
        watchWS[i].close();
        watchWS[i] = null;
      }
    }
  }

  function connectWatch(user, tabIdx) {
    // Close previous watch on this tab slot
    if (watchWS[tabIdx]) {
      watchWS[tabIdx].onclose = null;
      watchWS[tabIdx].close();
      watchWS[tabIdx] = null;
    }

    // Lazily create the xterm for this tab
    var termContainer = document.getElementById('watch-term');
    if (!watchTerms[tabIdx]) {
      // Hide previous terminal divs, show this one
      var div = document.createElement('div');
      div.style.cssText = 'position:absolute;inset:0';
      // We'll manage visibility via watchTabIdx comparison
      termContainer._divs = termContainer._divs || {};
      termContainer._divs[tabIdx] = div;
      termContainer.style.position = 'relative';
      termContainer.appendChild(div);
      var t = makeTerminal(div);
      watchTerms[tabIdx] = t.term;
      watchFits[tabIdx]  = t.fit;
    }

    // Show only the active tab's div
    if (termContainer._divs) {
      Object.keys(termContainer._divs).forEach(function (k) {
        termContainer._divs[k].style.display = (parseInt(k) === tabIdx) ? '' : 'none';
      });
    }

    setWatchStatus('connecting to tab ' + (tabIdx + 1) + '...');

    var wsProto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    var ws = new WebSocket(wsProto + '//' + location.host + '/terminal/watch?sub=' + encodeURIComponent(user.user_sub) + '&tab=' + tabIdx);
    ws.binaryType = 'arraybuffer';
    watchWS[tabIdx] = ws;

    ws.onopen = function () {
      setWatchStatus('watching ' + user.username + ' tab ' + (tabIdx + 1));
      if (watchFits[tabIdx]) watchFits[tabIdx].fit();
    };

    ws.onmessage = function (evt) {
      if (!(evt.data instanceof ArrayBuffer)) return;
      var arr = new Uint8Array(evt.data);
      if (arr[0] === MSG_DATA && watchTerms[tabIdx]) {
        watchTerms[tabIdx].write(arr.slice(1));
      }
    };

    ws.onclose = function () {
      watchWS[tabIdx] = null;
      setWatchStatus('disconnected (tab ' + (tabIdx + 1) + ')');
    };

    ws.onerror = function () {
      setWatchStatus('\x1b[31mwatch error\x1b[0m');
    };
  }

  function setWatchStatus(msg) {
    var el = document.getElementById('watch-status');
    if (el) el.textContent = msg;
  }

  // ── Root exec pane ──────────────────────────────────────────────────

  function connectRoot(user) {
    if (rootWS) {
      rootWS.onclose = null;
      rootWS.close();
      rootWS = null;
    }

    var termContainer = document.getElementById('root-term');
    if (!rootTerm) {
      var t = makeTerminal(termContainer);
      rootTerm = t.term;
      rootFit  = t.fit;

      rootTerm.onData(function (d) {
        if (!rootWS || rootWS.readyState !== WebSocket.OPEN) return;
        var enc = new TextEncoder().encode(d);
        var msg = new Uint8Array(1 + enc.length);
        msg[0] = MSG_DATA;
        msg.set(enc, 1);
        rootWS.send(msg);
      });
    } else {
      rootFit.fit();
    }

    setRootStatus('connecting root session in ' + user.pod_name + '...');

    var wsProto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    rootWS = new WebSocket(wsProto + '//' + location.host + '/terminal/root?pod=' + encodeURIComponent(user.pod_name));
    rootWS.binaryType = 'arraybuffer';

    rootWS.onopen = function () {
      setRootStatus('root@' + user.pod_name);
      rootFit.fit();
      sendRootResize();
    };

    rootWS.onmessage = function (evt) {
      if (!(evt.data instanceof ArrayBuffer)) return;
      var arr = new Uint8Array(evt.data);
      if (arr[0] === MSG_DATA && rootTerm) rootTerm.write(arr.slice(1));
    };

    rootWS.onclose = function () {
      rootWS = null;
      setRootStatus('disconnected — press any key to reconnect');
      if (rootTerm) {
        var disp = rootTerm.onData(function () {
          disp.dispose();
          if (selectedUser) connectRoot(selectedUser);
        });
      }
    };

    rootWS.onerror = function () {
      setRootStatus('connection error');
    };
  }

  function sendRootResize() {
    if (!rootWS || rootWS.readyState !== WebSocket.OPEN || !rootTerm) return;
    var size = { cols: rootTerm.cols, rows: rootTerm.rows };
    var enc  = new TextEncoder().encode(JSON.stringify(size));
    var msg  = new Uint8Array(1 + enc.length);
    msg[0] = MSG_RESIZE;
    msg.set(enc, 1);
    rootWS.send(msg);
  }

  function setRootStatus(msg) {
    var el = document.getElementById('root-status');
    if (el) el.textContent = msg;
  }

  // ── Inspector open/close ────────────────────────────────────────────

  function openInspector(user) {
    selectedUser = user;
    var panel = document.getElementById('inspector');
    panel.classList.add('open');

    var title = document.getElementById('inspector-title');
    title.innerHTML = 'inspecting: <span>' + esc(user.username || user.pod_name) + '</span>';

    buildWatchTabBar();

    if (activePane === 'watch') {
      connectWatch(user, watchTabIdx);
    } else {
      connectRoot(user);
    }

    // Highlight selected row
    document.querySelectorAll('#tbody tr').forEach(function (tr) {
      tr.classList.toggle('selected', tr.dataset.sub === user.user_sub);
    });
  }

  function closeInspector() {
    disconnectWatch();
    if (rootWS) { rootWS.onclose = null; rootWS.close(); rootWS = null; }
    selectedUser = null;
    document.getElementById('inspector').classList.remove('open');
    document.querySelectorAll('#tbody tr').forEach(function (tr) { tr.classList.remove('selected'); });
    setWatchStatus('not connected');
    setRootStatus('not connected');
  }

  document.getElementById('inspector-close').addEventListener('click', closeInspector);

  document.querySelectorAll('.insp-tab').forEach(function (tab) {
    tab.addEventListener('click', function () {
      var pane = tab.dataset.pane;
      activePane = pane;
      document.querySelectorAll('.insp-tab').forEach(function (t) { t.classList.toggle('active', t.dataset.pane === pane); });
      document.querySelectorAll('.insp-pane').forEach(function (p) { p.classList.toggle('active', p.id === 'pane-' + pane); });
      if (!selectedUser) return;
      if (pane === 'watch') {
        connectWatch(selectedUser, watchTabIdx);
        if (watchFits[watchTabIdx]) watchFits[watchTabIdx].fit();
      } else {
        connectRoot(selectedUser);
      }
    });
  });

  window.addEventListener('resize', function () {
    if (activePane === 'watch' && watchFits[watchTabIdx]) watchFits[watchTabIdx].fit();
    if (activePane === 'root' && rootFit) { rootFit.fit(); sendRootResize(); }
  });

  // ── Overview table ──────────────────────────────────────────────────

  function colorForPct(pct) {
    if (pct >= 80) return '#f48771';
    if (pct >= 50) return '#cca700';
    return '#89d185';
  }

  function fmtAge(seconds) {
    if (seconds <= 0) return '—';
    var d = Math.floor(seconds / 86400);
    var h = Math.floor((seconds % 86400) / 3600);
    var m = Math.floor((seconds % 3600) / 60);
    if (d > 0) return d + 'd ' + h + 'h';
    if (h > 0) return h + 'h ' + m + 'm';
    return m + 'm';
  }

  function fmtPct(pct) { return pct > 0 ? pct.toFixed(1) + '%' : '—'; }

  function phaseClass(phase) {
    switch (phase) {
      case 'Running':  return 'phase-running';
      case 'Pending':  return 'phase-pending';
      case 'Failed':   return 'phase-failed';
      default:         return 'phase-unknown';
    }
  }

  function bar(pct) {
    var color = colorForPct(pct);
    var width = Math.min(pct, 100).toFixed(1);
    return '<div class="bar-wrap">' +
      '<div class="bar-bg"><div class="bar-fill" style="width:' + width + '%;background:' + color + '"></div></div>' +
      '<span class="bar-label" style="color:' + color + '">' + fmtPct(pct) + '</span>' +
      '</div>';
  }

  function esc(s) {
    return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
  }

  function sortData() {
    data.sort(function (a, b) {
      var av = a[sortCol], bv = b[sortCol];
      if (typeof av === 'string') av = av.toLowerCase();
      if (typeof bv === 'string') bv = bv.toLowerCase();
      if (av < bv) return sortAsc ? -1 : 1;
      if (av > bv) return sortAsc ?  1 : -1;
      return 0;
    });
  }

  function render() {
    var tbody = document.getElementById('tbody');
    if (data.length === 0) {
      tbody.innerHTML = '<tr><td colspan="7" class="empty">no pods found</td></tr>';
      return;
    }

    sortData();

    var selSub = selectedUser ? selectedUser.user_sub : null;
    var rows = data.map(function (u) {
      var dot = '<span class="dot ' + (u.connected ? 'dot-on' : 'dot-off') + '"></span>' +
                (u.connected ? 'active' : 'idle');
      var cls = u.user_sub === selSub ? ' class="selected"' : '';
      return '<tr' + cls + ' data-sub="' + esc(u.user_sub) + '" data-pod="' + esc(u.pod_name) + '" data-username="' + esc(u.username || u.pod_name) + '">' +
        '<td class="username">' + esc(u.username || u.pod_name) + '</td>' +
        '<td><span class="phase ' + phaseClass(u.pod_phase) + '">' + esc(u.pod_phase) + '</span></td>' +
        '<td>' + dot + '</td>' +
        '<td class="bar-cell">' + bar(u.cpu_percent) + '</td>' +
        '<td class="bar-cell">' + bar(u.memory_percent) + '</td>' +
        '<td class="age">' + fmtAge(u.pod_age_seconds) + '</td>' +
        '<td class="age">' + esc(u.pod_name) + '</td>' +
        '</tr>';
    });
    tbody.innerHTML = rows.join('');

    // Row click → open inspector
    tbody.querySelectorAll('tr[data-sub]').forEach(function (tr) {
      tr.addEventListener('click', function () {
        var user = {
          user_sub:  tr.dataset.sub,
          pod_name:  tr.dataset.pod,
          username:  tr.dataset.username,
        };
        if (selectedUser && selectedUser.user_sub === user.user_sub) {
          closeInspector();
        } else {
          openInspector(user);
        }
      });
    });

    // Summary stats
    var running   = data.filter(function (u) { return u.pod_phase === 'Running'; });
    var connected = data.filter(function (u) { return u.connected; });
    var cpuSum    = running.reduce(function (s, u) { return s + u.cpu_percent; }, 0);
    var memSum    = running.reduce(function (s, u) { return s + u.memory_percent; }, 0);

    document.getElementById('stat-total').textContent     = data.length;
    document.getElementById('stat-running').textContent   = running.length;
    document.getElementById('stat-connected').textContent = connected.length;
    document.getElementById('stat-cpu').textContent       = running.length ? (cpuSum / running.length).toFixed(1) + '%' : '—';
    document.getElementById('stat-mem').textContent       = running.length ? (memSum / running.length).toFixed(1) + '%' : '—';

    // Sort indicators
    document.querySelectorAll('thead th').forEach(function (th) {
      th.classList.remove('sort-asc', 'sort-desc');
      if (th.dataset.col === sortCol) th.classList.add(sortAsc ? 'sort-asc' : 'sort-desc');
    });
  }

  function refresh() {
    fetch('/api/admin/overview').then(function (r) {
      return r.ok ? r.json() : null;
    }).then(function (d) {
      if (!d) return;
      data = d;
      render();
      var el = document.getElementById('last-updated');
      if (el) el.textContent = 'updated ' + new Date().toLocaleTimeString();
    }).catch(function () {});
  }

  // Column sort
  document.querySelectorAll('thead th[data-col]').forEach(function (th) {
    th.addEventListener('click', function () {
      var col = th.dataset.col;
      if (sortCol === col) { sortAsc = !sortAsc; } else { sortCol = col; sortAsc = true; }
      render();
    });
  });

  refresh();
  setInterval(refresh, 5000);
})();
