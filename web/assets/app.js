'use strict';

let csrfToken = '';
let config = null;
let toastTimer = null;
let generatedKeyDownloaded = false;
let lastETag = '';
let configETag = '';

const $ = (selector, root = document) => root.querySelector(selector);
const $$ = (selector, root = document) => Array.from(root.querySelectorAll(selector));

async function request(path, options = {}) {
  const headers = new Headers(options.headers || {});
  if (options.body) headers.set('Content-Type', 'application/json');
  if (options.method && options.method !== 'GET') headers.set('X-CSRF-Token', csrfToken);
  const response = await fetch(path, {...options, headers});
  // Whichever configuration this answer describes. Kept so the next save can
  // say which one it was editing, and be told when that is no longer the one
  // on disk - a second tab, or somebody at the SSH prompt.
  lastETag = response.headers.get('ETag') || '';
  const body = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(body.error || `${response.status} ${response.statusText}`);
  return body;
}

function toast(message, error = false) {
  const element = $('#toast');
  element.textContent = message;
  element.className = `show${error ? ' error' : ''}`;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { element.className = ''; }, 4000);
}

function setText(selector, text) { $(selector).textContent = text; }
function bytes(value) {
  if (!value) return '—';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) { value /= 1024; unit += 1; }
  return `${value.toFixed(unit > 1 ? 1 : 0)} ${units[unit]}`;
}
function duration(seconds) {
  const days = Math.floor(seconds / 86400);
  const hours = Math.floor((seconds % 86400) / 3600);
  const minutes = Math.floor((seconds % 3600) / 60);
  const secs = seconds % 60;
  return `${days ? `${days}d ` : ''}${String(hours).padStart(2, '0')}:${String(minutes).padStart(2, '0')}:${String(secs).padStart(2, '0')}`;
}
// What the kernel is actually doing about USB power saving, which is not the
// same question as what was saved. A backend serving read-only cannot write
// sysfs at all, and reporting the saved number as if it had arrived would hide
// precisely the case worth seeing.
function usbAutosuspendState(config) {
  const active = config.usb_autosuspend_active;
  const spell = value => (value < 0 ? 'off' : `${value} s`);
  if (active === null || active === undefined) return 'Kernel setting could not be read.';
  if (active === config.usb_autosuspend) return `Kernel: ${spell(active)}.`;
  return `Kernel still says ${spell(active)} — the saved setting has not taken effect.`;
}

function badge(selector, running) {
  const element = $(selector);
  element.textContent = running ? 'RUNNING' : 'STOPPED';
  element.className = `badge ${running ? 'running' : 'stopped'}`;
}

async function loadStatus() {
  try {
    const status = await request('/api/status');
    $('#overall-dot').className = 'dot online';
    setText('#overall-state', 'ONLINE');
    setText('#hostname', status.hostname);
    setText('#mdns', `${status.hostname}.local`);
    setText('#ip-address', status.ip_addresses[0] || 'No address');
    setText('#uptime', duration(status.uptime_seconds));
    setText('#cpu', `${status.cpu_percent.toFixed(1)} %`);
    setText('#memory', bytes(status.memory.used_bytes));
    setText('#memory-total', `${bytes(status.memory.total_bytes)} total`);
    setText('#storage', bytes(status.storage.free_bytes));
    setText('#storage-total', `${bytes(status.storage.total_bytes)} total`);
    setText('#dash-grbl-device', status.config.grbl_device);
    setText('#dash-grbl-port', `TCP :${status.config.grbl_port}`);
    setText('#dash-grbl-clients', status.clients.grbl);
    setText('#usb-autosuspend-state', usbAutosuspendState(status.config));
    // Normally empty. It is not empty on an appliance running an older slot
    // than the one that wrote /data/config.yaml, and then it is the first thing
    // worth reading on this page.
    const warnings = status.config_warnings || [];
    setText('#config-warning', warnings.join(' · '));
    $('#config-warning-card').hidden = warnings.length === 0;
    setText('#dash-camera-device', status.config.camera_device);
    setText('#dash-camera-mode', status.config.camera_mode);
    setText('#version', `LaserBridgeOS ${status.version}`);
    // Two GRBL backends ship; report on whichever one the configuration has
    // put in charge of the serial port.
    const backend = status.config.grbl_backend || 'ser2net';
    badge('#ser2net-badge', status.services[backend]);
    $$('.service-action[data-grbl]').forEach(button => { button.dataset.service = backend; });
    badge('#ustreamer-badge', status.services.ustreamer);
    renderServices(status.services);
    badge('#machine-camera-badge', status.services.ustreamer);
    // The same stream is shown on the dashboard and beside the steering, and
    // reassigning src restarts the connection - so it is only set when it
    // actually changed.
    if (status.services.ustreamer) {
      $$('.camera-stream').forEach(image => {
        if (image.dataset.src === status.config.camera_stream_url) return;
        image.dataset.src = status.config.camera_stream_url;
        image.src = status.config.camera_stream_url;
      });
    }
  } catch (error) {
    $('#overall-dot').className = 'dot offline';
    setText('#overall-state', 'OFFLINE');
  }
}

function renderServices(services) {
  const names = {'laserbridge-web': 'Web interface', ser2net: 'GRBL bridge (ser2net)', laserbridged: 'GRBL bridge (laserbridged)', ustreamer: 'Camera stream', sshd: 'SSH', 'avahi-daemon': 'mDNS / Bonjour', 'laserbridge-watchdog': 'Hardware watchdog'};
  $('#services').replaceChildren(...Object.entries(names).map(([key, label]) => {
    const row = document.createElement('div');
    row.className = 'service-row';
    const name = document.createElement('span'); name.textContent = label;
    const state = document.createElement('span'); state.className = `service-state ${services[key] ? 'running' : ''}`; state.textContent = services[key] ? 'RUNNING' : 'STOPPED';
    const button = document.createElement('button'); button.className = 'button small service-action'; button.dataset.service = key; button.dataset.action = 'restart'; button.textContent = 'Restart';
    row.append(name, state, button);
    return row;
  }));
}

