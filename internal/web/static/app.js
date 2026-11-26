// ChronoHost portal helpers. HTMX handles fragment swaps, Alpine handles local UI state.
window.CH = {
  async api(method, url, body) {
    const opts = { method, headers: { 'Accept': 'application/json' } };
    if (body !== undefined) {
      opts.headers['Content-Type'] = 'application/json';
      opts.body = JSON.stringify(body);
    }
    const res = await fetch(url, opts);
    let data = null;
    try { data = await res.json(); } catch (_) { /* empty response */ }
    if (!res.ok) throw new Error((data && data.error) || ('Request failed: ' + res.status));
    return data;
  },

  toast(message, kind = 'info') {
    let host = document.getElementById('toasts');
    if (!host) {
      host = document.createElement('div');
      host.id = 'toasts';
      host.style.cssText = 'position:fixed;right:1.2rem;bottom:1.2rem;z-index:100;display:flex;flex-direction:column;gap:.5rem;max-width:360px';
      document.body.appendChild(host);
    }
    const el = document.createElement('div');
    el.className = 'alert ' + kind;
    el.textContent = message;
    host.appendChild(el);
    setTimeout(() => el.remove(), 5000);
  },

  confirm(message, fn) {
    if (window.confirm(message)) fn();
  },

  reload(delay = 600) { setTimeout(() => location.reload(), delay); },

  escapeHtml(s) {
    return String(s).replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
  }
};

// Shared hover/focus tooltip. A fixed bubble avoids clipping inside scrollable tables.
(function () {
  let bubble = null;
  let current = null;

  function ensureBubble() {
    if (bubble) return bubble;
    bubble = document.createElement('div');
    bubble.className = 'tooltip-box';
    bubble.setAttribute('role', 'tooltip');
    bubble.style.display = 'none';
    document.body.appendChild(bubble);
    return bubble;
  }

  function place(el) {
    if (!current || !bubble) return;
    const r = el.getBoundingClientRect();
    if (!r.width && !r.height) { hide(); return; }
    const gap = 8;
    const x = Math.max(gap, Math.min(r.left + r.width / 2 - bubble.offsetWidth / 2, window.innerWidth - bubble.offsetWidth - gap));
    let y = r.top - bubble.offsetHeight - gap;
    if (y < gap) y = r.bottom + gap;
    bubble.style.left = x + 'px';
    bubble.style.top = y + 'px';
  }

  function show(el) {
    const text = el.getAttribute('data-tip');
    if (!text) return;
    current = el;
    const tip = ensureBubble();
    tip.textContent = text;
    tip.style.display = 'block';
    place(el);
  }

  function hide() {
    current = null;
    if (bubble) bubble.style.display = 'none';
  }

  document.addEventListener('pointerover', (e) => {
    const el = e.target.closest && e.target.closest('[data-tip]');
    if (el && el !== current) show(el);
  });
  document.addEventListener('pointermove', () => { if (current) place(current); });
  document.addEventListener('pointerout', (e) => {
    if (current && !(e.relatedTarget && current.contains(e.relatedTarget))) hide();
  });
  document.addEventListener('focusin', (e) => {
    const el = e.target.closest && e.target.closest('[data-tip]');
    if (el && el !== current) show(el);
  });
  document.addEventListener('focusout', (e) => {
    if (current && !(e.relatedTarget && current.contains(e.relatedTarget))) hide();
  });
  window.addEventListener('scroll', () => { if (current) place(current); }, true);
})();

document.addEventListener('htmx:responseError', (e) => {
  const msg = e.detail.xhr && e.detail.xhr.responseText ? e.detail.xhr.responseText.slice(0, 200) : 'Request failed';
  CH.toast(msg, 'error');
});

// Wire simple data-action buttons declared in templates.
document.addEventListener('click', (ev) => {
  const el = ev.target.closest('[data-action]');
  if (!el) return;
  const action = el.dataset.action;
  const url = el.dataset.url;
  const confirmText = el.dataset.confirm;
  const run = async () => {
    el.disabled = true;
    try {
      const body = el.dataset.body ? JSON.parse(el.dataset.body) : undefined;
      await CH.api(el.dataset.method || 'POST', url, body);
      CH.toast(el.dataset.success || 'Done', 'success');
      if (el.dataset.reload === 'true') CH.reload();
    } catch (err) {
      CH.toast(err.message, 'error');
      el.disabled = false;
    }
  };
  if (confirmText) CH.confirm(confirmText, run); else run();
});
