"use strict";

// Tokens live in memory only, never in localStorage or sessionStorage. A reload therefore signs
// you out, which is the price of not leaving a bearer token where any injected script on the page
// could read it. For a demo that trade is easy; a real client would use an httpOnly cookie.
const state = {
  access: null,
  refresh: null,
  username: null,
  games: [],
  game: null,
  session: null,
  ws: null,
  channel: null,
  timer: null,
  points: new Map(),
};

const $ = (id) => document.getElementById(id);

// ---------------------------------------------------------------- transport

async function api(method, path, body, { auth = true, retry = true } = {}) {
  const headers = {};
  if (body !== undefined) headers["Content-Type"] = "application/json";
  if (auth && state.access) headers["Authorization"] = `Bearer ${state.access}`;

  const response = await fetch(path, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
  });

  // One transparent refresh attempt. Access tokens last fifteen minutes, so a session left open
  // will hit this rather than appearing to break.
  if (response.status === 401 && auth && retry && state.refresh) {
    if (await refreshTokens()) {
      return api(method, path, body, { auth, retry: false });
    }
  }

  if (response.status === 204) return null;

  const payload = await response.json().catch(() => null);
  if (!response.ok) {
    const detail = payload?.error;
    const fields = detail?.fields ? ` (${Object.entries(detail.fields).map(([k, v]) => `${k} ${v}`).join(", ")})` : "";
    throw new Error(`${detail?.message ?? response.statusText}${fields}`);
  }
  return payload;
}

async function refreshTokens() {
  try {
    const tokens = await api("POST", "/v1/auth/refresh", { refresh_token: state.refresh }, { auth: false, retry: false });
    state.access = tokens.access_token;
    state.refresh = tokens.refresh_token;
    return true;
  } catch {
    signOut();
    return false;
  }
}

// ---------------------------------------------------------------- auth

function showError(id, message) {
  const node = $(id);
  node.textContent = message;
  node.hidden = !message;
}

async function signIn(tokens, username) {
  state.access = tokens.access_token;
  state.refresh = tokens.refresh_token;
  state.username = username;

  $("auth").hidden = true;
  $("play").hidden = false;
  $("who").innerHTML = "";
  $("who").append(document.createTextNode(`signed in as ${username} `));

  const out = document.createElement("button");
  out.className = "ghost";
  out.textContent = "sign out";
  out.onclick = signOut;
  $("who").append(out);

  await openSocket();
  renderBoard();
}

function signOut() {
  if (state.refresh) {
    api("POST", "/v1/auth/logout", { refresh_token: state.refresh }, { auth: false, retry: false }).catch(() => {});
  }
  state.access = state.refresh = state.username = null;
  state.session = null;
  state.ws?.close();
  state.ws = null;

  $("auth").hidden = false;
  $("play").hidden = true;
  $("who").textContent = "";
  $("stage").hidden = true;
  $("result").hidden = true;
  setSocketState(false);
}

$("login-form").onsubmit = async (event) => {
  event.preventDefault();
  showError("auth-error", "");
  try {
    const body = { login: $("login-id").value, password: $("login-pass").value };
    const out = await api("POST", "/v1/auth/login", body, { auth: false });
    await signIn(out.tokens, out.user.username);
  } catch (err) {
    showError("auth-error", err.message);
  }
};

$("register-form").onsubmit = async (event) => {
  event.preventDefault();
  showError("auth-error", "");
  try {
    const body = {
      username: $("reg-user").value,
      email: $("reg-email").value,
      password: $("reg-pass").value,
    };
    const out = await api("POST", "/v1/auth/register", body, { auth: false });
    await signIn(out.tokens, out.user.username);
  } catch (err) {
    showError("auth-error", err.message);
  }
};

// ---------------------------------------------------------------- games

async function loadGames() {
  const out = await api("GET", "/v1/games", undefined, { auth: false });
  state.games = out.games;

  const picker = $("game-picker");
  picker.innerHTML = "";
  for (const game of state.games) {
    const button = document.createElement("button");
    button.textContent = game.name;
    button.setAttribute("aria-pressed", "false");
    button.onclick = () => startGame(game.slug);
    picker.append(button);
  }

  const scope = $("board-scope");
  scope.innerHTML = "";
  scope.append(new Option("everything", "global"));
  for (const game of state.games) scope.append(new Option(game.name, game.slug));
}

