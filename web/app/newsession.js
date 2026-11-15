// New session: one draft screen with machine, project, worktree (an
// existing one or a new name), harness and the first prompt.

import { html, useState, useEffect } from './lib.js';
import { useStore, loadProjects, run } from './store.js';
import { newID } from './api.js';
import { Icon, TopBar, Empty } from './ui.js';

const NAME = /^[a-z0-9][a-z0-9._-]{0,62}$/;
const NEW = '\u0000new';

export function NewSession({ query }) {
  const st = useStore();
  const q = new URLSearchParams(query);
  const [machine, setMachine] = useState(q.get('machine') || '');
  const [ref, setRef] = useState(q.get('ref') || '');
  const [worktree, setWorktree] = useState(q.get('worktree') || 'main');
  const [newName, setNewName] = useState('');
  const [harness, setHarness] = useState(q.get('harness') || '');
  const [prompt, setPrompt] = useState('');
  const [busy, setBusy] = useState(false);
  // unsure holds a start whose outcome is not known: starting again with
  // the same choices reuses its ID, so at most one session is started.
  const [unsure, setUnsure] = useState(null);
  useEffect(() => { loadProjects(); }, []);

  const machines = (st.projects || []).filter((m) => m.online && m.projects.length);
  const mp = machines.find((m) => m.machine === machine) || machines[0];
  const proj = mp && (mp.projects.find((p) => p.ref === ref) || mp.projects.find((p) => p.box === 'running') || mp.projects[0]);
  const harnesses = proj ? proj.harnesses : [];
  const h = harnesses.find((x) => x.id === harness) || harnesses.find((x) => x.ready);
  const isNew = worktree === NEW;
  const wtName = isNew ? newName.trim() : worktree;
  const wtOK = !isNew || (NAME.test(wtName) && wtName !== 'main' && !(proj && proj.worktrees.some((w) => w.name === wtName)));
  const ready = mp && proj && h && h.ready && proj.box === 'running' && wtOK && !proj.problem;

  const submit = async (e) => {
    e.preventDefault();
    if (!ready || busy) return;
    setBusy(true);
    const params = { ref: proj.ref, harness: h.id, worktree: wtName || 'main', prompt: prompt.trim() };
    if (isNew) params.new_worktree = true;
    const sig = JSON.stringify([mp.machine, params]);
    const id = unsure && unsure.sig === sig && st.pending[unsure.id] ? unsure.id : newID();
    const rec = await run(mp.machine, 'start', params, { wait: 45, quiet: true, id });
    setBusy(false);
    setUnsure(rec && rec.outcome === 'unknown' ? { id, sig } : null);
    if (rec && rec.session && rec.outcome !== 'refused') location.hash = `#/s/${encodeURIComponent(mp.machine)}/${rec.session}`;
  };

  if (!st.projects) return html`<div class="screen"><${TopBar} title="New session" /><main class="content"><${Empty} icon="folder">Asking the machines…<//></main></div>`;
  if (!machines.length) return html`<div class="screen"><${TopBar} title="New session" /><main class="content"><${Empty} icon="server">No machine with projects is online.<//></main></div>`;
  return html`<div class="screen">
    <${TopBar} title="New session" />
    <main class="content">
      <form class="form" onSubmit=${submit}>
        <label>Machine
          <select value=${mp ? mp.machine : ''} onChange=${(e) => { setMachine(e.currentTarget.value); setRef(''); }}>
            ${!mp ? html`<option value="">Choose a machine</option>` : null}
            ${machines.map((m) => html`<option value=${m.machine}>${m.machine}</option>`)}
          </select></label>
        <label>Project
          <select value=${proj ? proj.ref : ''} onChange=${(e) => { setRef(e.currentTarget.value); setWorktree('main'); setHarness(''); }} disabled=${!mp}>
            ${!proj ? html`<option value="">Choose a project</option>` : null}
            ${(mp ? mp.projects : []).map((p) => html`<option value=${p.ref}>${p.ref}${p.box !== 'running' ? ` (box ${p.box})` : ''}</option>`)}
          </select></label>
        ${proj && proj.box !== 'running' ? html`<p class="note warn">The box of ${proj.ref} is ${proj.box}. Start it on the machine with <code>devc up ${proj.ref}</code>.</p>` : null}
        ${proj && proj.problem ? html`<p class="note warn">${proj.problem}</p>` : null}
        <label>Worktree
          <select value=${worktree} onChange=${(e) => setWorktree(e.currentTarget.value)} disabled=${!proj}>
            ${(proj ? proj.worktrees : [{ name: 'main' }]).map((w) => html`<option value=${w.name}>${w.name}${w.branch && w.branch !== w.name ? ` (${w.branch})` : ''}</option>`)}
            ${proj && !proj.worktrees.some((w) => w.name === 'main') ? html`<option value="main">main</option>` : null}
            <option value=${NEW}>New worktree…</option>
          </select></label>
        ${isNew ? html`<label>Name of the new worktree (and its branch)
          <input type="text" value=${newName} onInput=${(e) => setNewName(e.currentTarget.value)} placeholder="fix-login" autocapitalize="off" autocomplete="off" spellcheck="false" />
          ${newName && !wtOK ? html`<span class="field-err">Lower-case letters, digits, '.', '_' and '-'; not main and not an existing worktree.</span>` : null}</label>` : null}
        <label>Harness
          <select value=${h ? h.id : ''} onChange=${(e) => setHarness(e.currentTarget.value)} disabled=${!proj}>
            ${harnesses.map((x) => html`<option value=${x.id} disabled=${!x.ready}>${x.id}${x.ready ? '' : ' (not installed in the box)'}</option>`)}
          </select></label>
        <label>First prompt
          <textarea rows="5" value=${prompt} onInput=${(e) => setPrompt(e.currentTarget.value)} placeholder="What should the agent do? (optional)"></textarea></label>
        ${unsure && st.pending[unsure.id] ? html`<p class="note warn">The machine did not confirm the last start. Starting again with the same choices asks for the same session; it is started at most once.</p>` : null}
        <button class="btn primary wide" type="submit" disabled=${!ready || busy}><${Icon} name="play" />${busy ? 'Starting…' : 'Start session'}</button>
      </form>
    </main>
  </div>`;
}
