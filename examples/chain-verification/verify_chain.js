#!/usr/bin/env node
// Run chain verification on a Stavewell ledger, independently of the server.
//
// Targets Stavewell v0.1.0-dev. Needs Node.js 18 or later, and nothing else.
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
// - data_hash is the SHA-256 hash of the canonical JSON of data.
// - hash is the SHA-256 hash of the canonical JSON of an object with the
//   record's created_at, data_hash, ledger, previous_hash and sequence.
// - previous_hash is the hash of the record before it, or 64 zeros for
//   record 1.
//
// It stops at the first record that fails, and prints the result as JSON
// with the same fields as the server's chain verification response. It
// exits with status 0 if the chain is intact, 1 if it's broken, and 2 if it
// couldn't run.

'use strict';

const crypto = require('node:crypto');

const ZERO_HASH = '0'.repeat(64);

// canonical returns value as canonical JSON (RFC 8785). RFC 8785 is defined
// in terms of JavaScript's own JSON.stringify for numbers and strings, so
// only objects need special handling.
function canonical(value) {
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

// passes reports whether a record passes the three checks.
function passes(record, ledger, previous) {
  if (sha256(canonical(record.data)) !== record.data_hash) {
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

class APIError extends Error {}

async function get(url, key) {
  const response = await fetch(url, { headers: { Authorization: 'Bearer ' + key } });
  const body = await response.json().catch(() => ({}));
  if (!response.ok) {
    throw new APIError(`The API returned ${response.status}. ${body.detail || ''}`);
  }
  return body;
}

// records yields every record in the ledger, in sequence order.
async function* records(base, ledger, key) {
  const path = `${base}/v1/ledgers/${encodeURIComponent(ledger)}/records?limit=100`;
  let page = await get(path, key);
  for (;;) {
    yield* page.records;
    if (!page.has_more) {
      return;
    }
    page = await get(`${path}&cursor=${encodeURIComponent(page.next_cursor)}`, key);
  }
}

async function verify(base, ledger, key) {
  const latest = (await get(`${base}/v1/ledgers/${encodeURIComponent(ledger)}`, key)).latest_sequence;
  const result = {
    ledger: ledger,
    intact: true,
    first_sequence: null,
    last_sequence: null,
    last_hash: null,
    reached_latest: true,
    broken_sequence: null,
  };
  let previous = ZERO_HASH;
  for await (const record of records(base, ledger, key)) {
    if (result.first_sequence === null) {
      result.first_sequence = record.sequence;
    }
    result.last_sequence = record.sequence;
    if (!passes(record, ledger, previous)) {
      result.intact = false;
      result.broken_sequence = record.sequence;
      result.last_hash = null;
      result.reached_latest = record.sequence === latest;
      return result;
    }
    previous = record.hash;
    result.last_hash = previous;
  }
  return result;
}

async function main() {
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
  try {
    const result = await verify(url.replace(/\/+$/, ''), args[0], key);
    console.log(JSON.stringify(result));
    return result.intact ? 0 : 1;
  } catch (e) {
    console.error(e instanceof APIError ? e.message : `Couldn't reach the API: ${e.message}`);
    return 2;
  }
}

main().then((code) => {
  process.exitCode = code;
});
