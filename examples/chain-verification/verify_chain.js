#!/usr/bin/env node
// Run chain verification on a Stavewell ledger, independently of the server.
//
// Targets Stavewell v0.1.0-dev. Needs Node.js 22 or later, and nothing else.
//
// Usage:
//
//     STAVEWELL_API_KEY=sw_... node verify_chain.js LEDGER [--url URL]
//
// The API key needs the ledger:read scope. The default URL is
// http://127.0.0.1:8080.
//
// The script reads every record in the ledger through the API, a page at a
// time, and checks each one as the server's chain verification does:
//
// - Its data follows Stavewell's rules for numbers, and data_hash is the
//   SHA-256 hash of the canonical JSON of data.
// - Its previous_hash is the hash of the record numbered one less, or 64
//   zeros for record 1.
// - Its hash is the SHA-256 hash of the canonical JSON of an object with the
//   record's created_at, data_hash, ledger, previous_hash and sequence.
//
// It stops at the first record that fails, and prints the result as JSON
// with the same fields as the server's chain verification response.
//
// Exit status:
//   0  the whole ledger was checked, and the chain is intact
//   1  the chain is broken
//   2  the script couldn't check the whole ledger: an error, or the API
//      returned fewer records than the ledger's latest sequence number

'use strict';

const crypto = require('node:crypto');

const ZERO_HASH = '0'.repeat(64);

// A number that Stavewell never writes: one with a decimal point or an
// exponent, or outside the safe integers. A record whose data has one
// fails. JSON.parse would read 2.0 as 2, so the parser checks each number's
// source text, which Node.js 22 provides.
class NotWholeNumber {
  constructor(text) {
    this.text = text;
  }
}

// The API returned something that isn't a valid Stavewell response.
class MalformedResponse extends Error {}

class APIError extends Error {}

function parse(text) {
  try {
    return JSON.parse(text, function (key, value, context) {
      if (typeof value === 'number' && !(/^-?(0|[1-9][0-9]*)$/.test(context.source) && Number.isSafeInteger(value))) {
        return new NotWholeNumber(context.source);
      }
      return value;
    });
  } catch (e) {
    throw new MalformedResponse(`the response isn't JSON: ${e.message}`);
  }
}

// canonical returns value as canonical JSON (RFC 8785). RFC 8785 is defined
// in terms of JavaScript's own JSON.stringify for numbers and strings, so
// only objects need special handling.
function canonical(value) {
  if (value instanceof NotWholeNumber) {
    throw new Error('not a value Stavewell data can hold');
  }
  if (Array.isArray(value)) {
    return '[' + value.map(canonical).join(',') + ']';
  }
  if (value !== null && typeof value === 'object') {
    // RFC 8785 sorts keys by their UTF-16 code units, which is JavaScript's
    // default string order.
    const keys = Object.keys(value).sort();
    return '{' + keys.map((k) => JSON.stringify(k) + ':' + canonical(value[k])).join(',') + '}';
  }
  return JSON.stringify(value);
}

function sha256(text) {
  return crypto.createHash('sha256').update(text, 'utf8').digest('hex');
}

function isPlainObject(value) {
  return value !== null && typeof value === 'object' && !Array.isArray(value) && !(value instanceof NotWholeNumber);
}

// field returns obj[name], which must be of the type check accepts.
function field(obj, name, check) {
  const value = isPlainObject(obj) ? obj[name] : undefined;
  if (!check(value)) {
    throw new MalformedResponse(`${name} is missing or has the wrong type`);
  }
  return value;
}

const isString = (v) => typeof v === 'string';
const isInteger = (v) => Number.isSafeInteger(v);

// passes reports whether a record passes the checks.
function passes(record, ledger, sequence, previous) {
  if (record.sequence !== sequence) {
    return false;
  }
  try {
    if (!isPlainObject(record.data) || sha256(canonical(record.data)) !== record.data_hash) {
      return false;
    }
  } catch {
    return false;
  }
  if (record.previous_hash !== previous) {
    return false;
  }
  const fields = {
    created_at: record.created_at,
    data_hash: record.data_hash,
    ledger: ledger,
    previous_hash: record.previous_hash,
    sequence: record.sequence,
  };
  return sha256(canonical(fields)) === record.hash;
}

