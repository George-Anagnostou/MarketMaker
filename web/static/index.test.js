const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const html = fs.readFileSync(path.join(__dirname, 'index.html'), 'utf8');
const script = html.match(/<script>([\s\S]*?)<\/script>/)?.[1];
if (!script) throw new Error('embedded application script not found');

const GAME_ID = '11111111-1111-4111-8111-111111111111';
const CREATE_ID = '22222222-2222-4222-8222-222222222222';
const QUOTE_ID = '33333333-3333-4333-8333-333333333333';
const QUIT_ID = '44444444-4444-4444-8444-444444444444';

class Element {
  constructor(tag = 'div') {
    this.tagName = tag.toUpperCase();
    this.children = [];
    this.parentElement = {hidden:false};
    this.textContent = '';
    this.className = '';
    this.hidden = false;
    this.disabled = false;
    this.value = '';
    this.attributes = new Map();
  }
  append(...children) { for (const child of children) { if (child && typeof child === 'object') child.parentElement = this; this.children.push(child); } }
  appendChild(child) { this.append(child); return child; }
  replaceChildren(...children) { this.children = []; this.append(...children); }
  setAttribute(name, value) { this.attributes.set(name, String(value)); }
  addEventListener() {}
  focus() { this.focused = true; }
  scrollIntoView() { this.scrolled = true; }
}

function response(status, body) {
  return {ok:status >= 200 && status < 300, status, async text() { return typeof body === 'string' ? body : JSON.stringify(body); }};
}

function loadApp(fetchImpl, timeout = 100) {
  const elements = new Map();
  const document = {
    activeElement:null,
    getElementById(id) {
      if (!elements.has(id)) elements.set(id, new Element());
      return elements.get(id);
    },
    createElement(tag) { return new Element(tag); },
    createTextNode(text) { const node = new Element('#text'); node.textContent = text; return node; },
    addEventListener() {}
  };
  document.getElementById('bid').value = '99.50';
  document.getElementById('ask').value = '100.50';
  document.getElementById('game-stage').hidden = true;
  const storage = new Map(), alerts = [];
  let uuid = 5;
  const context = {
    __MM_TEST_MODE__:true,
    __MM_HYDRATION_TIMEOUT_MS__:timeout,
    document,
    fetch:fetchImpl,
    localStorage:{getItem:key => storage.has(key) ? storage.get(key) : null, setItem:(key, value) => storage.set(key, String(value)), removeItem:key => storage.delete(key)},
    alert:message => alerts.push(String(message)),
    confirm:() => true,
    crypto:{randomUUID:() => `aaaaaaaa-aaaa-4aaa-8aaa-${String(uuid++).padStart(12, '0')}`},
    AbortController,
    setTimeout,
    clearTimeout,
    console
  };
  context.globalThis = context;
  vm.createContext(context);
  vm.runInContext(script, context, {filename:'index.html'});
  return {api:context.__MM_TEST__, elements, storage, alerts, document};
}

const scenario = {
  id:'first-spread-v1', revision:'1', title:'First Spread', briefing:'Practice.', objective:'Quote well.', turns:5
};

function state({version = 0, turn = 0, cash = '1000.00000000', position = '0.0000', mark = '100.0000', equity = '1000.00000000', isOver = false, reason = ''} = {}) {
  return {version, turn, cash, position, mark, equity, is_over:isOver, reason, best_bid:'0.0000', best_ask:'0.0000'};
}

function envelope(overrides = {}) {
  const current = overrides.state || state();
  return {
    game_id:GAME_ID,
    version:current.version,
    starting_equity:'1000.00000000',
    events_through:0,
    state:current,
    scenario,
    ...overrides
  };
}

function summary() {
  return {
    orders_received:0,
    units_traded:'0.0000',
    net_fill_cash:'0.00000000',
    storage_cost:'0.00000000',
    turn_pnl:'0.00000000',
    buy_volume:'0.0000',
    sell_volume:'0.0000',
    pnl_attribution:{execution_edge:'0.00000000', inventory_mark_pnl:'0.00000000', storage_pnl:'0.00000000'}
  };
}

