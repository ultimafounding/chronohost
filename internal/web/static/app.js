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
    el.style.boxShadow = '0 8px 24px rgba(0,0,0,.4)';
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
