// Forwards detections to the local amac daemon.
//
// The content script deliberately does not talk to the daemon directly: it
// runs in the page's origin, so a hostile page could see the request and the
// token. The service worker is isolated from page context, which is where a
// credential belongs.

const DEFAULTS = { endpoint: 'http://100.127.168.102:7788', token: '' };

async function config() {
  const stored = await chrome.storage.local.get(['endpoint', 'token']);
  return { ...DEFAULTS, ...stored };
}

chrome.runtime.onMessage.addListener((msg, _sender, sendResponse) => {
  if (msg?.type !== 'amac:application') return;

  (async () => {
    const { endpoint, token } = await config();
    if (!token) {
      console.warn('amac: no token set; open the extension options');
      sendResponse({ ok: false, error: 'no token' });
      return;
    }
    try {
      const r = await fetch(`${endpoint}/api/applications`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', 'X-Amac-Token': token },
        body: JSON.stringify({ ...msg.payload, source: 'extension' }),
      });
      const body = await r.json().catch(() => ({}));
      // A failure here is not retried. The confirmation email is the backstop,
      // so a dropped detection costs latency rather than data.
      sendResponse({ ok: r.ok, status: r.status, body });
    } catch (e) {
      console.warn('amac: daemon unreachable (is the tailnet up?)', e);
      sendResponse({ ok: false, error: String(e) });
    }
  })();

  return true; // keep the message channel open for the async reply
});
