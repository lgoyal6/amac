
const $ = id => document.getElementById(id);
function node(tag,text,cls){const n=document.createElement(tag);n.textContent=String(text);if(cls)n.className=cls;return n;}
function rows(items){$('details').replaceChildren(...items.map(([title,body])=>{const r=node('div','','row');r.append(node('strong',title),node('small',body));return r;}));}
function error(e){$('result').textContent=e.message||String(e);$('result').className='status error';$('details').replaceChildren();}
function ready(text){$('result').textContent=text;$('result').className='status';}

const initial=[{name:'Nightly backup',runs:['delivered','delivered','delivered'],state:'delivered'},{name:'Invoice sync',runs:['failed','failed','delivered','delivered'],state:'delivered'},{name:'Digest',runs:['delivered','no-op','no-op'],state:'late'},{name:'Link checker',runs:['delivered','failed'],state:'failed'}];
let automations=structuredClone(initial),selected=0;
function render(){const a=automations[selected];ready(a.name+': '+a.state);$('tiles').replaceChildren(...a.runs.map((s,i)=>node('div',(i+1)+': '+s,'tile')));rows([['Latest run',a.runs.at(-1)],['Delivery health',a.state==='late'?'Late: no recent delivery, despite successful no-op runs.':a.state],['Historical failures',a.runs.filter(s=>s==='failed').length+' retained in this example']]);}
$('job').onchange=()=>{selected=Number($('job').value);render();};
for(const status of ['delivered','failed','no-op']){$(status).onclick=()=>{const a=automations[selected];a.runs.push(status);a.runs=a.runs.slice(-24);if(status==='delivered')a.state='delivered';else if(status==='failed')a.state='failed';render();};}
$('silence').onclick=()=>{automations[selected].state='late';render();};
$('reset').onclick=()=>{automations=structuredClone(initial);selected=0;$('job').value='0';render();};render();
