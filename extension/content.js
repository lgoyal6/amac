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

// Every major ATS puts the employer's own slug in the URL: greenhouse and
// lever and ashby as the first path segment, workday as the subdomain. That is
// far more reliable than anything in the page, because it is what the employer
// registered rather than whatever marketing put in an <h1>.
function companyFromUrl() {
  const h = location.hostname.toLowerCase();
  if (h.includes('myworkdayjobs.com') || h.includes('workday.com')) {
    const sub = h.split('.')[0];
    return sub && sub !== 'www' ? sub : '';
  }
  const seg = location.pathname.split('/').filter(Boolean)[0] || '';
  // Skip routing segments that are not an org.
  if (/^(jobs?|careers?|embed|applications?|en-us|search)$/i.test(seg)) return '';
  return seg;
}

// A trailing fragment is only a company if it does not look like a term, a
// season, a year, or a location. "Software Engineering Internship - Summer
// 2027" recorded its company as "Summer 2027" before this guard existed, which
// is the kind of wrong that quietly poisons a tracker.
function looksLikeCompany(s) {
  if (!s || s.length < 2) return false;
  if (/\b(19|20)\d{2}\b/.test(s)) return false;                       // any year
  if (/^(summer|fall|autumn|winter|spring|q[1-4]|h[12])\b/i.test(s)) return false;
  if (/^(intern(ship)?|full[- ]time|part[- ]time|contract|remote|hybrid|on[- ]?site)\b/i.test(s)) return false;
  if (/^(new grad|entry level|senior|junior|mid|staff|principal)\b/i.test(s)) return false;
  return true;
}

function titleize(slug) {
  const s = slug.replace(/[-_]+/g, ' ').trim();
  return s ? s[0].toUpperCase() + s.slice(1) : '';
}

function details() {
  const meta = n => document.querySelector(`meta[property="og:${n}"]`)?.content || '';
  const text = sel => document.querySelector(sel)?.textContent?.trim() || '';

  let role =
    text('h1') ||
    text('[data-qa="posting-name"]') ||
    text('.app-title') ||
    meta('title') ||
    document.title;

  // Explicit markup first, then the URL slug, then the title tail. Ordered by
  // how much each source can be trusted, not by how easy it is to read.
  let company =
    text('[data-qa="company-name"]') ||
    text('.company-name') ||
    meta('site_name') ||
    '';

  // Split "Role at Company" / "Role - Company" off the title regardless, since
  // the tail is noise in the role even when it is not a usable company.
  const m = role.match(/\s+(?:at|-|–|\|)\s+([^|\-–]{2,40})$/);
  if (m) {
    const tail = m[1].trim();
    role = role.slice(0, m.index).trim();
    if (!company && looksLikeCompany(tail)) company = tail;
  }

  if (!company) company = titleize(companyFromUrl());

  return { company: company.slice(0, 80), role: role.slice(0, 120) };
}

function looksConfirmed() {
  if (/confirmation|thank|submitted|success/i.test(location.pathname)) return true;
  // Bounded slice: scanning the whole body on a heavy SPA is wasteful and the
  // confirmation copy is always near the top.
  const body = (document.body?.innerText || '').slice(0, 4000);
  return CONFIRM.some(re => re.test(body));
}

// Mark the DOM so injection is observable from outside the isolated world.
// Content scripts are otherwise invisible: when one silently fails to inject
// there is nothing to distinguish "not running" from "running but decided not
// to fire", and those need completely different fixes. Costs one attribute.
try {
  document.documentElement.dataset.amacTracker = '0.1.0';
} catch {
  /* documentElement should always exist at document_idle, but never let a
     diagnostic be the thing that breaks detection. */
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