function markPicked(slug) {
  [...$("game-picker").children].forEach((button, index) => {
    button.setAttribute("aria-pressed", String(state.games[index].slug === slug));
  });
}

async function startGame(slug) {
  showError("play-error", "");
  $("result").hidden = true;

  const game = state.games.find((g) => g.slug === slug);
  state.game = game;
  markPicked(slug);

  $("game-name").textContent = game.name;
  $("game-rules").hidden = false;
  $("game-rules").innerHTML = game.rules.map((rule) => `<li>${escapeHTML(rule)}</li>`).join("");

  // Watching the board for the game being played is the useful default.
  $("board-scope").value = slug;
  await subscribe();

  try {
    state.session = await api("POST", `/v1/games/${slug}/sessions`);
    $("stage").hidden = false;
    $("quit").hidden = false;
    render();
  } catch (err) {
    showError("play-error", err.message);
  }
}

async function move(payload) {
  try {
    state.session = await api("POST", `/v1/sessions/${state.session.id}/moves`, payload);
    render();
    if (state.session.score) showResult();
  } catch (err) {
    showError("play-error", err.message);
    // A deadline or an ended session is not recoverable by moving again, so settle up.
    if (/time limit|already ended/i.test(err.message)) await finish();
  }
}

async function finish() {
  if (!state.session) return;
  try {
    state.session = await api("POST", `/v1/sessions/${state.session.id}/finish`);
    render();
    showResult();
  } catch (err) {
    showError("play-error", err.message);
  }
}

$("quit").onclick = finish;

