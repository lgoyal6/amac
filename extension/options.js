// Inline handlers are blocked by the extension CSP, so wiring lives here.
const $ = id => document.getElementById(id);

chrome.storage.local.get(['endpoint', 'token']).then(cfg => {
  $('endpoint').value = cfg.endpoint || 'http://100.127.168.102:7788';
  $('token').value = cfg.token || '';
});

// Validate before storing. The first real install put the literal string
// "~/.amac/token" in this field, because the placeholder named the file and
// that reads as an instruction. A wrong token fails as a 401 that is only
// visible in the service worker console, so every detection would have been
// silently dropped. Catching it here is the difference between a typo and a
// tracker that quietly records nothing.
function tokenProblem(t) {
  if (!t) return 'Token is empty.';
  if (t.startsWith('~') || t.startsWith('/') || t.includes('/')) {
    return 'That looks like a file path. Paste the file’s contents: cat ~/.amac/token | pbcopy';
  }
  if (!/^[a-f0-9]{64}$/i.test(t)) {
    return `Expected 64 hex characters, got ${t.length}.`;
  }
  return null;
}

$('save').addEventListener('click', async () => {
  const endpoint = $('endpoint').value.trim().replace(/\/+$/, '');
  const token = $('token').value.trim();
  const status = $('status');

  const problem = tokenProblem(token);
  if (problem) {
    status.textContent = problem;
    status.style.color = '#c0392b';
    return;
  }

  await chrome.storage.local.set({ endpoint, token });
  $('saved').textContent = 'saved';

  // Prove it works now rather than letting the first real application be the
  // test. A saved-but-broken config is the failure mode this page exists to
  // prevent.
  status.style.color = '#666';
  status.textContent = 'checking connection…';
  try {
    const r = await fetch(`${endpoint}/api/agents`, { headers: { 'X-Amac-Token': token } });
    if (r.ok) {
      status.style.color = '#128a3d';
      status.textContent = 'connected to the amac daemon.';
    } else if (r.status === 401) {
      status.style.color = '#c0392b';
      status.textContent = 'daemon rejected the token (401). Re-copy it from ~/.amac/token.';
    } else {
      status.style.color = '#c0392b';
      status.textContent = `daemon responded ${r.status}.`;
    }
  } catch {
    status.style.color = '#c0392b';
    status.textContent = 'could not reach the daemon. Is `amac daemon` running and the tailnet up?';
  }
  setTimeout(() => ($('saved').textContent = ''), 1500);
});
