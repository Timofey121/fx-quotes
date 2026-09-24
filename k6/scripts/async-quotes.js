import http from 'k6/http';
import { check, sleep } from 'k6';
import { Counter, Rate, Trend } from 'k6/metrics';

const baseURL = __ENV.FX_QUOTES_K6_BASE_URL || 'http://127.0.0.1:8080';
const expectedRate = __ENV.FX_QUOTES_K6_EXPECTED_RATE || '0.9234';
const profile = __ENV.FX_QUOTES_K6_PROFILE || 'load';
const runID = __ENV.FX_QUOTES_K6_RUN_ID || `local-${Date.now()}`;
const resultDeadlineMs = positiveIntegerEnv('FX_QUOTES_K6_RESULT_DEADLINE_MS', 3000);
const pollIntervalMs = positiveIntegerEnv('FX_QUOTES_K6_POLL_INTERVAL_MS', 25);
const requestTimeoutMs = durationEnv('FX_QUOTES_K6_HTTP_TIMEOUT', '2s');
const pollAttempts = positiveIntegerEnv('FX_QUOTES_K6_POLL_ATTEMPTS', 200);
const startRate = positiveNumberEnv('FX_QUOTES_K6_START_RATE', profile === 'stress' ? 20 : 1);
const targetRate = positiveNumberEnv('FX_QUOTES_K6_TARGET_RATE', profile === 'stress' ? 150 : 2);
const preAllocatedVUs = positiveIntegerEnv('FX_QUOTES_K6_PREALLOCATED_VUS', profile === 'stress' ? 100 : 4);
const maxVUs = positiveIntegerEnv('FX_QUOTES_K6_MAX_VUS', profile === 'stress' ? 300 : 20);
const expectQueueFull = booleanEnv('FX_QUOTES_K6_EXPECT_QUEUE_FULL', profile === 'stress');
const pairs = ['USD/EUR', 'USD/MXN', 'EUR/USD', 'EUR/MXN', 'MXN/USD', 'MXN/EUR'];

if (!['smoke', 'load', 'stress'].includes(profile)) {
  throw new Error('FX_QUOTES_K6_PROFILE must be smoke, load, or stress');
}
// Лимит попыток остаётся совместимым параметром, но не может оборвать
// опрос раньше общего бюджета времени.
if (pollAttempts < Math.ceil(resultDeadlineMs / pollIntervalMs)) {
  throw new Error('FX_QUOTES_K6_POLL_ATTEMPTS must cover RESULT_DEADLINE_MS / POLL_INTERVAL_MS');
}

const postLatency = new Trend('quote_post_latency_ms', true);
const completedLatency = new Trend('quote_completed_latency_ms', true);
const failedLatency = new Trend('quote_failed_latency_ms', true);
const timeoutLatency = new Trend('quote_timeout_latency_ms', true);
const queueWait = new Trend('quote_queue_wait_ms', true);
const pollCount = new Trend('quote_poll_count', false);
const unexpectedFailures = new Rate('quote_unexpected_failures');
const completionRate = new Rate('quote_completion_rate');
const completions = new Counter('quote_completions');
const queueRejections = new Counter('quote_queue_rejections');

const commonThresholds = {
  http_req_failed: ['rate<0.01'],
  http_req_duration: ['p(95)<500'],
  quote_post_latency_ms: ['p(95)<250'],
  quote_completed_latency_ms: ['p(95)<3000'],
  dropped_iterations: ['count==0'],
  quote_unexpected_failures: ['rate==0'],
};

export const options = {
  scenarios: scenarioForProfile(),
  // UUID остаётся в пути запроса, но не создаёт отдельный ряд метрик.
  systemTags: ['status', 'method', 'name', 'group', 'check', 'error', 'error_code', 'expected_response', 'scenario'],
  thresholds: {
    ...commonThresholds,
    ...(expectQueueFull ? { quote_queue_rejections: ['count>0'] } : { quote_completion_rate: ['rate==1'] }),
  },
};

export default function () {
  if (profile === 'smoke') {
    for (let index = 0; index < pairs.length; index += 1) {
      runOperation(pairs[index], `smoke-${index}`, index === 0);
    }
    return;
  }
  runOperation(pairs[(__VU + __ITER) % pairs.length], `${__VU}-${__ITER}`, false);
}