function markEvent(sequence, commandID, mark, previous) {
  return {sequence, command_id:commandID, type:'mark_updated', mark, previous_mark:previous, message:`previous=${previous}`};
}

function oneTurnEnvelope(extra = {}) {
  return envelope({
    state:state({version:1, turn:1, mark:'101.0000'}),
    events_through:1,
    latest_turn:{turn:1, summary:summary(), coaching:{code:'turn', title:'Turn coach', body:'Review the fill.'}},
    coaching:{code:'turn', title:'Turn coach', body:'Review the fill.'},
    ...extra
  });
}

test('replayed create is only an acknowledgement and hydrates advanced canonical state', async () => {
  const calls = [];
  const app = loadApp(async (url, options = {}) => {
    calls.push({url, options});
    if (url === '/api/v2/games') return response(200, {game_id:GAME_ID, command:{id:CREATE_ID, type:'create_game', replayed:true}, version:0, state:state()});
    if (url === `/api/v2/games/${GAME_ID}`) return response(200, oneTurnEnvelope());
    if (url.includes('/events?')) return response(200, {events:[markEvent(1, QUOTE_ID, '101.0000', '100.0000')], next_after:1, has_more:false});
    throw new Error(`unexpected fetch ${url}`);
  });
  app.api.setCatalog([scenario], scenario.id);
  const result = await app.api.startNewGame({game_id:GAME_ID, command_id:CREATE_ID, scenario_id:scenario.id});
  assert.equal(result.status, 'started');
  assert.equal(app.api.model().version, 1);
  assert.equal(app.elements.get('turn').textContent, 'Turn 1');
  assert.equal(app.storage.get('mmg.game_id'), GAME_ID);
  assert.deepEqual(calls.map(call => call.url), ['/api/v2/games', `/api/v2/games/${GAME_ID}`, `/api/v2/games/${GAME_ID}/events?after=0&through=1&limit=200`]);
  assert.equal(calls[1].options.signal, calls[2].options.signal);
});

test('definitive create acknowledgement clears create retry but retains game ID when hydration fails', async () => {
  let createCalls = 0;
  const app = loadApp(async url => {
    if (url === '/api/v2/games') { createCalls++; return response(201, {game_id:GAME_ID, command:{id:CREATE_ID, type:'create_game', replayed:false}}); }
    return response(503, {error:{code:'storage_failure', message:'unavailable'}});
  });
  app.api.setCatalog([scenario], scenario.id);
  const result = await app.api.startNewGame({game_id:GAME_ID, command_id:CREATE_ID, scenario_id:scenario.id});
  assert.equal(result.status, 'failed');
  assert.equal(app.storage.get('mmg.game_id'), GAME_ID);
  assert.equal(app.storage.has('mmg.pending_create'), false);
  assert.equal(app.api.model().gameId, null);
  assert.equal(app.elements.get('start-default').disabled, false);
  assert.equal(app.elements.get('start-default').textContent, 'Resume lesson');
  assert.equal(app.api.model().retryHydrationID, GAME_ID);
  await app.api.startDefault();
  assert.equal(createCalls, 1);
  app.elements.get('scenario-options').children[0].onclick();
  assert.equal(app.api.model().retryHydrationID, null);
  assert.equal(app.elements.get('start-default').textContent, 'Start lesson');
  assert.equal(app.api.model().restoration, false);
});

test('play again resumes an acknowledged game instead of creating a duplicate', async () => {
  let createCalls = 0;
  const app = loadApp(async (url, options = {}) => {
    if (url === '/api/v2/games' && options.method === 'POST') { createCalls++; return response(201, {game_id:GAME_ID, command:{id:CREATE_ID, type:'create_game', replayed:false}}); }
    if (url === `/api/v2/games/${GAME_ID}`) return response(503, {error:{code:'storage_failure', message:'unavailable'}});
    throw new Error(`unexpected fetch ${url}`);
  });
  app.api.setCatalog([scenario], scenario.id);
  app.api.seedTestModel(GAME_ID, state({isOver:true, reason:'completed'}), '1000.00000000');
  await app.api.startNewGame({game_id:GAME_ID, command_id:CREATE_ID, scenario_id:scenario.id});
  assert.equal(app.elements.get('play-again').textContent, 'Resume lesson');
  await app.api.playAgain();
  assert.equal(createCalls, 1);
});