async function loadMachine() {
  const workspace = $('#machine-workspace');
  const fallbackNote = $('#ser2net-note');
  let answer;
  try {
    answer = await request('/api/grbl');
  } catch (error) {
    workspace.hidden = true;
    fallbackNote.hidden = true;
    return;
  }
  // With ser2net in charge there is nothing to ask. Hiding the card and saying
  // nothing was the old behaviour, and an absence is not a statement: somebody
  // who switched backends a month ago has nothing left to remind them that the
  // beam watchdog went with it. So the panel says what is not being watched.
  workspace.hidden = !answer.available;
  fallbackNote.hidden = answer.available;
  $('#journal-unavailable').hidden = answer.available;
  if (!answer.available) return;

  const bridge = answer.bridge;
  const machine = bridge.machine || {};
  const state = machine.state || bridge.state;
  const element = $('#machine-badge');
  element.textContent = state;
  element.className = `badge ${machineClass(machine.state, bridge.state)}`;

  setText('#machine-bridge-state', bridge.state);
  setText('#machine-client', bridge.client || 'none');
  updateReadout(machine);
  setText('#machine-feed', machine.state ? `${machine.feed || 0} mm/min · ${machine.spindle || 0}` : '—');
  setText('#machine-bytes', `${bytes(bridge.rx_bytes)} in · ${bytes(bridge.tx_bytes)} out`);
  setText('#machine-beam', beamText(machine));
  // What is actually on the other end of the cable, and the one setting that
  // decides whether a feed hold switches the output off. Both used to be seen
  // and thrown away, and $32 is the difference between the appliance knowing
  // that a hold is enough and only hoping so.
  setText('#machine-firmware', firmwareText(machine));
  setText('#machine-job', jobText(bridge.job || {}));

  // Anything the bridge did on its own accord is worth saying plainly: an
  // operator who finds a paused machine should not have to guess why.
  updateSteering(bridge);

  const note = $('#machine-note');
  // Most specific first: a wrong baud rate explains everything else on the
  // page, so saying anything else while that is true would mislead.
  const text = bridge.gibberish
    ? 'The controller is answering with something that is not GRBL — check the baud rate.'
    : machine.laser_mode === 'off'
      ? 'Laser mode ($32) is off: this controller treats the laser as a spindle, and a feed hold does not switch a spindle off. Keep "on disconnect" on soft reset.'
      : bridge.controller_silent
        ? 'The controller has stopped answering while a client is connected.'
        : bridge.last_intervention || bridge.last_error || '';
  note.textContent = text;
  note.hidden = !text;

  // The workspace may have just appeared - a backend switched, a daemon that
  // came back - and the console follows it rather than the panel alone.
  updateConsolePolling();
}

// The record is read in two places, which is the point: the last few lines
// belong beside the machine an operator is standing at, and the whole thing
// belongs in a window of its own that can be left open next to it.
async function loadJournal() {
  const card = $('#journal-card');
  let answer;
  try {
    answer = await request('/api/grbl/journal?limit=50');
  } catch (error) {
    card.hidden = true;
    return;
  }
  card.hidden = !answer.available;
  $('#machine-record').hidden = !answer.available;
  if (!answer.available) return;

  const events = answer.events || [];
  if (!events.length) {
    const nothing = () => Object.assign(document.createElement('p'), {
      className: 'muted', textContent: 'Nothing has gone wrong yet.'});
    $('#journal').replaceChildren(nothing());
    $('#machine-journal').replaceChildren(nothing());
    return;
  }
  // Newest first on screen, oldest first in the record.
  const rows = events.slice().reverse().map(event => {
    const row = document.createElement('div');
    row.className = `journal-row journal-${event.kind}`;

    const when = document.createElement('time');
    // The wall clock depends on an RTC battery and a network that may not be
    // there. When it is obviously wrong, the uptime at least keeps the order
    // of events honest.
    when.textContent = event.unix > 1600000000
      ? new Date(event.unix * 1000).toLocaleString()
      : `+${duration(event.uptime_seconds || 0)} after boot`;
    const kind = document.createElement('span');
    kind.className = 'journal-kind';
    kind.textContent = event.kind;
    const what = document.createElement('span');
    what.className = 'journal-text';
    what.textContent = event.text || '';
    row.append(when, kind, what);

    if (event.position) {
      const where = document.createElement('span');
      where.className = 'journal-where';
      where.textContent = `X ${event.position.x} Y ${event.position.y}`;
      row.append(where);
    }
    if (event.context && event.context.length) {
      // The reason the journal exists: GRBL names no line number. Replies come
      // back in order, so usually the exact line is known — and when it is not,
      // the surrounding lines are still better than nothing.
      const context = document.createElement('pre');
      context.className = 'journal-context';
      context.textContent = event.context
        .map(line => (line === event.line ? `→ ${line}` : `  ${line}`))
        .join('\n');
      row.append(context);
    }
    return row;
  });
  $('#journal').replaceChildren(...rows);
  // Beside the machine, only what has just happened - a page an operator uses
  // while standing at a laser should not open onto fifty rows of history.
  $('#machine-journal').replaceChildren(...rows.slice(0, 8).map(row => row.cloneNode(true)));
  setText('#machine-record-count', `${events.length} entries`);
}

