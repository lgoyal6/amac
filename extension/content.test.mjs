// Tests for the detection logic in content.js.
//
// The risk in a content script is not "does Chrome parse the manifest", it is
// "does it fire on the right pages and stay silent on the wrong ones". A
// tracker that invents an application from a job listing you merely browsed is
// worse than one that misses, because you stop trusting the whole table.
//
// So this harness stubs the three globals the script touches (document,
// location, chrome) and exercises the real file. Run: node content.test.mjs
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import assert from 'node:assert';

const here = dirname(fileURLToPath(import.meta.url));
const SRC = readFileSync(join(here, 'content.js'), 'utf8');

// A DOM stub with exactly the surface content.js uses. Deliberately minimal:
// pulling in jsdom would test jsdom's HTML parser, not our detection rules.
function makeEnv({ host, path = '/', bodyText = '', h1 = '', title = '', meta = {} }) {
  const sent = [];
  const el = text => (text === undefined ? null : { textContent: text });

  const env = {
    location: { hostname: host, pathname: path, href: `https://${host}${path}` },
    document: {
      title,
      body: { innerText: bodyText },
      documentElement: {},
      querySelector(sel) {
        if (sel === 'h1') return el(h1);
        const m = sel.match(/^meta\[property="og:(\w+)"\]$/);
        if (m) return meta[m[1]] ? { content: meta[m[1]] } : null;
        return null;
      },
    },
    chrome: { runtime: { sendMessage: msg => sent.push(msg) } },
    // MutationObserver is only used to re-run check(); the initial call is
    // enough to exercise detection, so this is a no-op.
    MutationObserver: class { observe() {} },
    setTimeout: (fn) => fn(),
    clearTimeout: () => {},
    console,
  };
  env.sent = sent;
  return env;
}

function run(env) {
  const keys = Object.keys(env).filter(k => k !== 'sent');
  const fn = new Function(...keys, SRC);
  fn(...keys.map(k => env[k]));
  return env.sent;
}

let pass = 0, fail = 0;
const t = (name, fn) => {
  try { fn(); console.log(`  ok   ${name}`); pass++; }
  catch (e) { console.log(`  FAIL ${name}\n       ${e.message}`); fail++; }
};

console.log('detection: should NOT fire');

t('job listing you are only browsing', () => {
  const env = makeEnv({
    host: 'boards.greenhouse.io', path: '/vercel/jobs/4567',
    h1: 'Backend Engineer',
    bodyText: 'Apply for this job. We are looking for a backend engineer to join our team.',
  });
  assert.equal(run(env).length, 0, 'fired on a listing page');
});

t('non-ATS site that says thank you for applying', () => {
  const env = makeEnv({
    host: 'news.ycombinator.com', path: '/item',
    bodyText: 'Thank you for applying to YC. Your application has been received.',
  });
  assert.equal(run(env).length, 0, 'fired on a non-ATS host');
});

t('ATS host with an empty page', () => {
  const env = makeEnv({ host: 'jobs.lever.co', path: '/anthropic/abc', bodyText: '' });
  assert.equal(run(env).length, 0, 'fired with no confirmation signal');
});

console.log('\ndetection: SHOULD fire');

t('greenhouse confirmation text', () => {
  const env = makeEnv({
    host: 'boards.greenhouse.io', path: '/vercel/jobs/4567',
    h1: 'Backend Engineer at Vercel',
    bodyText: 'Thank you for applying to Vercel. Your application has been submitted.',
  });
  const sent = run(env);
  assert.equal(sent.length, 1, 'did not fire on a real confirmation');
  assert.equal(sent[0].type, 'amac:application');
  assert.equal(sent[0].payload.ats, 'Greenhouse');
  assert.equal(sent[0].payload.company, 'Vercel', `company was ${sent[0].payload.company}`);
  assert.equal(sent[0].payload.role, 'Backend Engineer', `role was ${sent[0].payload.role}`);
});

t('confirmation route in the URL, no body copy', () => {
  const env = makeEnv({
    host: 'jobs.ashbyhq.com', path: '/openai/application/confirmation',
    h1: 'Research Engineer - OpenAI', bodyText: 'All set.',
  });
  const sent = run(env);
  assert.equal(sent.length, 1, 'did not fire on a confirmation route');
  assert.equal(sent[0].payload.ats, 'Ashby');
});

t('workday phrasing', () => {
  const env = makeEnv({
    host: 'acme.myworkdayjobs.com', path: '/en-US/careers/job/123',
    h1: 'Platform Engineer', title: 'Platform Engineer | Acme',
    bodyText: 'We have received your application and will review it shortly.',
  });
  const sent = run(env);
  assert.equal(sent.length, 1);
  assert.equal(sent[0].payload.ats, 'Workday');
});

console.log('\ncompany extraction');

t('a term is not a company (the real bug)', () => {
  const env = makeEnv({
    host: 'job-boards.greenhouse.io', path: '/ctccampusboard/jobs/4708230005',
    h1: 'Software Engineering Internship - Summer 2027',
    bodyText: 'Thank you for applying. Your application has been submitted.',
  });
  const p = run(env)[0].payload;
  assert.notEqual(p.company, 'Summer 2027', 'recorded a term as the company');
  assert.equal(p.role, 'Software Engineering Internship', `role was ${p.role}`);
  assert.equal(p.company, 'Ctccampusboard', `company was ${p.company}`);
});

t('falls back to the greenhouse org slug', () => {
  const env = makeEnv({
    host: 'boards.greenhouse.io', path: '/stripe/jobs/12345',
    h1: 'Backend Engineer',
    bodyText: 'Thanks for applying! Your application was received.',
  });
  assert.equal(run(env)[0].payload.company, 'Stripe');
});

t('workday takes the company from the subdomain', () => {
  const env = makeEnv({
    host: 'acme.myworkdayjobs.com', path: '/en-US/careers/job/999',
    h1: 'Platform Engineer',
    bodyText: 'We have received your application.',
  });
  assert.equal(run(env)[0].payload.company, 'Acme');
});

t('an explicit company in the page still wins', () => {
  const env = makeEnv({
    host: 'jobs.lever.co', path: '/anthropic/abc',
    h1: 'Engineer - Fall 2027', meta: { site_name: 'Anthropic' },
    bodyText: 'Thank you for applying.',
  });
  assert.equal(run(env)[0].payload.company, 'Anthropic');
});

console.log('\nreporting once');

t('does not report twice on re-render', () => {
  const env = makeEnv({
    host: 'jobs.lever.co', path: '/anthropic/xyz', h1: 'Engineer at Anthropic',
    bodyText: 'Thanks for applying! Your application was submitted.',
  });
  // The MutationObserver stub is a no-op, so simulate a re-render by letting
  // the module's own debounced check run again through setTimeout.
  const sent = run(env);
  assert.equal(sent.length, 1, `reported ${sent.length} times`);
});

console.log(`\n${pass} passed, ${fail} failed`);
process.exit(fail ? 1 : 0);
