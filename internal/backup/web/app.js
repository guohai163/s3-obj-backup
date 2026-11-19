"use strict";

const app = document.querySelector('#app');
const editor = document.querySelector('#editor');
const picker = document.querySelector('#picker');
const notice = document.querySelector('#notice');
const tabs = { inventory: '▤', tasks: '◷', connections: '▱', activity: '≡' };
const state = {
  lang: localStorage.getItem('s3backup-language') || (navigator.language.startsWith('zh') ? 'zh' : 'en'),
  theme: localStorage.getItem('s3backup-theme') || (matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light'),
  tab: 'inventory', role: '', connections: [], buckets: [], tasks: [], runs: [],
  inventory: { bucket: 0, prefix: '', query: '', browse: false, pages: [], data: null },
  runPages: [],
};
const t = key => words[state.lang][key] || key;
const h = value => String(value ?? '').replace(/[&<>"']/g, char => ({ '&':'&amp;', '<':'&lt;', '>':'&gt;', '"':'&quot;', "'":'&#39;' })[char]);
const localError = message => state.lang === 'zh' ? (errorsZh[message] || message) : message;
const admin = () => state.role === 'admin';
const zone = () => Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC';
const date = value => value ? new Date(value).toLocaleString(state.lang === 'zh' ? 'zh-CN' : 'en') : t('none');
const number = value => Number(value).toLocaleString(state.lang === 'zh' ? 'zh-CN' : 'en');
const bucketLabel = id => {
  const bucket = state.buckets.find(item => item.id === Number(id));
  if (!bucket) return t('none');
  const connection = state.connections.find(item => item.id === bucket.connection_id);
  return connection ? bucket.name + ' · ' + connection.name : bucket.name;
};
const button = (text, action, style = '', attrs = '') =>
  `<button type="button" class="${style}" data-action="${action}" ${attrs}>${h(t(text))}</button>`;
const option = (value, text, selected) => `<option value="${h(value)}" ${String(value) === String(selected) ? 'selected' : ''}>${h(text)}</option>`;
const field = (label, input, style = '', help = '') =>
  `<label class="field ${style}"><span>${h(t(label))}</span>${input}${help ? `<small>${h(t(help))}</small>` : ''}</label>`;
const input = (name, value = '', extra = '') => `<input name="${name}" value="${h(value)}" ${extra}>`;
const select = (name, options, extra = '') => `<select name="${name}" ${extra}>${options}</select>`;
const check = (name, text, checked = false, attributes = '') =>
  `<label class="check"><input type="checkbox" name="${name}" ${checked ? 'checked' : ''} ${attributes}><span>${h(t(text))}</span></label>`;
const bucketsOptions = (selected, all = false) =>
  option(all ? 0 : '', t(all ? 'allBuckets' : 'choose'), selected) +
  state.buckets.map(b => option(b.id, bucketLabel(b.id), selected)).join('');
const activeRun = id => state.runs.some(run => run.task_id === id && ['queued','running'].includes(run.state));
const brand = '<span class="brand"><span class="mark" aria-hidden="true">S3</span><span>Object Backup</span></span>';

async function api(path, body) {
  const response = await fetch('/api/' + path, body === undefined ? {} : {
    method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
  });
  const result = await response.json();
  if (response.status === 401 && state.role && path !== 'login') {
    state.role = '';
    if (picker.open) picker.close();
    if (editor.open) editor.close();
    render();
    notify(t('sessionExpired'));
  }
  if (!response.ok) throw new Error(result.error || t('unavailable'));
  return result;
}

let noticeTimer;
function notify(message) {
  notice.textContent = localError(message);
  notice.hidden = false;
  clearTimeout(noticeTimer);
  noticeTimer = setTimeout(() => { notice.hidden = true; }, 4500);
}

function preferences() {
  document.documentElement.lang = state.lang === 'zh' ? 'zh-CN' : 'en';
  document.documentElement.dataset.theme = state.theme;
}
function controls() {
  return `<div class="controls">
    <button type="button" data-action="language" aria-label="${h(t('language'))}">${state.lang === 'zh' ? 'English' : '简体中文'}</button>
    <button type="button" data-action="theme" aria-label="${h(t(state.theme === 'dark' ? 'light' : 'dark'))}">
      <span aria-hidden="true">${state.theme === 'dark' ? '☀' : '☾'}</span></button>
    ${state.role ? button('signOut', 'logout') : ''}</div>`;
}
function render() {
  preferences();
  if (!state.role) {
    app.innerHTML = `<div class="login"><header class="topbar">${brand}${controls()}</header>
      <main class="login-main"><section class="card login-card" aria-labelledby="login-title">
      <h1 id="login-title">${t('signIn')}</h1><p class="muted">${t('signInSub')}</p>
      <form class="stack" data-form="login">
        ${field('user', input('user', '', 'required autocomplete="username"'))}
        ${field('password', input('password', '', 'required type="password" autocomplete="current-password"'))}
        <p class="message" role="alert" hidden></p><button class="primary">${t('signIn')}</button>
      </form></section></main></div>`;
    return;
  }
  const nav = Object.entries(tabs).map(([name, icon]) =>
    `<button data-action="tab" data-tab="${name}" ${name === state.tab ? 'aria-current="page"' : ''}>
      <span class="nav-icon" aria-hidden="true">${icon}</span>${t(name)}</button>`).join('');
  const page = { inventory: inventoryPage, tasks: tasksPage, connections: connectionsPage, activity: activityPage }[state.tab];
  app.innerHTML = `<div class="shell">
    <aside class="sidebar">${brand}<nav class="nav" aria-label="Workspace">${nav}</nav>
      <div class="sidebar-foot">${t(state.role)}</div></aside>
    <div><header class="topbar"><strong>${t(state.tab)}</strong>${controls()}</header>
    <main class="workspace" id="workspace">${page()}</main></div></div>`;
}
function intro(tab, actions = '') {
  return `<div class="intro"><div><h1>${t(tab)}</h1><p class="muted">${t(tab + 'Sub')}</p></div>
    <div class="actions">${actions}</div></div>`;
}
function empty(title, subtitle, icon, actions = '') {
  return `<div class="empty"><div class="empty-icon" aria-hidden="true">${icon}</div><h3>${t(title)}</h3>
    <p class="muted">${t(subtitle)}</p><div class="actions">${actions}</div></div>`;
}
function table(columns, rows) {
  return `<div class="scroll"><table><thead><tr>${columns.map(column => `<th scope="col">${t(column)}</th>`).join('')}</tr></thead>
    <tbody>${rows.join('')}</tbody></table></div>`;
}
function pager(back, next, action) {
  return back || next ? `<div class="pager">${button('back', action + '-back', '', back ? '' : 'disabled')}
    ${button('next', action + '-next', '', next ? '' : 'disabled')}</div>` : '';
}
function crumbs(prefix, action) {
  let directory = '';
  const parts = prefix ? prefix.slice(0, -1).split('/') : [];
  return `<div class="crumb">${button('root', action, 'link', 'data-key=""')}${parts.map(part => {
    directory += part + '/';
    return `<span class="muted" aria-hidden="true">/</span>${button(part || '/', action, 'link', `data-key="${h(directory)}"`)}`;
  }).join('')}</div>`;
}
function size(value) {
  if (!value) return '0 B';
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  const power = Math.min(units.length - 1, Math.floor(Math.log(value) / Math.log(1024)));
  return (value / 1024 ** power).toLocaleString(undefined, { maximumFractionDigits: 1 }) + ' ' + units[power];
}
function objectRows(objects, bucketColumn, openAction) {
  return objects.map(item => {
    const icon = item.directory ? '▱' : '▫';
    const label = `${icon}  ${item.key}`;
    return `<tr><td class="mono">${item.directory ? button(label, openAction, 'link mono', `data-key="${h(item.key)}"`) : h(label)}</td>
      ${bucketColumn ? `<td>${h(bucketLabel(item.bucket_id))}</td>` : ''}
      <td class="nowrap">${item.directory ? t('none') : size(item.size)}</td><td class="nowrap">${h(date(item.modified))}</td></tr>`;
  });
}
function inventoryPage() {
  const view = state.inventory;
  const b = state.buckets.find(item => item.id === view.bucket);
  if (!state.buckets.length) return intro('inventory') + `<section class="card">
    ${empty('noBuckets','noBucketsSub','▤',admin() ? button('addConnection','add-connection','primary') : '')}</section>`;
  const indexStatus = b ? `<span class="muted index-info">${b.indexed_at ?
    `${number(b.object_count)} ${t('objects')} · ${t('indexed')} ${h(date(b.indexed_at))}` : t('unindexed')}</span>` : '';
  const actions = admin() && b ? button('refreshIndex','index-here','primary') : '';
  const folderActions = admin() && b && view.browse ?
    button('backupHere','backup-here') + (view.prefix ? button('cleanupHere','cleanup-here') : '') : '';
  const objects = view.data?.items || [];
  const results = !view.data ? `<div class="empty muted">${t('pending')}</div>` : objects.length ?
    table(view.browse ? ['key','size','modified'] : ['key','bucket','size','modified'],objectRows(objects,!view.browse,'inventory-folder')) :
    empty('noObjects','noObjectsSub','▤');
  return intro('inventory',actions) + `<section class="card">
    <div class="card-head stack"><div class="inline">
      <div class="segment">
        ${button('searchMode','search-mode','',`aria-pressed="${!view.browse}"`)}
        ${button('browse','browse-mode','',`aria-pressed="${view.browse}"`)}</div>${indexStatus}</div>
      <form class="searchbar ${view.browse ? 'browse' : ''}" data-form="search">
        ${field('bucket',select('bucket',view.browse ? state.buckets.map(b => option(b.id,bucketLabel(b.id),view.bucket)).join('') : bucketsOptions(view.bucket,true)))}
        ${view.browse ? `<div class="actions">${folderActions}</div>` :
          field('key',input('query',view.query,`type="search" placeholder="${h(t('searchPlaceholder'))}"`)) + `<button class="primary">${t('search')}</button>`}
      </form>${view.browse ? crumbs(view.prefix,'inventory-folder') : ''}</div>
    ${results}${pager(view.pages.length,view.data?.more,'objects')}</section>`;
}
function scope(id,prefix) {
  return `<span>${h(bucketLabel(id))}</span>${prefix ? ` <span class="mono">/${h(prefix)}</span>` : ''}`;
}
function tasksPage() {
  const rows = state.tasks.map(task => {
    const s = task.settings;
    const edit = admin() ? button('edit','edit-task','',`data-id="${task.id}"`) : '';
    const run = admin() ? button(task.kind === 'cleanup' ? 'preview' : 'run','run-task','',
      `data-id="${task.id}" ${activeRun(task.id) ? 'disabled' : ''}`) : '';
    return `<tr><td><strong>${h(task.name)}</strong><br><small>${t(task.kind)}${s.move ? ' · ' + t('move') : ''}</small></td>
      <td>${scope(task.source_id,s.source_prefix)}${task.target_id ? `<br>→ ${scope(task.target_id,s.target_prefix)}` : ''}</td>
      <td>${task.enabled ? `<span class="mono">${h(s.cron)}</span><br><small>${h(s.timezone)} · ${h(date(task.next_run))}</small>` : t('manualOnly')}</td>
      ${admin() ? `<td><div class="actions">${run}${edit}</div></td>` : ''}</tr>`;
  });
  return intro('tasks',admin() ? button('addTask','add-task','primary') : '') + `<section class="card">${rows.length ?
    table(admin() ? ['name','scope','schedule',''] : ['name','scope','schedule'],rows) : empty('noTasks','noTasksSub','◷')}</section>`;
}
function connectionsPage() {
  const cards = state.connections.map(c => `<section class="card connection-card">
    <div class="inline"><div><h2>${h(c.name)}</h2><span class="mono muted">${h(c.endpoint)} · ${h(c.region)}</span></div>
    ${admin() ? `<div class="actions">${button('addBucket','add-bucket','',`data-id="${c.id}"`)}
    ${button('edit','edit-connection','',`data-id="${c.id}"`)}</div>` : ''}</div>
    <div class="bucket-list">${state.buckets.filter(b => b.connection_id === c.id)
      .map(b => button(b.name,'open-bucket','',`data-id="${b.id}"`)).join('') || `<small>${t('noBucketOnConnection')}</small>`}</div>
  </section>`).join('');
  return intro('connections',admin() ? button('addConnection','add-connection','primary') : '') +
    `<div class="stack">${cards || `<section class="card">${empty('noConnections','noConnectionsSub','▱')}</section>`}</div>`;
}
function activityPage() {
  const rows = state.runs.map(run => {
    const counts = ['scanned','copied','skipped','deleted','matched'].filter(name => run[name])
      .map(name => `${t(name)} ${number(run[name])}`).join(' · ');
    return `<tr><td><strong>${h(run.task_name)}</strong><br><small>#${run.id} · ${t(run.mode)}</small></td>
      <td><span class="pill ${h(run.state)}">${t(run.state)}</span></td><td class="nowrap">${h(date(run.created_at))}</td>
      <td>${h(counts || t('none'))}</td><td><div class="actions">
        ${button('runDetails','run-details','',`data-id="${run.id}"`)}
        ${run.mode === 'preview' && run.state === 'success' && run.matched ?
          button('previewList','preview-list','',`data-id="${run.id}"`) : ''}
        ${admin() && run.can_execute ? button('confirmDelete','execute-plan','danger',`data-id="${run.id}"`) : ''}
      </div></td></tr>`;
  });
  return intro('activity',button('refresh','reload')) + `<section class="card">${rows.length ?
    table(['name','state','started','counters',''],rows) : empty('noRuns','noRunsSub','≡')}
    ${pager(state.runPages.length,state.runs.length === 40,'runs')}</section>`;
}

function dialogFrame(title, body, { form = '', submit = 'save', danger = false } = {}) {
  return `<div class="dialog-head"><h2 id="dialog-title">${t(title)}</h2>${button('close','close-dialog','link')}</div>
    ${form ? `<form data-form="${form}">` : ''}
    <div class="dialog-body stack">${body}${form ? '<p class="message" role="alert" hidden></p>' : ''}</div>
    ${form ? `<footer class="dialog-foot">${button('cancel','close-dialog')}<button class="${danger ? 'danger' : 'primary'}">${t(submit)}</button></footer></form>` : ''}`;
}
function showDialog(title, body, options) {
  editor.setAttribute('aria-labelledby','dialog-title');
  editor.innerHTML = dialogFrame(title,body,options);
  if (!editor.open) editor.showModal();
}
function connectionEditor(connection = {}) {
  const editing = Boolean(connection.id);
  const body = `<div class="form-grid">
    ${editing ? '' : field('provider',select('preset',['generic','Cloudflare R2','MinIO'].map(name => option(name,t(name),'generic')).join('')),'full')}
    ${field('name',input('name',connection.name,'required maxlength="160"'),'full')}
    ${field('endpoint',input('endpoint',connection.endpoint,`required type="url" placeholder="https://…" ${editing ? 'readonly' : ''}`),'full')}
    ${field('region',input('region',connection.region || 'auto'))}
    ${check('path_style','pathStyle',connection.path_style ?? true)}
    ${field('accessKey',input('access_key','','autocomplete="off"'))}
    ${field('secretKey',input('secret_key','','type="password" autocomplete="new-password"'))}
    ${field('sessionToken',input('session_token','','type="password" autocomplete="new-password"'),'full')}
    ${editing ? `<small class="full">${t('credentialsHelp')}</small>` : ''}
    <input type="hidden" name="id" value="${connection.id || 0}"></div>`;
  showDialog(editing ? 'editConnection' : 'addConnection',body,{ form: 'connection' });
}
function bucketEditor(connectionID) {
  showDialog('addBucket',`
    ${field('connection',select('connection_id',state.connections.map(c => option(c.id,c.name,connectionID)).join(''),'required'))}
    ${field('bucketName',input('name','','required list="discovered-buckets"'))}
    <datalist id="discovered-buckets"></datalist>
    <div>${button('discover','discover')}</div>`,{ form: 'bucket' });
}
function datetimeLocal(value) {
  if (!value) return '';
  const time = new Date(value);
  return new Date(time.getTime() - time.getTimezoneOffset() * 60000).toISOString().slice(0,19);
}
function taskEditor(task = {}) {
  const s = task.settings || {};
  const kind = task.kind || 'backup';
  const folder = (label,name,value) => field(label,
    `<span class="inline">${input(name,value,'maxlength="1024"')}${button('pick','pick-folder','',`data-field="${name}"`)}</span>`,
    'full', name === 'source_prefix' && kind === 'cleanup' ? '' : 'rootOptional');
  const body = `<div class="form-grid">
    ${field('name',input('name',task.name,'required maxlength="160"'))}
    ${field('type',select('kind',['backup','cleanup','index'].map(k => option(k,t(k),kind)).join(''),task.id ? 'disabled' : ''))}
    ${field('source',select('source_id',bucketsOptions(task.source_id),'required'),'full')}
    <div class="full" data-for="backup cleanup">${folder('sourcePrefix','source_prefix',s.source_prefix)}</div>
    <div class="full" data-for="backup">${field('target',select('target_id',bucketsOptions(task.target_id)))}</div>
    <div class="full" data-for="backup">${folder('targetPrefix','target_prefix',s.target_prefix)}</div>
    <div class="full" data-for="backup">${check('move','move',s.move)}<small>${t('moveHelp')}</small></div>
    <div class="full" data-for="cleanup">${field('cleanupRule',select('cleanup_mode',
      ['all','before','retain'].map(k => option(k,t(k),s.cleanup_mode || 'before')).join('')))}</div>
    <div class="full" data-cleanup="before">${field('cutoff',input('cutoff',datetimeLocal(s.cutoff),'type="datetime-local" step="1"'))}</div>
    <div class="full" data-cleanup="retain">${field('retainDays',input('retain_days',s.retain_days || 30,'type="number" min="1" max="36500"'))}</div>
    <div class="full" data-destructive>${check('quiescent','quiescent',s.quiescent)}</div>
    <div class="full">${check('enabled','enabled',task.enabled)}</div>
    ${field('cron',input('cron',s.cron,'placeholder="0 3 * * *"'),'','cronHelp')}
    ${field('timezone',input('timezone',s.timezone || zone(),'required'))}
    <input type="hidden" name="id" value="${task.id || 0}"><input type="hidden" name="revision" value="${task.revision || 0}">
  </div>`;
  showDialog(task.id ? 'editTask' : 'addTask',body,{ form: 'task' });
  updateTaskFields(editor.querySelector('form'));
}
function updateTaskFields(form) {
  const fields = form.elements;
  const kind = fields.kind.value;
  form.querySelectorAll('[data-for]').forEach(section => { section.hidden = !section.dataset.for.split(' ').includes(kind); });
  form.querySelectorAll('[data-cleanup]').forEach(section => { section.hidden = kind !== 'cleanup' || section.dataset.cleanup !== fields.cleanup_mode.value; });
  form.querySelector('[data-destructive]').hidden = kind !== 'cleanup' && !(kind === 'backup' && fields.move.checked);
  fields.target_id.required = kind === 'backup';
  fields.source_prefix.required = kind === 'cleanup';
  fields.cutoff.required = kind === 'cleanup' && fields.cleanup_mode.value === 'before';
  fields.retain_days.required = kind === 'cleanup' && fields.cleanup_mode.value === 'retain';
  fields.quiescent.required = (kind === 'backup' && fields.move.checked) || (kind === 'cleanup' && fields.enabled.checked);
  fields.cron.required = fields.enabled.checked;
}

let folder;
async function chooseFolder(fieldName) {
  const fields = editor.querySelector('form').elements;
  const bucket = Number(fields[fieldName === 'target_prefix' ? 'target_id' : 'source_id'].value);
  if (!bucket) { (fieldName === 'target_prefix' ? fields.target_id : fields.source_id).focus(); return; }
  folder = { bucket, fieldName, prefix: '', after: '', items: [], more: false };
  picker.setAttribute('aria-label',t('folderList'));
  picker.innerHTML = `<div class="dialog-body">${t('pending')}</div>`;
  picker.showModal();
  await loadFolder('');
}
async function loadFolder(prefix, append = false) {
  const selected = folder;
  const request = new URLSearchParams({ bucket: selected.bucket, prefix, browse: 1, after: append ? selected.after : '' });
  const result = await api('objects?' + request);
  if (folder !== selected || !picker.open) return;
  selected.prefix = prefix;
  selected.after = result.items.at(-1)?.key || '';
  selected.more = result.more;
  selected.items = (append ? selected.items : []).concat(result.items.filter(item => item.directory));
  picker.innerHTML = dialogFrame('folderList',`<p class="muted">${h(bucketLabel(selected.bucket))}</p>
    ${crumbs(prefix,'picker-folder')}<div class="folder-options">${selected.items.map(item =>
      button('▱  ' + item.key,'picker-folder','link mono',`data-key="${h(item.key)}"`)).join('') ||
      (selected.more ? '' : `<small>${t('noFolders')}</small>`)}</div>
    <div class="actions">${button('selectFolder','choose-folder','primary')}${selected.more ? button('next','picker-next') : ''}</div>`);
}

function confirmRun(task,run) {
  showDialog('confirmDelete',`<p class="muted">${t(run ? 'removePlan' : 'movePlan')}${run ? ` (${number(run.matched)})` : ''}</p>
    ${check('confirm','confirmPaused',false,'required')}<input type="hidden" name="task" value="${task.id}">
    <input type="hidden" name="plan" value="${run?.id || ''}">`,{ form: 'confirm', submit: 'authorize', danger: true });
}
let preview;
async function showPreview(run, after = '', append = false) {
  if (!append) {
    preview = { run, items: [], after: '' };
    showDialog('previewList',`<p class="muted">${t('pending')}</p>`);
  }
  const selected = preview;
  const result = await api(`runs/${run.id}/items?` + new URLSearchParams({ after }));
  if (preview !== selected || !editor.open) return;
  selected.items = selected.items.concat(result.items);
  selected.after = result.items.at(-1)?.key || '';
  showDialog('previewList',`<div class="inline"><p class="muted">${t('matched')} ${number(run.matched)}</p>
    ${admin() && run.can_execute ? button('confirmDelete','execute-plan','danger',`data-id="${run.id}"`) : ''}</div>
    ${table(['key','size','modified'],objectRows(selected.items,false,''))}
    ${result.more ? button('next','preview-next') : ''}`);
}
function showRunDetails(run) {
  const counters = ['scanned','copied','skipped','deleted','matched'].map(key =>
    `<div><small>${t(key)}</small><br><strong>${number(run[key])}</strong></div>`).join('');
  showDialog('runDetails',`<h3>${h(run.task_name)} · #${run.id}</h3><div class="inline">
    <span class="pill ${h(run.state)}">${t(run.state)}</span><span class="muted">${h(date(run.created_at))}</span></div>
    <div class="inline" aria-label="${t('counters')}">${counters}</div>
    ${run.detail ? `<p class="message">${h(localError(run.detail))}</p>` : ''}`);
}

async function loadConfig() { Object.assign(state,await api('config')); }
async function loadRuns() {
  state.runs = await api('runs?' + new URLSearchParams({ before: state.runPages.at(-1) || 0 }));
}
let objectRequest = 0;
async function loadObjects({ reset = false } = {}) {
  const view = state.inventory;
  if (reset) view.pages = [];
  if (!state.buckets.length) { view.data = null; return; }
  if (view.browse && !view.bucket) view.bucket = state.buckets[0].id;
  const cursor = view.pages.at(-1);
  const requestID = ++objectRequest;
  const response = await api('objects?' + new URLSearchParams({
    bucket: view.bucket, prefix: view.browse ? view.prefix : '', q: view.browse ? '' : view.query,
    browse: view.browse ? 1 : 0, after: cursor?.key || '', after_bucket: cursor?.bucket_id || 0,
  }));
  if (requestID === objectRequest) view.data = response;
}
async function switchTab(tab) {
  state.tab = tab;
  render();
  if (tab === 'inventory') await loadObjects();
  if (tab === 'activity' || tab === 'tasks') await loadRuns();
  if (state.tab === tab) render();
}
async function queue(task,mode,plan = null,confirm = false) {
  await api(`tasks/${task.id}/run`,{ mode, plan_id: plan, confirm });
  if (editor.open) editor.close();
  state.runPages = [];
  notify(t('runQueued'));
  await switchTab('activity');
}

async function doAction(name, element) {
  const id = Number(element.dataset.id);
  const view = state.inventory;
  const task = state.tasks.find(item => item.id === id);
  const run = state.runs.find(item => item.id === id);
  switch (name) {
    case 'language': {
      const login = app.querySelector('[data-form="login"]');
      const user = login?.elements.user.value;
      const password = login?.elements.password.value;
      state.lang = state.lang === 'zh' ? 'en' : 'zh';
      localStorage.setItem('s3backup-language',state.lang);
      render();
      if (login) {
        const fields = app.querySelector('[data-form="login"]').elements;
        fields.user.value = user;
        fields.password.value = password;
      }
      break;
    }
    case 'theme':
      state.theme = state.theme === 'dark' ? 'light' : 'dark';
      localStorage.setItem('s3backup-theme',state.theme);
      preferences();
      element.innerHTML = `<span aria-hidden="true">${state.theme === 'dark' ? '☀' : '☾'}</span>`;
      element.setAttribute('aria-label',t(state.theme === 'dark' ? 'light' : 'dark'));
      break;
    case 'logout':
      await api('logout',{});
      state.role = '';
      state.tab = 'inventory';
      state.connections = state.buckets = state.tasks = state.runs = [];
      state.runPages = [];
      Object.assign(view,{ bucket: 0, prefix: '', query: '', browse: false, pages: [], data: null });
      render();
      break;
    case 'tab': await switchTab(element.dataset.tab); break;
    case 'close-dialog':
      element.closest('dialog').close();
      break;
    case 'add-connection': connectionEditor(); break;
    case 'edit-connection': connectionEditor(state.connections.find(c => c.id === id)); break;
    case 'add-bucket': bucketEditor(id); break;
    case 'discover': {
      const form = element.closest('form');
      const selected = form.elements.connection_id.value;
      const names = await api('connections/' + selected + '/buckets');
      if (form.elements.connection_id.value !== selected) break;
      form.querySelector('datalist').innerHTML = names.map(n => option(n,n,'')).join('');
      if (names.length === 1) form.elements.name.value = names[0];
      form.elements.name.focus();
      break;
    }
    case 'add-task': taskEditor(); break;
    case 'edit-task': taskEditor(task); break;
    case 'run-task':
      if (task.settings.move) confirmRun(task);
      else await queue(task,task.kind === 'cleanup' ? 'preview' : 'manual');
      break;
    case 'index-here': {
      const existing = state.tasks.find(item => item.kind === 'index' && item.source_id === view.bucket);
      taskEditor(existing || { kind: 'index', source_id: view.bucket, name: `${t('index')} · ${bucketLabel(view.bucket)}` });
      break;
    }
    case 'backup-here': case 'cleanup-here':
      taskEditor({ kind: name === 'backup-here' ? 'backup' : 'cleanup', source_id: view.bucket, settings: { source_prefix: view.prefix } });
      break;
    case 'open-bucket':
      Object.assign(view,{ bucket: id, prefix: '', browse: true, pages: [], data: null });
      await switchTab('inventory');
      break;
    case 'search-mode': case 'browse-mode':
      view.browse = name === 'browse-mode';
      view.prefix = '';
      await loadObjects({ reset: true });
      render();
      break;
    case 'inventory-folder':
      view.prefix = element.dataset.key;
      await loadObjects({ reset: true });
      render();
      break;
    case 'objects-next': case 'objects-back':
      if (name === 'objects-next') view.pages.push(view.data.items.at(-1));
      else view.pages.pop();
      await loadObjects();
      render();
      break;
    case 'pick-folder': await chooseFolder(element.dataset.field); break;
    case 'picker-folder': await loadFolder(element.dataset.key); break;
    case 'picker-next': await loadFolder(folder.prefix,true); break;
    case 'choose-folder':
      editor.querySelector('form').elements[folder.fieldName].value = folder.prefix;
      picker.close();
      break;
    case 'preview-list': await showPreview(run); break;
    case 'preview-next': await showPreview(preview.run,preview.after,true); break;
    case 'run-details': showRunDetails(run); break;
    case 'execute-plan': confirmRun(state.tasks.find(item => item.id === run.task_id),run); break;
    case 'runs-next': case 'runs-back':
      if (name === 'runs-next') state.runPages.push(state.runs.at(-1).id);
      else state.runPages.pop();
      await loadRuns();
      render();
      break;
    case 'reload':
      await loadConfig();
      await loadRuns();
      render();
      break;
  }
}
document.addEventListener('click',async event => {
  const control = event.target.closest('[data-action]');
  if (!control || control.disabled) return;
  const form = control.closest('form');
  control.disabled = true;
  try { await doAction(control.dataset.action,control); }
  catch (error) { showError(form,error); }
  finally { control.disabled = false; }
});
function showError(form,error) {
  const message = localError(error.message || t('unavailable'));
  const target = form?.querySelector('[role="alert"]');
  if (target && form.isConnected) {
    target.textContent = message;
    target.hidden = false;
  } else notify(message);
}
document.addEventListener('change',async event => {
  const form = event.target.form;
  if (!form) return;
  const fields = form.elements;
  switch (form.dataset.form) {
    case 'task': updateTaskFields(form); break;
    case 'connection':
      if (event.target.name !== 'preset') break;
      if (fields.preset.value === 'Cloudflare R2') {
        fields.endpoint.value = '';
        fields.endpoint.placeholder = 'https://ACCOUNT_ID.r2.cloudflarestorage.com';
        fields.region.value = 'auto';
      } else if (fields.preset.value === 'MinIO') {
        fields.endpoint.value = '';
        fields.endpoint.placeholder = 'http://minio:9000';
        fields.region.value = 'us-east-1';
      } else fields.endpoint.placeholder = 'https://…';
      fields.path_style.checked = true;
      break;
    case 'bucket':
      if (event.target.name === 'connection_id') form.querySelector('datalist').replaceChildren();
      break;
    case 'search':
      if (event.target.name === 'bucket' && state.inventory.browse) form.requestSubmit();
      break;
  }
});
document.addEventListener('submit',async event => {
  const form = event.target;
  if (!form.dataset.form) return;
  event.preventDefault();
  const submit = event.submitter;
  if (submit) submit.disabled = true;
  const fields = form.elements;
  const str = key => fields[key].value;
  const num = key => Number(str(key));
  const checked = key => fields[key].checked;
  try {
    switch (form.dataset.form) {
      case 'login': {
        state.role = (await api('login',{ user: str('user'), password: str('password') })).role;
        await loadConfig();
        await switchTab('inventory');
        break;
      }
      case 'search':
        Object.assign(state.inventory,{ bucket: num('bucket'), prefix: '',
          query: state.inventory.browse ? '' : str('query') });
        await loadObjects({ reset: true });
        render();
        break;
      case 'connection':
        await api('connections',{ id: num('id'), name: str('name'), endpoint: str('endpoint'),
          region: str('region'), path_style: checked('path_style'), access_key: str('access_key'),
          secret_key: str('secret_key'), session_token: str('session_token') });
        editor.close();
        await loadConfig();
        await switchTab('connections');
        notify(t('saved'));
        break;
      case 'bucket':
        await api('buckets',{ connection_id: num('connection_id'), name: str('name') });
        editor.close();
        await loadConfig();
        render();
        notify(t('saved'));
        break;
      case 'task': {
        const cutoff = str('cutoff');
        const target = num('target_id');
        await api('tasks',{ id: num('id'), revision: num('revision'), name: str('name'), kind: str('kind'),
          source_id: num('source_id'), target_id: target || null, enabled: checked('enabled'),
          settings: {
            source_prefix: str('source_prefix'), target_prefix: str('target_prefix'), move: checked('move'),
            quiescent: checked('quiescent'), cleanup_mode: str('cleanup_mode'),
            cutoff: cutoff ? new Date(cutoff).toISOString() : null, retain_days: num('retain_days'),
            cron: str('cron'), timezone: str('timezone'),
          },
        });
        editor.close();
        await loadConfig();
        await switchTab('tasks');
        notify(t('saved'));
        break;
      }
      case 'confirm':
        await queue({ id: num('task') },'manual',num('plan') || null,checked('confirm'));
        break;
    }
  } catch (error) { showError(form,error); }
  finally { if (submit) submit.disabled = false; }
});
let polling = false;
setInterval(async () => {
  if (!state.role || document.hidden || polling || (state.tab !== 'activity' && state.tab !== 'tasks')) return;
  if (editor.open || picker.open || (state.tab === 'activity' && state.runPages.length)) return;
  polling = true;
  const tab = state.tab;
  const previous = JSON.stringify(state.runs);
  try {
    await loadRuns();
    if (state.tab === tab && previous !== JSON.stringify(state.runs)) render();
  } catch (error) { if (state.role) notify(error.message); }
  finally { polling = false; }
},4000);

render();
(async () => {
  try {
    state.role = (await api('session')).role;
    await loadConfig();
    await switchTab('inventory');
  } catch (error) { if (state.role) notify(error.message); }
})();