// The readout, which is where an operator looks first.
//
// Work coordinates are large and machine coordinates are small underneath, in
// that order and not the other way round: the operator set the work zero, works
// to it, and types go-to targets in it. The machine coordinates still have to
// be there - they are what the limits and the record are in - but they are a
// reference, not the reading.
//
// GRBL sends one coordinate system and the offset between them, the offset only
// every tenth report or so; the appliance remembers it and derives the other,
// which is why both can be shown from a report that carried one.
function updateReadout(machine) {
  const known = machine.has_position;
  const work = machine.work || {};
  const absolute = machine.position || {};
  for (const axis of ['x', 'y', 'z']) {
    setText(`#dro-${axis}`, known ? work[axis].toFixed(3) : '—');
    setText(`#dro-m${axis}`, known ? `machine ${absolute[axis].toFixed(3)}` : 'machine —');
  }
  setText('#dro-note', machine.has_offset
    ? 'work coordinates · G54 offset known'
    : 'work coordinates');
}

// What the controller said about its output, and when it said it.
//
// Three states, and "not known" is one of them: a page that showed an
// unreported laser as off would be asserting the one thing nobody may guess.
// The fourth case is subtler and was found on a real machine. GRBL mentions its
// outputs only alongside Ov:, every tenth report or so, and it reports what the
// output was at that instant - which in laser mode with M4 is genuinely off
// during rapids and between moves. So "off" beside a machine that is cutting at
// S450 is not a state, it is a stale sample, and printing it as "off" says more
// than the controller did.
//
// The age is computed from the appliance's own two timestamps rather than from
// this browser's clock, which may be years away from the appliance's if its RTC
// battery is flat.
function beamText(machine) {
  const said = {on: 'ON', off: 'off'}[machine.beam];
  if (!said) return 'not reported';
  const age = machine.last_report_unix && machine.beam_unix
    ? Math.max(0, machine.last_report_unix - machine.beam_unix)
    : null;
  if (machine.beam === 'off' && machine.state === 'Run' && (machine.spindle || 0) > 0) {
    return `not conclusive — last said off ${age === null ? 'earlier' : `${age} s ago`}, cutting at S${machine.spindle}`;
  }
  return age ? `${said} (as of ${age} s ago)` : said;
}

// The work in front of the machine, as opposed to what it is doing this
// instant. A pierce, a pause and a material change all look like "not moving",
// which is why the appliance keeps a job open across them - and why the guards
// that refuse to interrupt one finally mean what they say.
function jobText(job) {
  if (job.running) {
    const parts = [`running ${duration(job.seconds || 0)}`];
    if (job.lines) parts.push(`${job.lines} lines`);
    return parts.join(' · ');
  }
  if (!job.ended) return 'none yet';
  return `last: ${job.ended} after ${duration(job.seconds || 0)}${job.lines ? ` · ${job.lines} lines` : ''}`;
}

function firmwareText(machine) {
  const laser = {on: '$32 laser mode on', off: '$32 laser mode OFF — a feed hold does not stop a spindle'}[machine.laser_mode]
    || '$32 unknown';
  if (!machine.firmware) return `not identified — ${laser}`;
  return `${machine.firmware}${machine.options ? ` (${machine.options})` : ''} — ${laser}`;
}

function machineClass(machineState, bridgeState) {
  if (machineState === 'Alarm') return 'stopped';
  if (machineState) return 'running';
  return bridgeState === 'CLIENT_CONNECTED' || bridgeState === 'LISTENING' ? 'running' : 'stopped';
}

async function loadConfig() {
  config = await request('/api/config');
  configETag = lastETag;
  const grbl = $('#grbl-form').elements;
  grbl.device.value = config.grbl.device;
  grbl.baudrate.value = config.grbl.baudrate;
  grbl.port.value = config.grbl.port;
  grbl.max_connections.value = config.grbl.max_connections;
  grbl.reconnect.checked = config.grbl.reconnect;
  grbl.kick_old_user.checked = config.grbl.kick_old_user;
  grbl.backend.value = config.grbl.backend;
  grbl.on_disconnect.value = config.grbl.on_disconnect;
  grbl.monitor_port.value = config.grbl.monitor_port;
  grbl.stationary_beam_seconds.value = config.grbl.stationary_beam_seconds;
  const camera = $('#camera-form').elements;
  camera.device.value = config.camera.device;
  camera.format.value = config.camera.format;
  camera.resolution.value = config.camera.resolution;
  camera.fps.value = config.camera.fps;
  camera.port.value = config.camera.port;
  camera.quality.value = config.camera.quality;
  const system = $('#system-form').elements;
  system.hostname.value = config.system.hostname;
  system.usb_autosuspend.value = config.system.usb_autosuspend;
  system.network_mode.value = config.network.mode;
  system.ssh_enabled.checked = config.ssh.enabled;
  system.password_authentication.checked = config.ssh.password_authentication;
  system.wifi_enabled.checked = config.wifi.enabled;
  system.wifi_country.value = config.wifi.country;
  system.wifi_ssid.value = config.wifi.ssid;
  system.wifi_psk.value = '';
  system.wifi_hidden.checked = config.wifi.hidden;
}

async function loadDevices() {
  const devices = await request('/api/devices');
  $('#serial-devices').replaceChildren(...devices.serial.map(device => option(device.path)));
  $('#video-devices').replaceChildren(...devices.video.map(device => option(device.path)));
  const selected = devices.video.find(device => device.path === config?.camera.device) || devices.video[0];
  setText('#camera-capabilities', selected ? `${selected.name || selected.path}\n\n${selected.capabilities || 'Capabilities unavailable.'}` : 'No camera detected.');
}

