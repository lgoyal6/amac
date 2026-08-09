// Inline handlers are blocked by the extension CSP, so wiring lives here.
const $ = id => document.getElementById(id);

chrome.storage.local.get(['endpoint', 'token']).then(cfg => {
  $('endpoint').value = cfg.endpoint || 'http://100.127.168.102:7788';
  $('token').value = cfg.token || '';
});

$('save').addEventListener('click', async () => {
  await chrome.storage.local.set({
    endpoint: $('endpoint').value.trim().replace(/\/+$/, ''),
    token: $('token').value.trim(),
  });
  $('saved').textContent = 'saved';
  setTimeout(() => ($('saved').textContent = ''), 1500);
});
