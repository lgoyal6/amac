const $ = id => document.getElementById(id);
const form = $('health-form');
const rows = $('automations');
const result = $('result');
const reports = $('reports');
let wasmReady = false;

function setStatus(message, isError = false) {
  result.textContent = message;
  result.className = isError ? 'status error' : 'status';
}

function addAutomation(values = {}) {
  const index = rows.children.length + 1;
  const fieldset = document.createElement('fieldset');
  fieldset.className = 'automation';
  const legend = document.createElement('legend');
  legend.textContent = `Automation ${index}`;
  fieldset.append(legend);

  const fields = [
    ['name', 'Name', 'text', values.name ?? '', 'e.g. Daily report'],
    ['cadence', 'Expected cadence (minutes)', 'number', values.cadence ?? '', 'e.g. 60'],
    ['grace', 'Grace period (minutes)', 'number', values.grace ?? '', 'e.g. 15'],
    ['age', 'Last delivery age (minutes)', 'number', values.age ?? '', 'Leave blank if unknown'],
  ];
  for (const [key, labelText, type, value, placeholder] of fields) {
    const label = document.createElement('label');
    label.textContent = labelText;
    const input = document.createElement('input');
    input.dataset.field = key;
    input.type = type;
    input.value = value;
    input.placeholder = placeholder;
    if (type === 'number') {
      input.step = '1';
      input.inputMode = 'numeric';
    }
    label.append(input);
    fieldset.append(label);
  }

  const stateLabel = document.createElement('label');
  stateLabel.textContent = 'Latest run or probe result';
  const select = document.createElement('select');
  select.dataset.field = 'outcome';
  for (const [value, label] of [
    ['healthy', 'Healthy'],
    ['no-op', 'No-op, no new delivery'],
    ['failed', 'Run failed'],
    ['down', 'Host unreachable'],
    ['probe-error', 'Probe error'],
  ]) {
    const option = document.createElement('option');
    option.value = value;
    option.textContent = label;
    select.append(option);
  }
  select.value = values.outcome ?? 'healthy';
  stateLabel.append(select);
  fieldset.append(stateLabel);

  const remove = document.createElement('button');
  remove.type = 'button';
  remove.className = 'remove secondary';
  remove.textContent = 'Remove automation';
  remove.addEventListener('click', () => {
    fieldset.remove();
    renumber();
    clearReport('Inputs changed. Run the health check again.');
  });
  fieldset.append(remove);
  rows.append(fieldset);
}

function renumber() {
  [...rows.children].forEach((fieldset, index) => {
    fieldset.querySelector('legend').textContent = `Automation ${index + 1}`;
  });
}

function clearReport(message = 'Enter sample data and run the health check.') {
  reports.replaceChildren();
  setStatus(message);
}

function payload() {
  return JSON.stringify({
    automations: [...rows.children].map(fieldset => {
      const value = key => fieldset.querySelector(`[data-field="${key}"]`).value.trim();
      const minutes = key => {
        const raw = value(key);
        if (raw === '') return null;
        const number = Number(raw);
        return Number.isFinite(number) ? number : 'invalid';
      };
      return {
        name: value('name'),
        cadence_minutes: minutes('cadence'),
        grace_minutes: minutes('grace'),
        last_delivery_age_minutes: minutes('age'),
        outcome: value('outcome'),
      };
    }),
  });
}

function renderReport(data) {
  if (data.error) {
    clearReport(data.error);
    result.classList.add('error');
    return;
  }
  const items = data.reports ?? [];
  const attention = items.filter(item => item.state !== 'ok').length;
  setStatus(`Evaluated ${items.length} automation${items.length === 1 ? '' : 's'}; ${attention} need${attention === 1 ? 's' : ''} attention.`);
  reports.replaceChildren(...items.map(item => {
    const article = document.createElement('article');
    article.className = `report state-${item.state}`;
    const title = document.createElement('h3');
    title.textContent = `${item.state.toUpperCase()}: ${item.name}`;
    const detail = document.createElement('p');
    detail.textContent = item.detail;
    article.append(title, detail);
    for (const note of item.notes ?? []) {
      const explanation = document.createElement('p');
      explanation.className = 'note';
      explanation.textContent = note;
      article.append(explanation);
    }
    if (item.err) {
      const error = document.createElement('p');
      error.className = 'note';
      error.textContent = `Probe detail: ${item.err}`;
      article.append(error);
    }
    return article;
  }));
}

async function loadWasm() {
  try {
    const response = await fetch('health.wasm', { cache: 'no-store' });
    if (!response.ok) throw new Error(`Health engine request failed (${response.status})`);
    const bytes = await response.arrayBuffer();
    const go = new Go();
    const instance = await WebAssembly.instantiate(bytes, go.importObject);
    const runtime = go.run(instance.instance);
    const deadline = Date.now() + 5000;
    while (!window.amacReady && Date.now() < deadline) {
      await new Promise(resolve => setTimeout(resolve, 10));
    }
    if (!window.amacReady) throw new Error('Health engine did not start');
    runtime.catch(error => setStatus(`Health engine stopped: ${error.message}`, true));
    wasmReady = true;
    $('evaluate').disabled = false;
    setStatus('Health evaluator ready. Enter inputs and run a check.');
  } catch (error) {
    setStatus(`Could not start the Go evaluator: ${error.message}`, true);
  }
}

$('add-automation').addEventListener('click', () => {
  if (rows.children.length >= 20) {
    clearReport('The evaluator accepts at most 20 automations.');
    return;
  }
  addAutomation();
  clearReport('Inputs changed. Run the health check.');
});
$('load-example').addEventListener('click', () => {
  rows.replaceChildren();
  addAutomation({ name: 'Daily report', cadence: '60', grace: '15', age: '90', outcome: 'no-op' });
  addAutomation({ name: 'Invoice sync', cadence: '60', grace: '15', age: '5', outcome: 'failed' });
  clearReport('Example inputs loaded. Run the health check to evaluate them.');
});
$('clear').addEventListener('click', () => {
  rows.replaceChildren();
  clearReport();
});
form.addEventListener('input', () => clearReport('Inputs changed. Run the health check again.'));
form.addEventListener('change', () => clearReport('Inputs changed. Run the health check again.'));
form.addEventListener('submit', event => {
  event.preventDefault();
  if (!wasmReady) {
    clearReport('The Go evaluator is still loading.');
    return;
  }
  try {
    renderReport(JSON.parse(window.amacEvaluate(payload())));
  } catch (error) {
    clearReport(`Could not evaluate these inputs: ${error.message}`);
    result.classList.add('error');
  }
});

$('evaluate').disabled = true;
addAutomation();
loadWasm();