async function loadUpdateStatus() {
  try {
    const status = await request('/api/update/status');
    setText('#update-version', status.current_version || 'development');
    setText('#update-current-slot', (status.current_slot || '—').toUpperCase());
    // Three different facts, and the middle one used to be missing entirely:
    // what is running, what a rollback would boot into, and which slot the
    // next boot will take. "Staged" showed the last installation whether or not
    // it had already booted, so an appliance reported the same version twice
    // and looked as though something were pending.
    const other = (status.previous_slot || '—').toUpperCase();
    setText('#update-previous', status.previous_version
      ? `${status.previous_version} · slot ${other}`
      : `slot ${other} · version not recorded yet`);
    setText('#update-staged', status.reboot_required
      ? `${status.staged_version} · slot ${status.staged_slot.toUpperCase()} — reboot to switch`
      : `slot ${(status.current_slot || '—').toUpperCase()} · this one, nothing pending`);
    const badgeElement = $('#update-badge');
    badgeElement.textContent = status.reboot_required ? 'REBOOT REQUIRED' : 'READY';
    badgeElement.className = `badge ${status.reboot_required ? 'stopped' : 'running'}`;
    setText('#update-auth-hint', status.default_password_in_use
      ? 'This appliance still has its default password, which cannot authorise an update. Change it under System, or attach a signature below.'
      : 'Authorises this update. Alternatively attach a signature below.');
  } catch (error) {
    setText('#update-message', error.message);
  }
}
function option(value) { const result = document.createElement('option'); result.value = value; return result; }

async function scanWiFi(button) {
  button.disabled = true;
  const message = button.dataset.message ? $(button.dataset.message) : null;
  if (message) message.textContent = 'Scanning without stopping the setup hotspot…';
  try {
    const result = await request('/api/wifi/scan');
    $('#wifi-networks').replaceChildren(...result.networks.map(network => {
      const item = option(network.ssid);
      item.label = `${network.signal_dbm.toFixed(0)} dBm · ${network.security}`;
      return item;
    }));
    const text = result.networks.length ? `${result.networks.length} Wi-Fi networks found` : 'No visible Wi-Fi networks found';
    if (message) message.textContent = text; else toast(text);
  } catch (error) {
    if (message) message.textContent = error.message; else toast(error.message, true);
  } finally { button.disabled = false; }
}

async function saveConfig(message) {
  const result = await request('/api/config', {
    method: 'PUT',
    headers: configETag ? {'If-Match': configETag} : {},
    body: JSON.stringify(config)
  });
  config = result.config;
  configETag = lastETag;
  toast(result.warnings?.length ? `${message}; ${result.warnings.join(', ')}` : message, Boolean(result.warnings?.length));
  await loadStatus();
}

$$('#journal-refresh, #journal-refresh-panel').forEach(button => {
  button.addEventListener('click', async () => {
    try { await loadJournal(); } catch (error) { toast(error.message, true); }
  });
});
$('#journal-window').addEventListener('click', () => {
  window.open(`${location.pathname}#journal`, 'laserbridge-record', 'width=980,height=760');
});
$('#grbl-form').addEventListener('submit', async event => {
  event.preventDefault();
  const f = event.currentTarget.elements;
  Object.assign(config.grbl, {device: f.device.value, baudrate: Number(f.baudrate.value), port: Number(f.port.value), max_connections: Number(f.max_connections.value), reconnect: f.reconnect.checked, kick_old_user: f.kick_old_user.checked, backend: f.backend.value, on_disconnect: f.on_disconnect.value, monitor_port: Number(f.monitor_port.value), stationary_beam_seconds: Number(f.stationary_beam_seconds.value)});
  try { await saveConfig('GRBL settings saved'); } catch (error) { toast(error.message, true); }
});
$('#camera-form').addEventListener('submit', async event => {
  event.preventDefault();
  const f = event.currentTarget.elements;
  Object.assign(config.camera, {device: f.device.value, format: f.format.value, resolution: f.resolution.value, fps: Number(f.fps.value), port: Number(f.port.value), quality: Number(f.quality.value)});
  try { await saveConfig('Camera settings saved'); } catch (error) { toast(error.message, true); }
});
$('#system-form').addEventListener('submit', async event => {
  event.preventDefault();
  const f = event.currentTarget.elements;
  config.system.hostname = f.hostname.value;
  config.system.usb_autosuspend = Number(f.usb_autosuspend.value);
  config.network.mode = f.network_mode.value;
  config.ssh.enabled = f.ssh_enabled.checked;
  config.ssh.password_authentication = f.password_authentication.checked;
  const wifi = {enabled: f.wifi_enabled.checked, country: f.wifi_country.value.toUpperCase(), ssid: f.wifi_ssid.value, psk: f.wifi_psk.value, hidden: f.wifi_hidden.checked};
  try {
    // Before anything else: a rejected password should not leave half the
    // settings applied, and it is the credential the rest now depends on.
    if (f.new_password.value || f.current_password.value) {
      await request('/api/system/password', {
        method: 'PUT',
        body: JSON.stringify({current: f.current_password.value, new: f.new_password.value})
      });
      f.current_password.value = '';
      f.new_password.value = '';
      toast('Appliance password changed');
      await loadUpdateStatus();
    }
    await saveConfig('System settings saved');
    await request('/api/wifi', {method: 'PUT', body: JSON.stringify(wifi)});
    Object.assign(config.wifi, wifi); delete config.wifi.psk;
    f.wifi_psk.value = '';
    toast('Settings saved; network may reconnect');
  } catch (error) { toast(error.message, true); }
});

