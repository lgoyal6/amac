// Detects that an application was actually submitted, on a page we already
// know is an ATS.
//
// The hard part is not finding the submit button, it is not firing on people
// who merely opened the form. So detection requires a post-submit signal: a
// confirmation page, a thank-you element, or a URL that moved to a
// confirmation route. A click alone is never enough, because forms fail
// validation constantly.

const ATS = [
  { host: 'greenhouse.io', name: 'Greenhouse' },
  { host: 'lever.co', name: 'Lever' },
  { host: 'ashbyhq.com', name: 'Ashby' },
  { host: 'myworkdayjobs.com', name: 'Workday' },
  { host: 'smartrecruiters.com', name: 'SmartRecruiters' },
  { host: 'icims.com', name: 'iCIMS' },
  { host: 'jobvite.com', name: 'Jobvite' },
  { host: 'bamboohr.com', name: 'BambooHR' },
];

const CONFIRM = [
  /thank(s| you) for (applying|your application|your interest)/i,
  /your application (has been )?(was )?(submitted|received)/i,
  /application (submitted|received|complete)/i,
  /we('| ha)ve received your application/i,
];

function ats() {
  const h = location.hostname.toLowerCase();
  return ATS.find(a => h.includes(a.host))?.name || null;
}

// Company and role are read from the page rather than guessed from the URL,
// because ATS URLs are opaque ids far more often than they are readable slugs.
function details() {
  const meta = n => document.querySelector(`meta[property="og:${n}"]`)?.content || '';
  const text = sel => document.querySelector(sel)?.textContent?.trim() || '';

  let role =
    text('h1') ||
    text('[data-qa="posting-name"]') ||
    text('.app-title') ||
    meta('title') ||
    document.title;

  let company =
    text('[data-qa="company-name"]') ||
    text('.company-name') ||
    meta('site_name') ||
    '';

  // Titles are very often "Role at Company" or "Role - Company".
  if (!company) {
    const m = role.match(/\s+(?:at|-|–|\|)\s+([^|\-–]{2,40})$/);
    if (m) { company = m[1].trim(); role = role.slice(0, m.index).trim(); }
  }
  return { company: company.slice(0, 80), role: role.slice(0, 120) };
}

function looksConfirmed() {
  if (/confirmation|thank|submitted|success/i.test(location.pathname)) return true;
  // Bounded slice: scanning the whole body on a heavy SPA is wasteful and the
  // confirmation copy is always near the top.
  const body = (document.body?.innerText || '').slice(0, 4000);
  return CONFIRM.some(re => re.test(body));
}

let reported = false;

function check() {
  if (reported) return;
  const name = ats();
  if (!name || !looksConfirmed()) return;

  reported = true;
  const d = details();
  chrome.runtime.sendMessage({
    type: 'amac:application',
    payload: { company: d.company, role: d.role, url: location.href, ats: name },
  });
}

// SPAs replace content without navigating, so watch the DOM as well as load.
// Debounced: a React re-render should not run the scan hundreds of times.
let timer = null;
const debounced = () => { clearTimeout(timer); timer = setTimeout(check, 400); };

check();
new MutationObserver(debounced).observe(document.documentElement, { childList: true, subtree: true });