test('lesson picker renders catalog options and updates the lesson preview', () => {
  const app = loadApp(async () => { throw new Error('fetch not expected'); });
  const second = {...scenario, id:'inventory-pressure-v1', title:'Inventory Pressure', briefing:'Manage the position.', objective:'Reduce inventory.', turns:7};
  app.api.setCatalog([scenario, second]);
  const picker = app.elements.get('scenario-options');
  assert.equal(picker.children.length, 2);
  assert.equal(picker.children[0].children[0].children[0].textContent, 'First Spread');
  assert.equal(picker.children[1].children[0].children[0].textContent, 'Inventory Pressure');
  assert.equal(picker.children[0].attributes.get('aria-pressed'), 'false');
  assert.equal(app.elements.get('start-default').disabled, true);
  picker.children[1].onclick();
  assert.equal(picker.children[1].attributes.get('aria-pressed'), 'true');
  assert.equal(picker.children[1].focused, true);
  assert.equal(app.elements.get('start-default').disabled, false);
  assert.equal(app.elements.get('scenario-title').textContent, 'Inventory Pressure');
  assert.equal(app.elements.get('scenario-brief').textContent, 'Manage the position.');
  assert.equal(app.elements.get('scenario-objective').textContent, 'Goal: Reduce inventory.');
});

test('lesson and game stages replace one another', () => {
  const app = loadApp(async () => { throw new Error('fetch not expected'); });
  app.api.setCatalog([scenario]);
  assert.equal(app.api.model().view, 'lessons');
  app.api.seedTestModel(GAME_ID, state(), '1000.00000000');
  assert.equal(app.api.model().view, 'game');
  assert.equal(app.elements.get('lesson-stage').hidden, true);
  app.api.chooseAnotherLesson();
  assert.equal(app.api.model().view, 'lessons');
  assert.equal(app.elements.get('lesson-stage').hidden, false);
  assert.equal(app.elements.get('scenario-options').children[0].disabled, false);
  assert.equal(app.elements.get('start-default').textContent, 'Start lesson');
});

test('version conflict clears the quote and hydrates canonical state before controls resume', async () => {
  const calls = [], posted = [];
  const app = loadApp(async (url, options = {}) => {
    calls.push(url);
    if (options.method === 'POST') {
      posted.push(JSON.parse(options.body));
      return response(409, {error:{code:'version_conflict', message:'stale'}});
    }
    if (url === `/api/v2/games/${GAME_ID}`) return response(200, oneTurnEnvelope());
    return response(200, {events:[markEvent(1, QUOTE_ID, '101.0000', '100.0000')], next_after:1, has_more:false});
  });
  app.api.seedTestModel(GAME_ID, state(), '1000.00000000');
  await app.api.submitTurn();
  assert.equal(posted.length, 1);
  assert.equal(app.api.model().version, 1);
  assert.equal(app.api.model().retryCommand, null);
  assert.equal(app.elements.get('submitbtn').disabled, false);
  assert.equal(calls[1], `/api/v2/games/${GAME_ID}`);
});

test('state GET deadline abort preserves the prior model and restores controls', async () => {
  const app = loadApp((_url, options = {}) => new Promise((resolve, reject) => {
    options.signal.addEventListener('abort', () => reject(new Error('aborted')), {once:true});
  }), 10);
  app.api.seedTestModel(GAME_ID, state(), '1000.00000000');
  const hydration = app.api.hydrateGame(GAME_ID);
  assert.equal(app.elements.get('submitbtn').disabled, true);
  const result = await hydration;
  assert.equal(result.status, 'failed');
  assert.equal(app.api.model().version, 0);
  assert.equal(app.api.model().gameId, GAME_ID);
  assert.equal(app.elements.get('submitbtn').disabled, false);
  assert.equal(app.api.model().restoration, false);
});