$('#update-form').addEventListener('submit', async event => {
  event.preventDefault();
  // Hold on to the form: event.currentTarget is only set while the event is
  // being dispatched, and every handler here continues after an await, by
  // which time it reads null.
  const form = event.currentTarget;
  const button = $('#install-update');
  const fields = form.elements;
  if (!fields.bundle.files[0]) return;
  if (!fields.password.value && !fields.signature.files[0]) {
    setText('#update-message', 'Enter the appliance password, or attach a signature.');
    return;
  }
  // The password goes first so the appliance can reject a wrong one before
  // the whole bundle has crossed the network.
  const body = new FormData();
  if (fields.password.value) body.append('password', fields.password.value);
  body.append('bundle', fields.bundle.files[0]);
  if (fields.signature.files[0]) body.append('signature', fields.signature.files[0]);
  button.disabled = true;
  setText('#update-message', 'Uploading, verifying signature, and writing the inactive slot… Do not power off.');
  try {
    const response = await fetch('/api/update/install', {method: 'POST', headers: {'X-CSRF-Token': csrfToken}, body});
    const result = await response.json().catch(() => ({}));
    if (!response.ok) throw new Error(result.error || `Update failed (${response.status})`);
    setText('#update-message', `Version ${result.update.version} is staged in slot ${result.update.target_slot.toUpperCase()}. Reboot to activate it.`);
    form.reset();
    await loadUpdateStatus();
  } catch (error) {
    setText('#update-message', error.message);
  } finally { button.disabled = false; }
});

$('#rollback-update').addEventListener('click', async event => {
  const button = event.currentTarget;
  if (!window.confirm('Select the previous system slot for the next boot? Persistent configuration is kept.')) return;
  button.disabled = true;
  try {
    const result = await request('/api/update/rollback', {method: 'POST'});
    setText('#update-message', `Slot ${result.target_slot.toUpperCase()} is selected. Reboot to switch.`);
  } catch (error) { setText('#update-message', error.message); }
  finally { button.disabled = false; }
});

function selectKeyOption(value) {
  $('#password-field').hidden = value !== 'password';
  $('#generated-key-field').hidden = value !== 'generated';
  $('#public-key-field').hidden = value !== 'existing';
  $('#key-confirm').hidden = value !== 'generated';
  $('#setup-form').elements.public_key.required = value === 'existing';
}

$$('input[name="key_option"]').forEach(input => input.addEventListener('change', event => selectKeyOption(event.target.value)));

async function fetchPrivateKey() {
  const response = await fetch('/api/setup/ssh-key', {method: 'POST', headers: {'X-CSRF-Token': csrfToken}});
  if (!response.ok) {
    const body = await response.json().catch(() => ({}));
    throw new Error(body.error || 'Could not read the generated SSH key');
  }
  return response.text();
}

// Captive-portal windows (macOS and iOS open one automatically for the setup
// hotspot) run a stripped-down web view that silently ignores downloads. The
// button used to report success regardless, which looked like a download that
// had to be retried. Now the key is always shown as text as well, so it can be
// copied wherever the download does not arrive.
$('#download-key').addEventListener('click', async event => {
  const button = event.currentTarget;
  button.disabled = true;
  try {
    const key = await fetchPrivateKey();
    $('#key-text').value = key;
    $('#key-text-field').hidden = false;
    const blobURL = URL.createObjectURL(new Blob([key], {type: 'application/octet-stream'}));
    const link = document.createElement('a');
    link.href = blobURL;
    link.download = 'laserbridge_ed25519';
    document.body.appendChild(link);
    link.click();
    link.remove();
    setTimeout(() => URL.revokeObjectURL(blobURL), 1000);
    generatedKeyDownloaded = true;
    $('#setup-form').elements.key_saved.checked = false;
    setText('#setup-message', 'If no file arrived, copy the key shown below before continuing.');
  } catch (error) { setText('#setup-message', error.message); }
  finally { button.disabled = false; }
});

$('#show-key').addEventListener('click', async event => {
  const button = event.currentTarget;
  button.disabled = true;
  try {
    $('#key-text').value = await fetchPrivateKey();
    $('#key-text-field').hidden = false;
    $('#key-text').select();
    generatedKeyDownloaded = true;
  } catch (error) { setText('#setup-message', error.message); }
  finally { button.disabled = false; }
});

$('#setup-form').addEventListener('submit', async event => {
  event.preventDefault();
  const form = event.currentTarget;
  const fields = form.elements;
  const option = fields.key_option.value;
  const generated = option === 'generated';
  if (generated && (!generatedKeyDownloaded || !fields.key_saved.checked)) {
    setText('#setup-message', 'Save the generated private key and tick the confirmation before continuing.');
    return;
  }
  const payload = {
    hostname: fields.hostname.value,
    country: fields.country.value.toUpperCase(),
    ssid: fields.ssid.value,
    psk: fields.psk.value,
    hidden: fields.hidden.checked,
    use_generated_key: generated,
    public_key: option === 'existing' ? fields.public_key.value.trim() : '',
    password: option === 'password' ? fields.password.value : ''
  };
  $('#finish-setup').disabled = true;
  setText('#setup-message', 'Saving configuration…');
  try {
    const result = await request('/api/setup/complete', {method: 'POST', body: JSON.stringify(payload)});
    form.hidden = true;
    $('#setup-done').hidden = false;
    const url = `http://${result.hostname}`;
    $('#setup-next-url').href = url;
    setText('#setup-next-url', url);
    setText('#setup-ssh-command', result.ssh);
  } catch (error) {
    setText('#setup-message', error.message);
    $('#finish-setup').disabled = false;
  }
});

