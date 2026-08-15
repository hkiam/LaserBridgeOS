'use strict';

let csrfToken = '';
let config = null;
let toastTimer = null;
let generatedKeyDownloaded = false;

const $ = (selector, root = document) => root.querySelector(selector);
const $$ = (selector, root = document) => Array.from(root.querySelectorAll(selector));

async function request(path, options = {}) {
  const headers = new Headers(options.headers || {});
  if (options.body) headers.set('Content-Type', 'application/json');
  if (options.method && options.method !== 'GET') headers.set('X-CSRF-Token', csrfToken);
  const response = await fetch(path, {...options, headers});
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
    const preview = $('#camera-preview');
    if (status.services.ustreamer && preview.dataset.src !== status.config.camera_stream_url) {
      preview.dataset.src = status.config.camera_stream_url;
      preview.src = status.config.camera_stream_url;
    }
  } catch (error) {
    $('#overall-dot').className = 'dot offline';
    setText('#overall-state', 'OFFLINE');
  }
}

function renderServices(services) {
  const names = {'laserbridge-web': 'Web interface', ser2net: 'GRBL bridge (ser2net)', laserbridged: 'GRBL bridge (laserbridged)', ustreamer: 'Camera stream', sshd: 'SSH', 'avahi-daemon': 'mDNS / Bonjour'};
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
  const card = $('#machine-card');
  let answer;
  try {
    answer = await request('/api/grbl');
  } catch (error) {
    card.hidden = true;
    return;
  }
  // With ser2net in charge there is nothing to ask and nothing to show.
  card.hidden = !answer.available;
  if (!answer.available) return;

  const bridge = answer.bridge;
  const machine = bridge.machine || {};
  const state = machine.state || bridge.state;
  const element = $('#machine-badge');
  element.textContent = state;
  element.className = `badge ${machineClass(machine.state, bridge.state)}`;

  setText('#machine-bridge-state', bridge.state);
  setText('#machine-client', bridge.client || 'none');
  setText('#machine-position', machine.has_position
    ? `X ${machine.position.x.toFixed(2)}  Y ${machine.position.y.toFixed(2)}  Z ${machine.position.z.toFixed(2)}`
    : '—');
  setText('#machine-feed', machine.state ? `${machine.feed || 0} mm/min · ${machine.spindle || 0}` : '—');
  setText('#machine-bytes', `${bytes(bridge.rx_bytes)} in · ${bytes(bridge.tx_bytes)} out`);

  // Anything the bridge did on its own accord is worth saying plainly: an
  // operator who finds a paused machine should not have to guess why.
  const note = $('#machine-note');
  const text = bridge.controller_silent
    ? 'The controller has stopped answering while a client is connected.'
    : bridge.last_intervention || bridge.last_error || '';
  note.textContent = text;
  note.hidden = !text;
}

function machineClass(machineState, bridgeState) {
  if (machineState === 'Alarm') return 'stopped';
  if (machineState) return 'running';
  return bridgeState === 'CLIENT_CONNECTED' || bridgeState === 'LISTENING' ? 'running' : 'stopped';
}

async function loadConfig() {
  config = await request('/api/config');
  const grbl = $('#grbl-form').elements;
  grbl.device.value = config.grbl.device;
  grbl.baudrate.value = config.grbl.baudrate;
  grbl.port.value = config.grbl.port;
  grbl.max_connections.value = config.grbl.max_connections;
  grbl.reconnect.checked = config.grbl.reconnect;
  grbl.kick_old_user.checked = config.grbl.kick_old_user;
  grbl.backend.value = config.grbl.backend;
  grbl.on_disconnect.value = config.grbl.on_disconnect;
  const camera = $('#camera-form').elements;
  camera.device.value = config.camera.device;
  camera.format.value = config.camera.format;
  camera.resolution.value = config.camera.resolution;
  camera.fps.value = config.camera.fps;
  camera.port.value = config.camera.port;
  camera.quality.value = config.camera.quality;
  const system = $('#system-form').elements;
  system.hostname.value = config.system.hostname;
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
    setText('#update-staged', status.staged_version ? `${status.staged_version} · slot ${status.staged_slot.toUpperCase()}` : 'None');
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
  const result = await request('/api/config', {method: 'PUT', body: JSON.stringify(config)});
  config = result.config;
  toast(result.warnings?.length ? `${message}; ${result.warnings.join(', ')}` : message, Boolean(result.warnings?.length));
  await loadStatus();
}

$('#grbl-form').addEventListener('submit', async event => {
  event.preventDefault();
  const f = event.currentTarget.elements;
  Object.assign(config.grbl, {device: f.device.value, baudrate: Number(f.baudrate.value), port: Number(f.port.value), max_connections: Number(f.max_connections.value), reconnect: f.reconnect.checked, kick_old_user: f.kick_old_user.checked, backend: f.backend.value, on_disconnect: f.on_disconnect.value});
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

document.addEventListener('click', async event => {
  const navigation = event.target.closest('.nav-item');
  if (navigation) {
    $$('.nav-item').forEach(item => item.classList.toggle('active', item === navigation));
    $$('.panel').forEach(panel => panel.classList.toggle('active', panel.id === `panel-${navigation.dataset.panel}`));
    if (navigation.dataset.panel === 'logs') loadLogs();
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
$('#camera-preview').addEventListener('load', event => event.target.classList.add('loaded'));
$('#camera-preview').addEventListener('error', event => event.target.classList.remove('loaded'));

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
    setInterval(loadStatus, 5000);
    // The machine reading is asked for more often than the rest: it is the
    // one thing on the page that changes while a job runs.
    setInterval(loadMachine, 2000);
  } catch (error) { toast(error.message, true); }
}
start();
