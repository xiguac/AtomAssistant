// 文件的在线查看与修改：Word、Excel、PPT 和 PDF 都在浏览器里渲染，服务器只给文件、收文件。
// 第三方库放在 /static/vendor/，后台的安全策略不许加载外面的脚本。
(function () {
  'use strict';
  var root = document.getElementById('fv');
  if (!root) return;
  var PATH = root.dataset.path, KIND = root.dataset.kind, CSRF = root.dataset.csrf;
  var V = '/static/vendor/';
  var $ = function (id) { return document.getElementById(id); };
  var msgEl = $('fv-msg'), stage = $('fv-stage'), side = $('fv-side'), grid = $('fv-grid');
  var btnEdit = $('fv-edit'), btnSave = $('fv-save'), btnDiscard = $('fv-discard');
  var dirty = false;

  function say(text, cls) {
    msgEl.textContent = text;
    msgEl.className = 'fv-msg grow' + (cls ? ' ' + cls : '');
  }
  function setDirty(on, label) {
    dirty = on;
    if (btnSave) { btnSave.hidden = !on; btnSave.disabled = false; }
    if (btnDiscard) btnDiscard.hidden = !on;
    if (on) say(label || '有未保存的修改');
  }
  window.addEventListener('beforeunload', function (e) { if (dirty) { e.preventDefault(); e.returnValue = ''; } });

  var loaded = {};
  function loadScript(src) {
    if (!loaded[src]) {
      loaded[src] = new Promise(function (ok, fail) {
        var s = document.createElement('script');
        s.src = src;
        s.onload = ok;
        s.onerror = function () { fail(new Error('加载 ' + src + ' 失败')); };
        document.head.appendChild(s);
      });
    }
    return loaded[src];
  }
  function fetchFile() {
    return fetch('/files/raw?path=' + encodeURIComponent(PATH), { credentials: 'same-origin' }).then(function (r) {
      if (!r.ok) return r.text().then(function (t) { throw new Error(t || '读取文件失败'); });
      var sha = r.headers.get('X-File-SHA256');
      return r.arrayBuffer().then(function (buf) { return { buf: buf, sha: sha }; });
    });
  }
  // 保存失败时给出原因；文件在打开之后被助手改过（冲突）时多给一个刷新按钮。
  function showSaveError(body, status) {
    say((body && body.message) || ('保存失败（' + status + '）'), 'bad');
    if (body && body.conflict) {
      var b = document.createElement('button');
      b.type = 'button'; b.className = 'btn sm'; b.textContent = '刷新看最新内容'; b.style.marginLeft = '8px';
      b.onclick = function () { dirty = false; location.reload(); };
      msgEl.appendChild(b);
    }
  }

  // ---------- Word / PPT：文字修改 ----------
  var NS = {
    docx: 'http://schemas.openxmlformats.org/wordprocessingml/2006/main',
    pptx: 'http://schemas.openxmlformats.org/drawingml/2006/main'
  };
  var XMLNS = 'http://www.w3.org/XML/1998/namespace';

  // 一个段落自己的文字节点（不含嵌套在它里面的别的段落，如文本框）。
  function ownTexts(p, ns) {
    var out = [], all = p.getElementsByTagNameNS(ns, 't');
    for (var i = 0; i < all.length; i++) {
      var n = all[i].parentNode;
      while (n && !(n.namespaceURI === ns && n.localName === 'p')) n = n.parentNode;
      if (n === p) out.push(all[i]);
    }
    return out;
  }
  function inFallback(el) {
    for (var n = el.parentNode; n; n = n.parentNode) if (n.localName === 'Fallback') return true;
    return false;
  }

  // 读出 zip 里所有有文字的段落。
  function readUnits(zip) {
    var ns = NS[KIND], names;
    if (KIND === 'docx') names = ['word/document.xml'];
    else {
      names = Object.keys(zip.files).filter(function (n) { return /^ppt\/slides\/slide\d+\.xml$/.test(n); })
        .sort(function (a, b) { return parseInt(a.match(/\d+/)[0], 10) - parseInt(b.match(/\d+/)[0], 10); });
    }
    var parts = [];
    return Promise.all(names.map(function (name) {
      return zip.file(name).async('string').then(function (xml) {
        var doc = new DOMParser().parseFromString(xml, 'application/xml');
        if (doc.getElementsByTagName('parsererror').length) throw new Error(name + ' 解析失败');
        var units = [], ps = doc.getElementsByTagNameNS(ns, 'p');
        for (var i = 0; i < ps.length; i++) {
          if (inFallback(ps[i])) continue;
          var ts = ownTexts(ps[i], ns);
          var text = ts.map(function (t) { return t.textContent; }).join('');
          if (text.trim() === '') continue;
          units.push({ p: ps[i], ts: ts, orig: text, text: text });
        }
        return { name: name, doc: doc, units: units };
      });
    })).then(function (list) { parts = list; return parts; });
  }

  function buildEditor(zip, parts, rerender) {
    var total = parts.reduce(function (n, p) { return n + p.units.length; }, 0);
    side.hidden = false;
    grid.classList.add('split');
    side.textContent = '';
    if (!total) {
      var none = document.createElement('div');
      none.className = 'muted';
      none.textContent = '这份文件里没有可以修改的文字。';
      side.appendChild(none);
      return;
    }
    var note = document.createElement('div');
    note.className = 'help';
    note.style.marginBottom = '8px';
    note.textContent = '只改文字，格式、图片、表格等都保持原样。一个段落里原有的多种格式会并成第一段的格式。';
    side.appendChild(note);
    parts.forEach(function (part, pi) {
      if (!part.units.length) return;
      var h = document.createElement('h3');
      h.textContent = KIND === 'docx' ? '正文' : '幻灯片 ' + (pi + 1);
      side.appendChild(h);
      part.units.forEach(function (u) {
        var ta = document.createElement('textarea');
        ta.value = u.text;
        ta.rows = Math.min(6, Math.max(1, Math.ceil(u.text.length / 28)));
        ta.addEventListener('input', function () {
          u.text = ta.value.replace(/[\r\n]+/g, ' ');
          ta.classList.toggle('changed', u.text !== u.orig);
          var n = 0;
          parts.forEach(function (p) { p.units.forEach(function (x) { if (x.text !== x.orig) n++; }); });
          if (n) setDirty(true, '已修改 ' + n + ' 处，还没保存'); else setDirty(false), say('');
        });
        side.appendChild(ta);
      });
    });
    editor = { zip: zip, parts: parts, rerender: rerender };
  }
  var editor = null;

  function changedCount() {
    return editor ? editor.parts.reduce(function (n, p) { return n + p.units.filter(function (u) { return u.text !== u.orig; }).length; }, 0) : 0;
  }

  function saveOffice() {
    if (!editor || !changedCount()) return;
    var e = editor;
    btnSave.disabled = true;
    say('正在保存…');
    e.parts.forEach(function (part) {
      part.units.forEach(function (u) {
        if (u.text === u.orig) return;
        u.ts[0].textContent = u.text;
        u.ts[0].setAttributeNS(XMLNS, 'xml:space', 'preserve');
        for (var i = 1; i < u.ts.length; i++) u.ts[i].textContent = '';
      });
      var xml = new XMLSerializer().serializeToString(part.doc);
      // 浏览器序列化时可能已带上声明；声明只能出现在最开头，重复加会让文件损坏。
      if (!/^<\?xml\s/.test(xml)) xml = '<?xml version="1.0" encoding="UTF-8" standalone="yes"?>\n' + xml;
      e.zip.file(part.name, xml);
    });
    e.zip.generateAsync({ type: 'uint8array', compression: 'DEFLATE' }).then(function (bytes) {
      var q = '?path=' + encodeURIComponent(PATH) + '&sha=' + encodeURIComponent(sha) + '&csrf=' + encodeURIComponent(CSRF);
      return fetch('/files/save' + q, { method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/octet-stream' }, body: bytes })
        .then(function (r) { return r.json().catch(function () { return {}; }).then(function (b) { return { r: r, b: b, bytes: bytes }; }); });
    }).then(function (x) {
      if (!x.r.ok || !x.b.ok) { showSaveError(x.b, x.r.status); btnSave.disabled = false; return; }
      sha = x.b.sha256;
      setDirty(false);
      return e.rerender(x.bytes.buffer).then(function () { say('已保存。原来的版本留在了历史版本里。', 'ok'); });
    }).catch(function (err) { say('保存失败：' + err.message, 'bad'); btnSave.disabled = false; });
  }

  var sha = '';

  function showDocx() {
    return loadScript(V + 'jszip-3.10.2.min.js').then(function () { return loadScript(V + 'docx-preview-0.4.1.min.js'); }).then(function () {
      var render = function (buf) {
        stage.textContent = '';
        var box = document.createElement('div');
        box.className = 'fv-docx';
        stage.appendChild(box);
        return window.docx.renderAsync(buf, box, box, { inWrapper: true, breakPages: true, ignoreLastRenderedPageBreak: false });
      };
      return fetchFile().then(function (f) {
        sha = f.sha;
        return render(f.buf).then(function () { return enableOfficeEdit(f.buf, render); });
      });
    });
  }

  function showPptx() {
    return loadScript(V + 'pptx-preview-1.0.7.umd.js').then(function () {
      var render = function (buf) {
        stage.textContent = '';
        var box = document.createElement('div');
        box.className = 'fv-pptx';
        stage.appendChild(box);
        var w = Math.max(320, Math.min(960, stage.clientWidth - 32));
        var viewer = window.pptxPreview.init(box, { width: w, height: Math.round(w * 9 / 16), mode: 'list' });
        return viewer.preview(buf);
      };
      return fetchFile().then(function (f) {
        sha = f.sha;
        return render(f.buf).then(function () {
          return loadScript(V + 'jszip-3.10.2.min.js').then(function () { return enableOfficeEdit(f.buf, render); });
        });
      });
    });
  }

  // 解开文件，列出文字，按钮点了才打开修改面板。
  function enableOfficeEdit(buf, render) {
    return loadScript(V + 'jszip-3.10.2.min.js').then(function () { return window.JSZip.loadAsync(buf); }).then(function (zip) {
      return readUnits(zip).then(function (parts) {
        say('已打开。想改文字，点「编辑文字」。');
        btnEdit.hidden = false;
        btnEdit.onclick = function () {
          if (!side.hidden) { return; }
          buildEditor(zip, parts, function (newBuf) {
            // 保存之后用新文件重新画一遍，再重新读出文字。
            side.hidden = true; grid.classList.remove('split'); editor = null;
            return render(newBuf).then(function () { return enableOfficeEdit(newBuf, render); });
          });
          btnEdit.hidden = true;
        };
        btnDiscard.onclick = function () { dirty = false; location.reload(); };
        btnSave.onclick = saveOffice;
      });
    });
  }

  // ---------- PDF ----------
  function showPdf() {
    return fetchFile().then(function (f) {
      return import(V + 'pdfjs-6.4.299.min.mjs').then(function (pdfjs) {
        pdfjs.GlobalWorkerOptions.workerSrc = V + 'pdfjs-worker-6.4.299.min.mjs';
        return pdfjs.getDocument({ data: new Uint8Array(f.buf) }).promise;
      });
    }).then(function (pdf) {
      stage.textContent = '';
      var box = document.createElement('div');
      box.className = 'fv-pdf';
      stage.appendChild(box);
      say('共 ' + pdf.numPages + ' 页，PDF 只能查看。');
      var width = Math.max(320, Math.min(1000, stage.clientWidth - 32));
      var dpr = window.devicePixelRatio || 1;
      var chain = Promise.resolve();
      for (var i = 1; i <= pdf.numPages; i++) {
        (function (n) {
          chain = chain.then(function () { return pdf.getPage(n); }).then(function (page) {
            var base = page.getViewport({ scale: 1 });
            var vp = page.getViewport({ scale: (width / base.width) * dpr });
            var c = document.createElement('canvas');
            c.width = Math.floor(vp.width); c.height = Math.floor(vp.height);
            c.style.width = Math.floor(vp.width / dpr) + 'px';
            box.appendChild(c);
            return page.render({ canvasContext: c.getContext('2d'), viewport: vp }).promise;
          });
        })(i);
      }
      return chain;
    });
  }

  // ---------- Excel ----------
  var sheetData = null, curSheet = '', edits = {}, selected = null;
  var tabsEl = $('fv-tabs'), bar = $('fv-formula'), cellName = $('fv-cell'), cellVal = $('fv-cellval');

  function colName(i) { var s = ''; for (i++; i > 0; i = Math.floor((i - 1) / 26)) s = String.fromCharCode(65 + (i - 1) % 26) + s; return s; }
  function addr(r, c) { return colName(c) + (r + 1); }
  function parseAddr(a) {
    var m = /^([A-Z]+)(\d+)$/.exec(a), c = 0;
    for (var i = 0; i < m[1].length; i++) c = c * 26 + m[1].charCodeAt(i) - 64;
    return [parseInt(m[2], 10) - 1, c - 1];
  }
  function cellCss(st) {
    var css = '';
    if (st.bold) css += 'font-weight:700;';
    if (st.italic) css += 'font-style:italic;';
    if (st.underline || st.strike) css += 'text-decoration:' + (st.underline ? 'underline ' : '') + (st.strike ? 'line-through' : '') + ';';
    if (st.color) css += 'color:' + st.color + ';';
    if (st.size) css += 'font-size:' + Math.round(st.size * 4 / 3) + 'px;';
    if (st.font) css += 'font-family:"' + st.font.replace(/["\\;]/g, '') + '",sans-serif;';
    if (st.fill) css += 'background:' + st.fill + ';';
    if (st.align) css += 'text-align:' + (st.align === 'centerContinuous' ? 'center' : st.align) + ';';
    if (st.valign) css += 'vertical-align:' + st.valign + ';';
    var b = st.borders || {};
    ['left', 'right', 'top', 'bottom'].forEach(function (k) { if (b[k]) css += 'border-' + k + ':' + b[k] + ';'; });
    return css;
  }
  function editKey(sheet, a) { return sheet + '!' + a; }
  function editCount() { return Object.keys(edits).length; }

  function renderSheet() {
    var d = sheetData;
    stage.textContent = '';
    var merged = {}, spans = {};
    (d.merges || []).forEach(function (m) {
      var p = m.split(':'), a = parseAddr(p[0]), b = parseAddr(p[1]);
      var r2 = Math.min(b[0], d.rows - 1), c2 = Math.min(b[1], d.cols - 1);
      if (a[0] >= d.rows || a[1] >= d.cols) return;
      spans[a[0] + ',' + a[1]] = [r2 - a[0] + 1, c2 - a[1] + 1];
      for (var r = a[0]; r <= r2; r++) for (var c = a[1]; c <= c2; c++) if (r !== a[0] || c !== a[1]) merged[r + ',' + c] = true;
    });
    var table = document.createElement('table');
    table.className = 'fv-grid';
    var total = 42;
    var cg = document.createElement('colgroup');
    var rn = document.createElement('col'); rn.style.width = '42px'; cg.appendChild(rn);
    d.col_widths.forEach(function (w) {
      var col = document.createElement('col'); var px = Math.round(w * 7 + 5); col.style.width = px + 'px'; total += px; cg.appendChild(col);
    });
    table.style.width = total + 'px';
    table.appendChild(cg);
    var thead = document.createElement('thead'), hr = document.createElement('tr');
    var corner = document.createElement('th'); corner.className = 'corner'; hr.appendChild(corner);
    for (var c = 0; c < d.cols; c++) { var th = document.createElement('th'); th.textContent = colName(c); hr.appendChild(th); }
    thead.appendChild(hr); table.appendChild(thead);
    var tb = document.createElement('tbody');
    for (var r = 0; r < d.rows; r++) {
      var tr = document.createElement('tr');
      if (d.row_heights && d.row_heights[r] > 0) tr.style.height = Math.round(d.row_heights[r] * 4 / 3) + 'px';
      var h = document.createElement('th'); h.className = 'rn'; h.textContent = r + 1; tr.appendChild(h);
      for (c = 0; c < d.cols; c++) {
        if (merged[r + ',' + c]) continue;
        var cell = d.cells[r][c], td = document.createElement('td'), a = addr(r, c);
        td.dataset.a = a;
        var sp = spans[r + ',' + c];
        if (sp) { if (sp[0] > 1) td.rowSpan = sp[0]; if (sp[1] > 1) td.colSpan = sp[1]; }
        var st = cell.s ? d.styles[cell.s] : null, shown = cell.v || '';
        if (st) { td.style.cssText = cellCss(st); if (st.wrap) td.className = 'wrap'; }
        var ed = edits[editKey(curSheet, a)];
        if (ed) { td.classList.add('dirty'); shown = ed.shown; }
        else if (!(st && st.align) && /^-?[\d,]*\.?\d+%?$/.test(shown.trim()) && shown.trim() !== '') td.classList.add('num');
        td.textContent = shown;
        tr.appendChild(td);
      }
      tb.appendChild(tr);
    }
    table.appendChild(tb);
    stage.appendChild(table);
    if (d.truncated) {
      var tip = document.createElement('div');
      tip.className = 'help'; tip.style.marginTop = '8px';
      tip.textContent = '表格很大，这里只显示前 ' + d.rows + ' 行、' + d.cols + ' 列。';
      stage.appendChild(tip);
    }
    selected = null;
    cellName.textContent = '—'; cellVal.value = ''; cellVal.disabled = true;
  }

  function select(td) {
    var prev = stage.querySelector('td.sel');
    if (prev) prev.classList.remove('sel');
    td.classList.add('sel');
    selected = td.dataset.a;
    var p = parseAddr(selected), cell = sheetData.cells[p[0]][p[1]];
    var ed = edits[editKey(curSheet, selected)];
    cellName.textContent = selected;
    cellVal.disabled = false;
    cellVal.value = ed ? ed.raw : (cell.f || cell.v || '');
  }

  function commit() {
    if (!selected) return;
    var p = parseAddr(selected), cell = sheetData.cells[p[0]][p[1]];
    var raw = cellVal.value, orig = cell.f || cell.v || '';
    var key = editKey(curSheet, selected), td = stage.querySelector('td[data-a="' + selected + '"]');
    if (raw === orig) { delete edits[key]; if (td) { td.classList.remove('dirty'); td.textContent = cell.v || ''; } }
    else {
      var val = raw.trim() === '' ? null : (/^-?\d+(\.\d+)?$/.test(raw.trim()) ? Number(raw.trim()) : raw);
      edits[key] = { sheet: curSheet, cell: selected, value: val, raw: raw, shown: raw };
      if (td) { td.classList.add('dirty'); td.textContent = raw; }
    }
    var n = editCount();
    if (n) setDirty(true, '已修改 ' + n + ' 个单元格，还没保存'); else { setDirty(false); say(''); }
  }

  function loadSheet(name) {
    var q = '/files/sheet?path=' + encodeURIComponent(PATH) + (name ? '&sheet=' + encodeURIComponent(name) : '');
    return fetch(q, { credentials: 'same-origin' }).then(function (r) {
      return r.json().then(function (b) { if (!r.ok) throw new Error(b.message || '读取表格失败'); return b; });
    }).then(function (d) {
      sheetData = d; curSheet = d.sheet; sha = d.sha256;
      tabsEl.hidden = d.sheets.length < 2;
      tabsEl.textContent = '';
      d.sheets.forEach(function (s) {
        var b = document.createElement('button'); b.type = 'button'; b.textContent = s; b.className = s === curSheet ? 'on' : '';
        b.onclick = function () { if (s !== curSheet) loadSheet(s); };
        tabsEl.appendChild(b);
      });
      renderSheet();
    });
  }

  function saveSheet() {
    var list = Object.keys(edits).map(function (k) { var e = edits[k]; return { sheet: e.sheet, cell: e.cell, value: e.value }; });
    if (!list.length) return;
    btnSave.disabled = true; say('正在保存…');
    fetch('/files/sheet/save?csrf=' + encodeURIComponent(CSRF), {
      method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ path: PATH, sha: sha, edits: list })
    }).then(function (r) { return r.json().catch(function () { return {}; }).then(function (b) { return { r: r, b: b }; }); })
      .then(function (x) {
        if (!x.r.ok || !x.b.ok) { showSaveError(x.b, x.r.status); btnSave.disabled = false; return; }
        edits = {};
        setDirty(false);
        var issues = x.b.formula_issues || [];
        return loadSheet(curSheet).then(function () {
          if (issues.length) say('已保存。有 ' + issues.length + ' 个公式有问题，如 ' + issues[0].sheet + '!' + issues[0].cell + '：' + issues[0].problem, 'bad');
          else say('已保存。原来的版本留在了历史版本里。', 'ok');
        });
      }).catch(function (err) { say('保存失败：' + err.message, 'bad'); btnSave.disabled = false; });
  }

  function showXlsx() {
    bar.hidden = false;
    stage.addEventListener('click', function (e) { var td = e.target.closest('td'); if (td && td.dataset.a) select(td); });
    stage.addEventListener('dblclick', function (e) { if (e.target.closest('td')) { cellVal.focus(); cellVal.select(); } });
    cellVal.addEventListener('keydown', function (e) {
      if (e.key === 'Enter') { e.preventDefault(); commit(); }
      else if (e.key === 'Escape') { var td = stage.querySelector('td.sel'); if (td) select(td); }
    });
    cellVal.addEventListener('blur', commit);
    btnSave.onclick = saveSheet;
    btnDiscard.onclick = function () { dirty = false; location.reload(); };
    return loadSheet('').then(function () { say('点单元格查看，在上方输入框里修改，回车确认，改完点「保存」。'); });
  }

  // ---------- 入口 ----------
  say('正在打开…');
  var run = { docx: showDocx, pptx: showPptx, xlsx: showXlsx, pdf: showPdf }[KIND];
  run().catch(function (err) {
    stage.textContent = '';
    say('打开失败：' + err.message, 'bad');
  });
})();