// Which panel is showing lives in the address, not in a variable.
//
// The record has no navigation entry any more - it belongs beside the machine,
// where the last few entries are - but it is still a page of its own, and the
// button on the machine page opens it in a second window to leave open next to
// the laser. That only works if a window can be pointed at a panel.
function showPanel(name) {
  const known = $(`#panel-${name}`) ? name : 'dashboard';
  $$('.nav-item').forEach(item => item.classList.toggle('active', item.dataset.panel === known));
  $$('.panel').forEach(panel => panel.classList.toggle('active', panel.id === `panel-${known}`));
  if (known === 'logs') loadLogs();
  if (known === 'journal') loadJournal();
  // The transcript is only recorded while somebody is reading it, so polling
  // follows the panel rather than running for the life of the page.
  updateConsolePolling();
}

window.addEventListener('hashchange', () => showPanel(location.hash.slice(1) || 'dashboard'));

document.addEventListener('click', async event => {
  const navigation = event.target.closest('.nav-item');
  if (navigation) {
    location.hash = navigation.dataset.panel;
    showPanel(navigation.dataset.panel);
  }
  const action = event.target.closest('.service-action');
  if (action) {
    action.disabled = true;
    try {
      await request(`/api/services/${encodeURIComponent(action.dataset.service)}/${encodeURIComponent(action.dataset.action)}`, {method: 'POST'});
      toast(`${action.dataset.service} ${action.dataset.action} requested`);
      setTimeout(loadStatus, 800);
    } catch (error) { toast(error.message, true); }
    finally { action.disabled = false; }
  }
  const wifiScan = event.target.closest('.wifi-scan');
  if (wifiScan) scanWiFi(wifiScan);
});

async function loadLogs() {
  try { const result = await request('/api/logs?limit=300'); setText('#logs', result.lines.join('\n') || 'No log entries.'); }
  catch (error) { setText('#logs', error.message); }
}

$('#refresh').addEventListener('click', loadStatus);
$('#refresh-logs').addEventListener('click', loadLogs);
$('#reboot').addEventListener('click', async () => {
  if (!window.confirm('Reboot LaserBridgeOS now? Active GRBL and camera connections will close.')) return;
  try { await request('/api/system/reboot', {method: 'POST'}); toast('Appliance is rebooting'); }
  catch (error) { toast(error.message, true); }
});
$$('.camera-stream').forEach(image => {
  image.addEventListener('load', event => event.target.classList.add('loaded'));
  image.addEventListener('error', event => event.target.classList.remove('loaded'));
});

// Steering.
//
// Two rules decide what this part of the page may do, and both live in the
// appliance rather than here - the buttons only reflect them. Nothing steers a
// machine somebody else is steering, so everything but Stop is refused while a
// client is connected. And the aiming beam is held on a lease: this page renews
// it every second, and if it stops - a closed laptop, a lost network, a tab
// that crashed - the beam goes out on its own. A switch would leave a laser on
// behind a browser that is no longer there.
let aimTimer = null;

function updateSteering(bridge) {
  const client = bridge.client || '';
  const machine = bridge.machine || {};
  const blocked = $('#control-blocked');
  const badge = $('#control-badge');

  // A machine takes time. A jog is answered when the controller accepts it, not
  // when the move is over, so the pad stays usable while the axes are still
  // running - but the page has to say that they are, and offer the way to stop
  // them that does not end the whole session.
  const moving = ['Run', 'Jog', 'Home'].includes(machine.state);
  $('#jog-cancel').hidden = !moving;
  setText('#jog-state', moving ? `${machine.state} — still moving` : '');

  // Homing is only offered where there is a homing cycle to run. Without $22
  // the controller answers error:5 and the operator gets a bare error code.
  const home = $('[data-command="home"]');
  home.title = machine.homing === 'off'
    ? 'This controller has no homing cycle configured ($22=0)'
    : 'Run the homing cycle';
  home.dataset.unavailable = machine.homing === 'off' ? 'yes' : '';
  // Stop is deliberately not disabled: it is the one command that ends the
  // conflict rather than joining it.
  $$('.steerable .button.jog, .steerable .button.control').forEach(b => {
    b.disabled = Boolean(client) || b.dataset.unavailable === 'yes';
  });
  $('#aim-on').disabled = Boolean(client);
  // The console keeps showing the traffic while a client is connected - that is
  // when it is most worth watching - but sending into somebody else's stream is
  // the one thing it must not offer.
  $('#console-send').disabled = Boolean(client);
  $('#console-line').disabled = Boolean(client);
  $('#console-line').placeholder = client
    ? 'A client is connected; the appliance sends nothing while somebody else is steering'
    : 'Type a command, e.g. $$ or G0 X10';
  $('#goto-send').disabled = Boolean(client);
  if (client) {
    badge.textContent = 'CLIENT IN CHARGE';
    badge.className = 'badge stopped';
    blocked.textContent = `${client} is connected and steering. The appliance will not send anything to a machine somebody else is driving — two applications on one laser is the failure it exists to prevent. Stop still works, and it ends their session as it goes.`;
    blocked.hidden = false;
    if (aimTimer) stopAiming();
  } else {
    badge.textContent = 'READY';
    badge.className = 'badge running';
    blocked.hidden = true;
  }
}

async function steer(command, body) {
  return request(`/api/grbl/control/${encodeURIComponent(command)}`, {
    method: 'POST',
    body: JSON.stringify(body || {})
  });
}

function stopAiming() {
  clearInterval(aimTimer);
  aimTimer = null;
  $('#aim-on').checked = false;
  steer('aim-off').catch(() => {});
}