test('event pagination pins every page to the state events_through bound', async () => {
  const eventURLs = [];
  const app = loadApp(async url => {
    if (url === `/api/v2/games/${GAME_ID}`) return response(200, envelope({state:state({version:2, turn:2, mark:'102.0000'}), events_through:2}));
    eventURLs.push(url);
    if (url.includes('after=0')) return response(200, {events:[markEvent(1, QUOTE_ID, '101.0000', '100.0000')], next_after:1, has_more:true});
    return response(200, {events:[markEvent(2, QUIT_ID, '102.0000', '101.0000')], next_after:2, has_more:false});
  });
  const result = await app.api.hydrateGame(GAME_ID);
  assert.equal(result.status, 'hydrated');
  assert.deepEqual(eventURLs, [
    `/api/v2/games/${GAME_ID}/events?after=0&through=2&limit=200`,
    `/api/v2/games/${GAME_ID}/events?after=1&through=2&limit=200`
  ]);
  assert.equal(app.api.model().historyLength, 3);
});

test('malformed successful command response retains the exact idempotency key and payload', async () => {
  let posted;
  const app = loadApp(async (_url, options) => { posted = JSON.parse(options.body); return response(200, {game_id:GAME_ID}); });
  app.api.seedTestModel(GAME_ID, state(), '1000.00000000');
  await app.api.submitTurn();
  assert.equal(app.api.model().version, 0);
  assert.equal(JSON.stringify(app.api.model().retryCommand), JSON.stringify(posted));
  assert.equal(app.api.model().retryCommand.id, posted.id);
  assert.equal(app.elements.get('submitbtn').textContent, 'Retry previous quote');
});

test('int64 mark and position values render without Number precision loss', () => {
  const app = loadApp(async () => { throw new Error('fetch not expected'); });
  app.api.seedTestModel(GAME_ID, state({cash:'0.00000000', position:'922337203685477.5807', mark:'0.0000', equity:'0.00000000'}), '0.00000000');
  assert.equal(app.elements.get('inventory').textContent, '922,337,203,685,477.58');
  app.api.seedTestModel(GAME_ID, state({cash:'0.00000000', position:'0.0000', mark:'922337203685477.5807', equity:'0.00000000'}), '0.00000000');
  assert.equal(app.elements.get('mid').textContent, '$922,337,203,685,477.58');
});

test('terminal player-quit coaching overrides stale turn coaching without dropping attribution', async () => {
  const latest = {turn:1, summary:summary(), coaching:{code:'old', title:'Latest turn', body:'Old turn advice.'}};
  const terminal = envelope({
    version:2,
    state:state({version:2, turn:1, mark:'101.0000', isOver:true, reason:'player_quit'}),
    events_through:2,
    latest_turn:latest,
    coaching:{code:'player_quit', title:'Session ended', body:'Review why you chose to stop.'}
  });
  const app = loadApp(async url => {
    if (url === `/api/v2/games/${GAME_ID}`) return response(200, terminal);
    return response(200, {events:[markEvent(1, QUOTE_ID, '101.0000', '100.0000'), {sequence:2, command_id:QUIT_ID, type:'game_ended', reason:'player_quit'}], next_after:2, has_more:false});
  });
  const result = await app.api.hydrateGame(GAME_ID);
  assert.equal(result.status, 'hydrated');
  assert.equal(app.elements.get('coaching').textContent, 'Session ended: Review why you chose to stop.');
  assert.equal(app.elements.get('coaching').hidden, false);
  assert.equal(app.elements.get('turn-attribution').hidden, false);
  assert.equal(app.elements.get('attribution-total').textContent, '$0.00');
});

test('malformed marked-total-P&L scorecard rejects hydration', async () => {
  const recap = {
    headline:'Review the desk', body:'Review.', end_reason:'completed', final_equity:'1000.00000000', total_pnl:'0.00000000', storage_paid:'0.00000000', max_abs_inventory:'0.0000', units_traded:'0.0000',
    scorecard:{focus_label:'Marked total P&L', focus_value:'not-a-decimal', focus_note:'Outcome.', reflection:'Reflect.'}
  };
  const app = loadApp(async url => {
    if (url === `/api/v2/games/${GAME_ID}`) return response(200, envelope({recap}));
    throw new Error(`unexpected fetch ${url}`);
  });
  const result = await app.api.hydrateGame(GAME_ID);
  assert.equal(result.status, 'failed');
  assert.equal(app.api.model().gameId, null);
});