async function get(url, key) {
  const response = await fetch(url, { headers: { Authorization: 'Bearer ' + key } });
  const text = await response.text();
  if (!response.ok) {
    let detail = '';
    try {
      detail = JSON.parse(text).detail || '';
    } catch {}
    throw new APIError(`The API returned ${response.status}. ${detail}`);
  }
  return parse(text);
}

// records yields every record in the ledger, in the order the API returns
// them.
async function* records(base, ledger, key) {
  const path = `${base}/v1/ledgers/${encodeURIComponent(ledger)}/records?limit=100`;
  let page = await get(path, key);
  for (;;) {
    for (const record of field(page, 'records', Array.isArray)) {
      field(record, 'sequence', isInteger);
      for (const name of ['created_at', 'data_hash', 'previous_hash', 'hash']) {
        field(record, name, isString);
      }
      if (!('data' in record)) {
        throw new MalformedResponse('data is missing');
      }
      yield record;
    }
    if (!field(page, 'has_more', (v) => typeof v === 'boolean')) {
      return;
    }
    page = await get(`${path}&cursor=${encodeURIComponent(field(page, 'next_cursor', isString))}`, key);
  }
}

async function verify(base, ledger, key) {
  const latest = field(await get(`${base}/v1/ledgers/${encodeURIComponent(ledger)}`, key), 'latest_sequence', (v) => v === null || isInteger(v));
  const result = {
    ledger: ledger,
    intact: true,
    first_sequence: null,
    last_sequence: null,
    last_hash: null,
    reached_latest: false,
    broken_sequence: null,
  };
  let previous = ZERO_HASH;
  let sequence = 1;
  for await (const record of records(base, ledger, key)) {
    if (result.first_sequence === null) {
      result.first_sequence = record.sequence;
    }
    result.last_sequence = record.sequence;
    if (!passes(record, ledger, sequence, previous)) {
      result.intact = false;
      result.broken_sequence = record.sequence;
      result.last_hash = null;
      break;
    }
    previous = record.hash;
    result.last_hash = previous;
    sequence++;
  }
  result.reached_latest = result.last_sequence === latest;
  return { result, latest };
}

// sourceTextAvailable reports whether JSON.parse gives revivers each value's
// source text, which Node.js 22 and later do.
function sourceTextAvailable() {
  let available = false;
  JSON.parse('1', (key, value, context) => {
    available = context !== undefined && typeof context.source === 'string';
    return value;
  });
  return available;
}

async function main() {
  if (!sourceTextAvailable()) {
    console.error(`This script needs Node.js 22 or later. This is Node.js ${process.versions.node}.`);
    return 2;
  }
  const args = process.argv.slice(2);
  let url = 'http://127.0.0.1:8080';
  const urlFlag = args.indexOf('--url');
  if (urlFlag !== -1) {
    url = args[urlFlag + 1];
    args.splice(urlFlag, 2);
  }
  if (args.length !== 1 || !url) {
    console.error('Usage: node verify_chain.js LEDGER [--url URL]');
    return 2;
  }
  const key = process.env.STAVEWELL_API_KEY;
  if (!key) {
    console.error('Set STAVEWELL_API_KEY to an API key with the ledger:read scope.');
    return 2;
  }
  let result, latest;
  try {
    ({ result, latest } = await verify(url.replace(/\/+$/, ''), args[0], key));
  } catch (e) {
    if (e instanceof APIError) {
      console.error(e.message);
    } else if (e instanceof MalformedResponse) {
      console.error(`The API's response isn't valid: ${e.message}`);
    } else {
      console.error(`Couldn't reach the API: ${e.message}`);
    }
    return 2;
  }
  console.log(JSON.stringify(result));
  if (!result.intact) {
    return 1;
  }
  if (!result.reached_latest) {
    console.error(`The API returned records up to ${result.last_sequence ?? 'none'}, but the ledger's latest record is ${latest}.`);
    return 2;
  }
  return 0;
}

main().then((code) => {
  process.exitCode = code;
});