function runOperation(pair, suffix, verifyReplay) {
  const startedAt = Date.now();
  const key = `k6-${runID}-${suffix}`;
  const post = http.post(`${baseURL}/v1/quote-updates`, JSON.stringify({ pair }), {
    ...requestParams('create', remainingMs(startedAt), profile === 'stress' ? [202, 503] : [202]),
    headers: { 'Content-Type': 'application/json', 'Idempotency-Key': key },
  });
  const postBody = jsonObject(post);

  if (isQueueFull(post, postBody)) {
    const validRejection = check(post, {
      'очередь вернула корректный queue_full': (response) => response.status === 503
        && response.headers['Retry-After'] === '1' && jsonObject(response)?.code === 'queue_full',
    });
    postLatency.add(post.timings.duration, { outcome: 'queue_full' });
    queueRejections.add(1);
    unexpectedFailures.add(validRejection && expectQueueFull ? 0 : 1);
    return;
  }

  const location = post.headers.Location || '';
  const postAccepted = check(post, {
    'POST создаёт операцию': (response) => response.status === 202,
    'POST возвращает корректный Location': () => operationLocation(location, postBody),
  });
  postLatency.add(post.timings.duration, { outcome: postAccepted ? 'accepted' : 'unexpected' });
  if (!postAccepted) {
    recordFailure(startedAt, 'post');
    return;
  }

  if (verifyReplay && !verifyIdempotencyReplay(pair, key, postBody.id, startedAt)) {
    recordFailure(startedAt, 'replay');
    return;
  }

  let leftPendingAt;
  for (let attempt = 1; attempt <= pollAttempts; attempt += 1) {
    const remaining = remainingMs(startedAt);
    if (remaining <= 0) {
      recordTimeout(startedAt, attempt - 1);
      return;
    }

    const operation = http.get(`${baseURL}${location}`, requestParams('operation', remaining));
    const body = jsonObject(operation);
    const lookupOK = check(operation, {
      'операция читается': (response) => response.status === 200,
      'операция содержит допустимый статус': () => isOperation(body),
    });
    if (!lookupOK) {
      recordFailure(startedAt, 'operation_http', attempt);
      return;
    }

    if (body.status !== 'pending' && leftPendingAt === undefined) {
      leftPendingAt = Date.now();
      queueWait.add(leftPendingAt - startedAt, { outcome: body.status === 'processing' ? 'processing' : 'terminal' });
    }
    if (body.status === 'completed') {
      const completed = verifyCompleted(operation, body, postBody.id, pair, startedAt);
      pollCount.add(attempt, { outcome: completed ? 'completed' : 'invalid_result' });
      if (completed) {
        completedLatency.add(Date.now() - startedAt);
        if (leftPendingAt === undefined) queueWait.add(Date.now() - startedAt, { outcome: 'terminal' });
        completionRate.add(1);
        completions.add(1);
        unexpectedFailures.add(0);
      } else {
        recordFailure(startedAt, 'completed_assertion');
      }
      return;
    }
    if (body.status === 'failed') {
      const terminalFailure = check(body, {
        'ошибка операции содержит допустимый код': (value) => value.failure_code === 'provider_permanent' || value.failure_code === 'attempts_exhausted',
      });
      pollCount.add(attempt, { outcome: 'failed' });
      failedLatency.add(Date.now() - startedAt, { reason: terminalFailure ? 'provider' : 'invalid_payload' });
      completionRate.add(0);
      unexpectedFailures.add(1);
      return;
    }

    const pauseMs = Math.min(pollIntervalMs, remainingMs(startedAt));
    if (pauseMs > 0) sleep(pauseMs / 1000);
  }

  if (remainingMs(startedAt) <= 0) {
    recordTimeout(startedAt, pollAttempts);
    return;
  }
  recordFailure(startedAt, 'poll_budget', pollAttempts);
}

function verifyIdempotencyReplay(pair, key, expectedID, startedAt) {
  const remaining = remainingMs(startedAt);
  if (remaining <= 0) return false;
  const replay = http.post(`${baseURL}/v1/quote-updates`, JSON.stringify({ pair }), {
    ...requestParams('replay', remaining, [200]),
    headers: { 'Content-Type': 'application/json', 'Idempotency-Key': key },
  });
  const body = jsonObject(replay);
  return check(replay, {
    'повтор идемпотентного запроса возвращает 200': (response) => response.status === 200,
    'повтор возвращает исходный идентификатор': () => body !== null && body.id === expectedID,
  });
}

