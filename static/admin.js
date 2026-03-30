(function () {
  var data = [];
  var sortCol = 'username';
  var sortAsc = true;

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

  function fmtPct(pct) {
    return pct > 0 ? pct.toFixed(1) + '%' : '—';
  }

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

    var rows = data.map(function (u) {
      var dot = '<span class="dot ' + (u.connected ? 'dot-on' : 'dot-off') + '"></span>' +
                (u.connected ? 'active' : 'idle');
      return '<tr>' +
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

    // Update summary stats
    var running = data.filter(function (u) { return u.pod_phase === 'Running'; });
    var connected = data.filter(function (u) { return u.connected; });
    var cpuSum = running.reduce(function (s, u) { return s + u.cpu_percent; }, 0);
    var memSum = running.reduce(function (s, u) { return s + u.memory_percent; }, 0);

    document.getElementById('stat-total').textContent = data.length;
    document.getElementById('stat-running').textContent = running.length;
    document.getElementById('stat-connected').textContent = connected.length;
    document.getElementById('stat-cpu').textContent = running.length ? (cpuSum / running.length).toFixed(1) + '%' : '—';
    document.getElementById('stat-mem').textContent = running.length ? (memSum / running.length).toFixed(1) + '%' : '—';

    // Update sort indicators
    document.querySelectorAll('thead th').forEach(function (th) {
      th.classList.remove('sort-asc', 'sort-desc');
      if (th.dataset.col === sortCol) {
        th.classList.add(sortAsc ? 'sort-asc' : 'sort-desc');
      }
    });
  }

  function esc(s) {
    return String(s)
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;');
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
      if (sortCol === col) {
        sortAsc = !sortAsc;
      } else {
        sortCol = col;
        sortAsc = true;
      }
      render();
    });
  });

  refresh();
  setInterval(refresh, 5000);
})();
