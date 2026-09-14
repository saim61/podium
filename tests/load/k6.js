// Load profile for Podium.
//
// Deliberately weighted towards the write path. Hammering GET /v1/leaderboards would measure a
// single ZREVRANGE and prove nothing interesting; what is worth knowing is the cost of the path
// that holds a row lock, runs a Lua script across eight keys and publishes a notification.
//
//   docker run --rm -i --network host grafana/k6 run - < tests/load/k6.js
//
// Override with -e BASE_URL=... when the API is not on localhost.

import http from "k6/http";
import ws from "k6/ws";
import { check, sleep } from "k6";
import { Trend, Counter, Rate } from "k6/metrics";
import { randomIntBetween } from "https://jslib.k6.io/k6-utils/1.4.0/index.js";

const BASE = __ENV.BASE_URL || "http://localhost:8080";

// One account per virtual user, not a shared pool.
//
// Sharing them collides with a deliberate server rule: starting a session abandons any session
// the same player already had open for that game, so two VUs on one account stomp each other and
// the loser gets a 409. That is the server being right and the load script being wrong.
const ACCOUNTS = Number(__ENV.ACCOUNTS || 45);

const sessionLifecycle = new Trend("podium_session_lifecycle", true);
const finishDuration = new Trend("podium_finish_duration", true);
const boardRead = new Trend("podium_board_read", true);
const scoresRecorded = new Counter("podium_scores_recorded");
const snapshotsReceived = new Counter("podium_snapshots_received");
const errorRate = new Rate("podium_errors");

export const options = {
  scenarios: {
    // The main load: players opening sessions, playing them out and finishing.
    players: {
      executor: "ramping-vus",
      exec: "play",
      startVUs: 0,
      stages: [
        { duration: "20s", target: 25 },
        { duration: "40s", target: 25 },
        { duration: "10s", target: 0 },
      ],
    },

    // Anonymous traffic reading boards, which is what a public leaderboard actually gets.
    watchers: {
      executor: "constant-vus",
      exec: "watch",
      vus: 10,
      duration: "70s",
    },

    // A handful of sockets held open, to show the fan-out cost alongside everything else.
    sockets: {
      executor: "constant-vus",
      exec: "subscribe",
      vus: 5,
      duration: "70s",
    },
  },

  thresholds: {
    // A leaderboard read is one sorted-set range; if it is slow, something is wrong.
    "podium_board_read": ["p(95)<150"],
    // Finishing does the real work: a transaction, the Lua script and a publish.
    "podium_finish_duration": ["p(95)<400"],
    "podium_errors": ["rate<0.01"],
    "http_req_failed": ["rate<0.01"],
  },
};

// setup runs once and mints the accounts every VU shares.
//
// Registering per iteration is what a naive version of this script does, and it measures the
// rate limiter rather than the system: every k6 VU comes from one address, so the per-IP
// registration cap correctly refuses almost all of it. Accounts are a fixture here, not the
// thing under test.
export function setup() {
  const tokens = [];

  for (let i = 0; i < ACCOUNTS; i++) {
    const name = `load${Date.now()}x${i}`;

    const response = http.post(`${BASE}/v1/auth/register`, JSON.stringify({
      username: name,
      email: `${name}@example.com`,
      password: "correct horse battery",
    }), { headers: { "Content-Type": "application/json" }, tags: { name: "register" } });

    if (response.status === 201) tokens.push(response.json("tokens.access_token"));
  }

  if (tokens.length === 0) {
    throw new Error("could not register any load accounts; is the API up?");
  }
  console.log(`registered ${tokens.length} load accounts`);

  return { tokens };
}

// Keyed on the virtual user alone, so a VU always plays as the same account for the whole run.
function pick(data) {
  return data.tokens[__VU % data.tokens.length];
}

function authed(token, name) {
  return { headers: { "Content-Type": "application/json", Authorization: `Bearer ${token}` }, tags: { name } };
}

// play opens a memory session and answers correctly for a few levels, which is the cheapest way
// to drive a real score all the way through to the leaderboards.
export function play(data) {
  const token = pick(data);
  const started = Date.now();

  let response = http.post(`${BASE}/v1/games/memory/sessions`, null, authed(token, "start_session"));
  if (!check(response, { "session opened": (r) => r.status === 201 })) {
    errorRate.add(1);
    return;
  }

  const id = response.json("id");
  let sequence = response.json("state.sequence");

  for (let level = 0; level < 6; level++) {
    response = http.post(`${BASE}/v1/sessions/${id}/moves`,
      JSON.stringify({ answer: sequence }), authed(token, "move"));

    if (response.status !== 200) {
      errorRate.add(1);
      break;
    }
    if (response.json("score")) break;
    sequence = response.json("state.sequence");
  }

  const finishStart = Date.now();
  response = http.post(`${BASE}/v1/sessions/${id}/finish`, null, authed(token, "finish"));
  finishDuration.add(Date.now() - finishStart);

  if (check(response, { "session finished": (r) => r.status === 200 })) {
    scoresRecorded.add(1);
    errorRate.add(0);
  } else {
    errorRate.add(1);
  }

  sessionLifecycle.add(Date.now() - started);
  sleep(randomIntBetween(1, 3));
}

export function watch() {
  const start = Date.now();
  const response = http.get(`${BASE}/v1/leaderboards/global?limit=10`, { tags: { name: "board" } });
  boardRead.add(Date.now() - start);

  // 429 is a correct answer under load, not a failure - the limiter doing its job.
  const ok = check(response, { "board read": (r) => r.status === 200 || r.status === 429 });
  errorRate.add(ok ? 0 : 1);

  sleep(1);
}

export function subscribe(data) {
  const token = pick(data);

  const ticket = http.post(`${BASE}/v1/realtime/ticket`, null, authed(token, "ticket")).json("ticket");
  const url = `${BASE.replace("http", "ws")}/v1/ws?ticket=${ticket}`;

  ws.connect(url, {}, (socket) => {
    socket.on("open", () => {
      socket.send(JSON.stringify({ op: "subscribe", channels: ["memory:all-time", "global:all-time"] }));
    });
    socket.on("message", (raw) => {
      if (JSON.parse(raw).type === "snapshot") snapshotsReceived.add(1);
    });
    socket.setTimeout(() => socket.close(), 20000);
  });
}