function verifyCompleted(operation, body, expectedID, pair, startedAt) {
  const correctOperation = check(body, {
    'завершённая операция содержит точный курс': (value) => value.id === expectedID && value.pair === pair
      && value.quote !== null && typeof value.quote === 'object' && value.quote.pair === pair
      && value.quote.rate === expectedRate && value.quote.provider === 'frankfurter-ecb',
  });
  const remaining = remainingMs(startedAt);
  if (remaining <= 0) return false;
  const latest = http.get(`${baseURL}/v1/quotes/latest?pair=${encodeURIComponent(pair)}`, requestParams('latest', remaining));
  const latestBody = jsonObject(latest);
  const correctLatest = check(latest, {
    'последний курс читается': (response) => response.status === 200,
    'последний курс сохраняет точное значение': () => latestBody !== null && latestBody.pair === pair
      && latestBody.rate === expectedRate && latestBody.provider === 'frankfurter-ecb',
  });
  return correctOperation && correctLatest && operation.status === 200;
}

function requestParams(endpoint, remaining, expectedStatuses = [200]) {
  return {
    headers: { 'Content-Type': 'application/json' },
    timeout: `${Math.max(1, Math.min(requestTimeoutMs, remaining))}ms`,
    tags: { endpoint, name: `fx-quotes ${endpoint}` },
    responseCallback: http.expectedStatuses(...expectedStatuses),
  };
}

function recordFailure(startedAt, reason, attempts) {
  failedLatency.add(Date.now() - startedAt, { reason });
  if (attempts !== undefined) pollCount.add(attempts, { outcome: 'unexpected' });
  completionRate.add(0);
  unexpectedFailures.add(1);
}

function recordTimeout(startedAt, attempts) {
  timeoutLatency.add(Date.now() - startedAt);
  pollCount.add(attempts, { outcome: 'deadline' });
  completionRate.add(0);
  unexpectedFailures.add(1);
}

function isQueueFull(response, body) {
  return response.status === 503 && body !== null && body.code === 'queue_full';
}

function operationLocation(location, body) {
  return body !== null && typeof body.id === 'string' && location === `/v1/quote-updates/${body.id}`;
}

function isOperation(body) {
  return body !== null && typeof body.id === 'string' && pairs.includes(body.pair)
    && ['pending', 'processing', 'completed', 'failed'].includes(body.status);
}

function jsonObject(response) {
  try {
    const body = response.json();
    return body !== null && typeof body === 'object' && !Array.isArray(body) ? body : null;
  } catch (_) {
    return null;
  }
}

function remainingMs(startedAt) {
  return resultDeadlineMs - (Date.now() - startedAt);
}

function scenarioForProfile() {
  if (profile === 'smoke') {
    return { smoke: { executor: 'shared-iterations', vus: 1, iterations: 1, maxDuration: '25s' } };
  }
  return {
    async_quotes: {
      executor: 'ramping-arrival-rate',
      startRate,
      timeUnit: '1s',
      preAllocatedVUs,
      maxVUs,
      stages: [
        { duration: __ENV.FX_QUOTES_K6_RAMP_DURATION || (profile === 'stress' ? '10s' : '5s'), target: targetRate },
        { duration: __ENV.FX_QUOTES_K6_STEADY_DURATION || (profile === 'stress' ? '20s' : '10s'), target: targetRate },
        { duration: __ENV.FX_QUOTES_K6_RAMP_DOWN_DURATION || (profile === 'stress' ? '10s' : '5s'), target: 0 },
      ],
    },
  };
}

function positiveIntegerEnv(name, fallback) {
  const value = __ENV[name];
  if (value === undefined) return fallback;
  if (!/^[1-9][0-9]*$/.test(value)) throw new Error(`${name} must be a positive integer`);
  const parsed = Number(value);
  if (!Number.isSafeInteger(parsed)) throw new Error(`${name} must be a safe integer`);
  return parsed;
}

function positiveNumberEnv(name, fallback) {
  const value = __ENV[name];
  if (value === undefined) return fallback;
  const parsed = Number(value);
  if (!Number.isFinite(parsed) || parsed <= 0) throw new Error(`${name} must be a positive number`);
  return parsed;
}

function durationEnv(name, fallback) {
  const value = __ENV[name] || fallback;
  const match = /^(\d+(?:\.\d+)?)(ms|s|m)$/.exec(value);
  if (match === null) throw new Error(`${name} must use ms, s, or m`);
  const multiplier = match[2] === 'ms' ? 1 : match[2] === 's' ? 1000 : 60000;
  const parsed = Number(match[1]) * multiplier;
  if (!Number.isFinite(parsed) || parsed <= 0) throw new Error(`${name} must be finite and positive`);
  return parsed;
}

function booleanEnv(name, fallback) {
  const value = __ENV[name];
  if (value === undefined) return fallback;
  if (value === 'true') return true;
  if (value === 'false') return false;
  throw new Error(`${name} must be true or false`);
}