function showResult() {
  const score = state.session.score;
  if (!score) return;

  // A game can end before its deadline - answering every question, or giving up. The countdown
  // would otherwise keep running and try to finish an already-finished session.
  stopTimer();

  $("quit").hidden = true;
  $("stage").hidden = true;

  const placement = state.session.placement;
  const gameRank = placement?.game?.["all-time"]?.rank;
  const globalRank = placement?.global?.["all-time"]?.rank;

  $("result").hidden = false;
  $("result").innerHTML = `
    <h3>${escapeHTML(state.game.name)} finished</h3>
    <div class="points">${score.points.toLocaleString()} points</div>
    <dl>
      <dt>${escapeHTML(score.metric)}</dt><dd>${formatRaw(score.raw)}</dd>
      ${gameRank ? `<dt>rank in game</dt><dd>#${gameRank}</dd>` : ""}
      ${globalRank ? `<dt>rank overall</dt><dd>#${globalRank}</dd>` : ""}
    </dl>`;

  const again = document.createElement("button");
  again.textContent = "Play again";
  again.onclick = () => startGame(state.game.slug);
  $("result").append(again);
}

function formatRaw(raw) {
  return Number.isInteger(raw) ? String(raw) : raw.toFixed(1);
}

// ---------------------------------------------------------------- game views

function render() {
  const view = state.session?.state;
  if (!view || state.session.status !== "active") return;

  const renderers = {
    "reaction": renderReaction,
    "math-sprint": renderMathSprint,
    "memory": renderMemory,
    "word-scramble": renderScramble,
    "number-guess": renderNumberGuess,
  };
  (renderers[state.game.slug] ?? (() => {}))(view);
}

function stage(html) {
  // Any countdown belonging to the outgoing view has to die here. Otherwise switching from a
  // timed game to another one leaves the old ticker running, and it eventually calls finish()
  // on whatever session is current by then.
  stopTimer();
  $("stage").innerHTML = html;
  return $("stage");
}

function stopTimer() {
  if (state.timer) {
    clearInterval(state.timer);
    state.timer = null;
  }
}

function deadlineTicker(node, deadline) {
  const tick = () => {
    const left = Math.max(0, (new Date(deadline) - Date.now()) / 1000);
    node.textContent = left.toFixed(1);
    if (left <= 0) {
      stopTimer();
      finish();
    }
  };
  state.timer = setInterval(tick, 100);
  tick();
}

function renderReaction(view) {
  const root = stage(`
    <div class="meta"><span>round <b>${view.round}</b> of ${view.rounds}</span>
      <span>fouls <b>${view.fouls}</b></span></div>
    <div class="signal wait" id="signal">${view.armed ? "TAP!" : "ready when you are"}</div>
    <div class="echo" id="echo">${view.results.length ? view.results.map((ms) => `${ms}ms`).join(" · ") : "&nbsp;"}</div>`);

  const signal = root.querySelector("#signal");

  if (view.armed) {
    // The arm response is the go signal, so by the time this renders the clock is already
    // running. Tapping is all that is left.
    signal.classList.remove("wait");
    signal.classList.add("go");
    signal.onclick = () => {
      signal.onclick = null;
      signal.textContent = "…";
      move({ action: "tap" });
    };
    return;
  }

  const arm = document.createElement("button");
  arm.textContent = view.round === 1 ? "Start" : "Next round";
  arm.onclick = () => {
    arm.disabled = true;
    signal.textContent = "wait for it…";
    move({ action: "arm" });
  };
  root.append(arm);
}

function renderMathSprint(view) {
  const root = stage(`
    <div class="meta"><span>correct <b>${view.correct}</b></span>
      <span>wrong <b>${view.wrong}</b></span>
      <span><b id="left">--</b>s left</span></div>
    <div class="prompt">${escapeHTML(view.question)} = ?</div>
    <div class="row"><label style="flex:1">Answer
      <input id="answer" type="number" inputmode="numeric" autocomplete="off" autofocus></label>
      <button id="send">Submit</button></div>`);

  deadlineTicker(root.querySelector("#left"), view.deadline);

  const input = root.querySelector("#answer");
  const send = () => {
    if (input.value === "") return;
    move({ answer: Number(input.value) });
  };
  root.querySelector("#send").onclick = send;
  input.onkeydown = (event) => { if (event.key === "Enter") send(); };
  input.focus();
}

function renderMemory(view) {
  const root = stage(`
    <div class="meta"><span>level <b>${view.level}</b></span>
      <span>best <b>${view.completed}</b></span></div>
    <div class="prompt small" id="watch">watch…</div>
    <div class="pads" id="pads"></div>
    <div class="echo" id="echo">&nbsp;</div>`);

  const pads = root.querySelector("#pads");
  const echo = root.querySelector("#echo");
  const answer = [];

  for (let symbol = 1; symbol <= view.symbols; symbol++) {
    const pad = document.createElement("button");
    pad.className = "pad";
    pad.textContent = String(symbol);
    pad.disabled = true;
    pad.onclick = () => {
      answer.push(symbol);
      echo.innerHTML = `<b>${answer.join(" ")}</b>`;
      flash(pad);
      if (answer.length === view.sequence.length) {
        pads.querySelectorAll("button").forEach((b) => (b.disabled = true));
        move({ answer });
      }
    };
    pads.append(pad);
  }

  // Play the sequence back, then hand over. The server has already revealed it - remembering it
  // is the game, and that part cannot live on the server.
  playSequence(view.sequence, pads, root.querySelector("#watch"));
}

function flash(pad) {
  pad.classList.add("lit");
  setTimeout(() => pad.classList.remove("lit"), 180);
}

function playSequence(sequence, pads, label) {
  const buttons = [...pads.querySelectorAll("button")];
  let index = 0;

  const step = () => {
    if (index >= sequence.length) {
      label.textContent = "your turn";
      buttons.forEach((b) => (b.disabled = false));
      return;
    }
    flash(buttons[sequence[index] - 1]);
    index++;
    setTimeout(step, 520);
  };
  setTimeout(step, 450);
}

function renderScramble(view) {
  const root = stage(`
    <div class="meta"><span>solved <b>${view.solved}</b></span>
      <span>skipped <b>${view.skipped}</b></span>
      <span><b id="left">--</b>s left</span></div>
    <div class="prompt small">${escapeHTML(view.scramble.toUpperCase())}</div>
    <div class="row"><label style="flex:1">Unscramble
      <input id="word" autocomplete="off" autocapitalize="off" spellcheck="false" autofocus></label>
      <button id="send">Submit</button>
      <button id="skip" class="ghost">Skip</button></div>`);

  deadlineTicker(root.querySelector("#left"), view.deadline);

  const input = root.querySelector("#word");
  const send = () => {
    if (!input.value.trim()) return;
    move({ answer: input.value.trim(), skip: false });
  };
  root.querySelector("#send").onclick = send;
  root.querySelector("#skip").onclick = () => move({ answer: "", skip: true });
  input.onkeydown = (event) => { if (event.key === "Enter") send(); };
  input.focus();
}

function renderNumberGuess(view) {
  const hint = view.hint ? { higher: "go higher", lower: "go lower", correct: "correct!" }[view.hint] ?? view.hint : "";

  const root = stage(`
    <div class="meta"><span>round <b>${view.round}</b> of ${view.rounds}</span>
      <span>guesses left <b>${view.guesses_left}</b></span>
      <span>solved <b>${view.solved}</b></span></div>
    <div class="prompt">${view.low} – ${view.high}</div>
    <div class="echo">${escapeHTML(hint) || "&nbsp;"}</div>
    <div class="row"><label style="flex:1">Your guess
      <input id="guess" type="number" min="${view.low}" max="${view.high}" autocomplete="off" autofocus></label>
      <button id="send">Guess</button></div>`);

  const input = root.querySelector("#guess");
  const send = () => {
    if (input.value === "") return;
    move({ guess: Number(input.value) });
  };
  root.querySelector("#send").onclick = send;
  input.onkeydown = (event) => { if (event.key === "Enter") send(); };
  input.focus();
}

// ---------------------------------------------------------------- leaderboard

function setSocketState(live) {
  const badge = $("ws-state");
  badge.textContent = live ? "live" : "offline";
  badge.className = `badge ${live ? "live" : "offline"}`;
}

async function openSocket() {
  const { ticket } = await api("POST", "/v1/realtime/ticket");

  const url = `${location.protocol === "https:" ? "wss" : "ws"}://${location.host}/v1/ws?ticket=${ticket}`;
  const socket = new WebSocket(url);
  state.ws = socket;

  socket.onopen = () => { setSocketState(true); subscribe(); };
  socket.onclose = () => { setSocketState(false); state.ws = null; };
  socket.onmessage = (event) => {
    const frame = JSON.parse(event.data);
    if (frame.type === "snapshot" && frame.channel === state.channel) renderBoard(frame);
  };
}