$('#jog-step').addEventListener('change', event => {
  setText('#jog-step-label', `${event.target.value} mm`);
});

$('#aim-on').addEventListener('change', async event => {
  if (!event.target.checked) { stopAiming(); return; }
  const percent = Number($('#aim-percent').value) || 2;
  try {
    await steer('aim', {percent});
    // Renewed twice per lease. Missing one renewal must not put the beam out
    // during normal use; missing all of them must.
    aimTimer = setInterval(() => {
      steer('aim', {percent}).catch(() => stopAiming());
    }, 1000);
  } catch (error) {
    event.target.checked = false;
    toast(error.message, true);
  }
});

document.addEventListener('click', async event => {
  const jog = event.target.closest('.button.jog');
  if (jog) {
    const distance = Number($('#jog-step').value) * Number(jog.dataset.sign);
    try {
      await steer('jog', {axis: jog.dataset.axis, distance, feed: Number($('#jog-feed').value)});
    } catch (error) { toast(error.message, true); }
    return;
  }
  const control = event.target.closest('.button.control');
  if (!control) return;
  // The work origin is a go-to, not a command of its own: X0 Y0 in the
  // coordinate system the readout is showing, with Z left alone.
  if (control.dataset.command === 'goto-origin') {
    try {
      await steer('goto', {x: 0, y: 0, feed: Number($('#jog-feed').value)});
      toast('Moving to the work origin');
    } catch (error) { toast(error.message, true); }
    return;
  }
  try {
    await steer(control.dataset.command, control.dataset.axis ? {axis: control.dataset.axis} : {});
    toast(control.dataset.command === 'zero'
      ? `${control.dataset.axis} zeroed here`
      : `${control.dataset.command} sent`);
  } catch (error) { toast(error.message, true); }
});

// Go to a position rather than by a distance.
//
// An empty field means "leave this axis alone", which is why the values are
// only sent when they were actually typed: a go-to that quietly added Z0
// because the box was empty would drive the head into the bed.
$('#goto-form').addEventListener('submit', async event => {
  event.preventDefault();
  const body = {feed: Number($('#jog-feed').value)};
  for (const axis of ['x', 'y', 'z']) {
    const value = $(`#goto-${axis}`).value.trim();
    if (value !== '') body[axis] = Number(value);
  }
  if (body.x === undefined && body.y === undefined && body.z === undefined) {
    toast('Give at least one coordinate to move to', true);
    return;
  }
  try {
    await steer('goto', body);
    toast('Moving — Cancel jog stops it');
  } catch (error) { toast(error.message, true); }
});

$('#control-stop').addEventListener('click', async () => {
  try {
    if (aimTimer) stopAiming();
    await steer('stop');
    toast('Soft reset sent; the output is off and the job is over');
  } catch (error) { toast(error.message, true); }
});

// A page that goes away releases the beam at once rather than waiting for the
// lease to run out. Best effort: if it does not arrive, the lease covers it in
// three seconds.
//
// Not sendBeacon, which was the obvious choice and is the wrong one: it cannot
// set headers, so the request arrives without the CSRF token and is refused.
// It looked like a safety feature and was a 403 every time. fetch with keepalive
// carries the header and still outlives the page.
window.addEventListener('pagehide', () => {
  if (!aimTimer) return;
  fetch('/api/grbl/control/aim-off', {
    method: 'POST',
    headers: {'X-CSRF-Token': csrfToken, 'Content-Type': 'application/json'},
    body: '{}',
    keepalive: true
  }).catch(() => {});
});

// The console.
//
// It is the monitor port for people who do not have a terminal: the same marked
// transcript, with > for what the client sent, < for what the controller
// answered and * for the appliance's own commands. The daemon only assembles it
// while somebody is asking, so the polling below is what keeps it alive - and
// stopping the polling when this panel is not showing is not an optimisation,
// it is what puts the byte path back to carrying bytes and nothing else.
const consoleKeep = 400;
let consoleLines = [];
let consoleSeq = 0;
let consoleTimer = null;
let consoleAsking = false;
const consoleHistory = [];
let consoleHistoryAt = -1;

function consoleShowing() {
  return $('#panel-machine').classList.contains('active') && !$('#machine-workspace').hidden;
}

function updateConsolePolling() {
  if (consoleShowing() && !consoleTimer) {
    consoleTimer = setInterval(pollConsole, 1000);
    pollConsole();
  } else if (!consoleShowing() && consoleTimer) {
    clearInterval(consoleTimer);
    consoleTimer = null;
    setConsoleBadge('IDLE', false);
  }
}

function setConsoleBadge(text, recording) {
  const element = $('#console-badge');
  element.textContent = text;
  element.className = `badge ${recording ? 'running' : ''}`;
}

async function pollConsole() {
  // A tab in the background is not somebody reading. Asking would renew the
  // lease and keep the daemon assembling lines for a window nobody can see -
  // which is the thing the lease exists to prevent.
  if (consoleAsking || document.hidden) return;
  consoleAsking = true;
  try {
    const answer = await request(`/api/grbl/console?after=${consoleSeq}`);
    if (!answer.available) {
      setConsoleBadge('UNAVAILABLE', false);
      return;
    }
    setConsoleBadge(answer.recording ? 'RECORDING' : 'STARTING', answer.recording);
    if (answer.missed) {
      // A gap is said out loud rather than closed over: a transcript with a
      // hole in it that looks continuous is worse than no transcript.
      consoleLines.push({mark: '!', text: `… ${answer.missed} lines were not kept (the page was not reading fast enough)`});
    }
    if (answer.lines.length) {
      consoleLines.push(...answer.lines);
      consoleSeq = answer.next;
    }
    if (consoleLines.length > consoleKeep) consoleLines = consoleLines.slice(-consoleKeep);
    if (answer.lines.length || answer.missed) renderConsole();
  } catch (error) {
    setConsoleBadge('UNAVAILABLE', false);
  } finally { consoleAsking = false; }
}

function consoleFilters() {
  return $$('.console-filter').filter(box => box.checked).map(box => box.value);
}

// The appliance asks the controller for a status report every second whenever
// nobody else has, and the controller answers - so left alone the transcript is
// two lines a second of "? (status)" and "<Idle|MPos:…>" with the one line that
// matters somewhere in between. They are hidden unless asked for, which is what
// every sender's console does with the same traffic.
function isPolling(line) {
  return line.text === '? (status)' || (line.text.startsWith('<') && line.text.endsWith('>'));
}

function renderConsole() {
  const marks = consoleFilters();
  const polling = $('#console-status').checked;
  const needle = $('#console-search').value.trim().toLowerCase();
  const out = $('#console-out');
  // Only follow the tail while the reader is at the tail. Scrolling up to read
  // something and being yanked back down every second is the way a console
  // becomes useless.
  const atBottom = out.scrollHeight - out.scrollTop - out.clientHeight < 40;
  const shown = consoleLines.filter(line =>
    (line.mark === '!' || marks.includes(line.mark)) &&
    (polling || !isPolling(line)) &&
    (!needle || line.text.toLowerCase().includes(needle)));
  if (!shown.length) {
    out.replaceChildren(Object.assign(document.createElement('p'), {
      className: 'muted',
      textContent: consoleLines.length
        ? 'Nothing matches the filter. Status polling is hidden unless the box above is ticked.'
        : 'Nothing yet. The transcript is only kept while this page is open on it.'}));
    return;
  }
  const marker = {'>': 'sent', '<': 'received', '*': 'appliance', '!': 'gap'};
  out.replaceChildren(...shown.map(line => {
    const row = document.createElement('div');
    row.className = `console-line console-${marker[line.mark] || 'appliance'}`;
    const mark = document.createElement('span');
    mark.className = 'console-mark';
    mark.textContent = line.mark;
    const text = document.createElement('span');
    text.className = 'console-text';
    text.textContent = line.text;
    row.append(mark, text);
    return row;
  }));
  if (atBottom) out.scrollTop = out.scrollHeight;
}

$$('.console-filter, .console-filter-status').forEach(box => box.addEventListener('change', renderConsole));
// Coming back to a tab should not mean waiting out an interval for the first
// line, and leaving it should stop the recording without waiting either.
document.addEventListener('visibilitychange', () => { if (!document.hidden) pollConsole(); });
$('#console-search').addEventListener('input', renderConsole);
$('#console-clear').addEventListener('click', () => {
  // Clears this reader's view, not the record: the journal is the record and it
  // is on disk.
  consoleLines = [];
  renderConsole();
});

$('#console-form').addEventListener('submit', async event => {
  event.preventDefault();
  const field = $('#console-line');
  const line = field.value.trim();
  if (!line) return;
  try {
    await steer('send', {line});
    consoleHistory.push(line);
    consoleHistoryAt = consoleHistory.length;
    field.value = '';
    // The line appears in the transcript the moment the appliance writes it,
    // marked *, so there is nothing to echo here.
    pollConsole();
  } catch (error) { toast(error.message, true); }
});

$('#console-line').addEventListener('keydown', event => {
  if (event.key !== 'ArrowUp' && event.key !== 'ArrowDown') return;
  if (!consoleHistory.length) return;
  event.preventDefault();
  consoleHistoryAt += event.key === 'ArrowUp' ? -1 : 1;
  consoleHistoryAt = Math.max(0, Math.min(consoleHistory.length, consoleHistoryAt));
  event.target.value = consoleHistory[consoleHistoryAt] || '';
});

async function start() {
  try {
    const session = await request('/api/session');
    csrfToken = session.csrf_token;
    const setup = await request('/api/setup');
    if (setup.required) {
      $('#setup').hidden = false;
      setText('#setup-ap-ssid', setup.ap_ssid || 'LaserBridge-Setup');
      setText('#setup-ap-password', setup.ap_password);
      const fields = $('#setup-form').elements;
      fields.hostname.value = setup.hostname;
      fields.country.value = setup.country;
      fields.password.minLength = setup.min_password_length || 8;
      if (setup.default_password) {
        setText('#default-password-hint',
          `Currently "${setup.default_password}" — the same on every image, so change it here unless the network is fully trusted.`);
      }
      $('#download-key').disabled = !setup.generated_key_available;
      $('#show-key').disabled = !setup.generated_key_available;
      selectKeyOption('password');
      // A captive-portal window cannot download files and is easy to lose.
      if (/CaptiveNetworkSupport/i.test(navigator.userAgent)) {
        setText('#setup-message',
          'This is your system\'s captive-portal window. It cannot save downloads — open http://10.42.0.1 in a normal browser for the full setup.');
      }
    }
    await Promise.all([loadConfig(), loadStatus(), loadUpdateStatus()]);
    await loadDevices();
    await loadMachine();
    await loadJournal();
    showPanel(location.hash.slice(1) || 'dashboard');
    setInterval(loadStatus, 5000);
    // The machine reading is asked for more often than the rest: it is the
    // one thing on the page that changes while a job runs.
    setInterval(loadMachine, 2000);
    // The record changes only when something goes wrong; once a minute is
    // plenty, and the button is there for impatience.
    setInterval(loadJournal, 60000);
  } catch (error) { toast(error.message, true); }
}
start();