async function subscribe() {
  const channel = `${$("board-scope").value}:${$("board-period").value}`;
  if (channel === state.channel) return;

  const socket = state.ws;
  if (socket?.readyState === WebSocket.OPEN) {
    if (state.channel) socket.send(JSON.stringify({ op: "unsubscribe", channels: [state.channel] }));
    socket.send(JSON.stringify({ op: "subscribe", channels: [channel] }));
  }

  state.channel = channel;
  state.points.clear();

  // Also fetch over HTTP: a socket only pushes on change, so a quiet board would stay blank.
  await refreshBoardFromAPI();
}

async function refreshBoardFromAPI() {
  const [scope, period] = state.channel.split(":");
  const path = scope === "global"
    ? `/v1/leaderboards/global?period=${period}&limit=10`
    : `/v1/leaderboards/${scope}?period=${period}&limit=10`;

  try {
    renderBoard(await api("GET", path, undefined, { auth: false }));
  } catch (err) {
    $("board-note").textContent = err.message;
  }
}

function renderBoard(page) {
  const body = $("board-rows");

  if (!page || !page.entries?.length) {
    body.innerHTML = `<tr class="empty"><td colspan="3">Nobody has played this yet.</td></tr>`;
    return;
  }

  body.innerHTML = "";
  for (const entry of page.entries) {
    const row = document.createElement("tr");
    if (entry.username === state.username) row.className = "me";

    // Highlight only rows whose points actually moved, so an update reads as a change rather
    // than the whole table blinking.
    if (state.points.has(entry.username) && state.points.get(entry.username) !== entry.points) {
      row.classList.add("bumped");
    }
    state.points.set(entry.username, entry.points);

    row.innerHTML = `<td>${entry.rank}</td><td>${escapeHTML(entry.username)}</td>
      <td>${entry.points.toLocaleString()}</td>`;
    body.append(row);
  }
}

$("board-scope").onchange = subscribe;
$("board-period").onchange = subscribe;

// ---------------------------------------------------------------- boot

function escapeHTML(value) {
  const node = document.createElement("span");
  node.textContent = String(value);
  return node.innerHTML;
}

loadGames().then(() => {
  state.channel = `${$("board-scope").value}:${$("board-period").value}`;
  return refreshBoardFromAPI();
}).catch((err) => showError("auth-error", err.message));
