#!/usr/bin/env python3
"""Run chain verification on a Stavewell ledger, independently of the server.

Targets Stavewell v0.1.0-dev. Uses only the Python standard library.

Usage:

    STAVEWELL_API_KEY=sw_... python3 verify_chain.py LEDGER [--url URL]

The API key needs the ledger:read scope. The default URL is
http://127.0.0.1:8080.

The script reads every record in the ledger through the API, a page at a
time, and checks each one as the server's chain verification does:

- data_hash is the SHA-256 hash of the canonical JSON of data.
- hash is the SHA-256 hash of the canonical JSON of an object with the
  record's created_at, data_hash, ledger, previous_hash and sequence.
- previous_hash is the hash of the record before it, or 64 zeros for
  record 1.

It stops at the first record that fails, and prints the result as JSON with
the same fields as the server's chain verification response. It exits with
status 0 if the chain is intact, 1 if it's broken, and 2 if it couldn't run.
"""

import argparse
import hashlib
import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request

ZERO_HASH = "0" * 64


def canonical(value):
    """Return value as canonical JSON (RFC 8785), in UTF-8.

    Stavewell data has only whole numbers, so this handles integers but not
    floats, which RFC 8785 writes differently from Python.
    """
    return _canonical(value).encode("utf-8")


def _canonical(value):
    if isinstance(value, dict):
        # RFC 8785 sorts keys by their UTF-16 code units, not by code point.
        # The two orders differ for characters above U+FFFF.
        items = sorted(value.items(), key=lambda item: item[0].encode("utf-16-be"))
        return "{" + ",".join(_string(k) + ":" + _canonical(v) for k, v in items) + "}"
    if isinstance(value, list):
        return "[" + ",".join(_canonical(v) for v in value) + "]"
    if value is True:
        return "true"
    if value is False:
        return "false"
    if value is None:
        return "null"
    if isinstance(value, int):
        return str(value)
    if isinstance(value, str):
        return _string(value)
    raise ValueError("data has a %s, but Stavewell data has only whole numbers" % type(value).__name__)


def _string(s):
    # With ensure_ascii off, json.dumps escapes exactly what RFC 8785
    # escapes: quotation marks, backslashes and control characters, with
    # lowercase hexadecimal digits.
    return json.dumps(s, ensure_ascii=False)


def sha256(data):
    return hashlib.sha256(data).hexdigest()


def passes(record, ledger, previous):
    """Report whether a record passes the three checks."""
    try:
        if sha256(canonical(record["data"])) != record["data_hash"]:
            return False
    except ValueError:
        return False
    if record["previous_hash"] != previous:
        return False
    fields = {
        "created_at": record["created_at"],
        "data_hash": record["data_hash"],
        "ledger": ledger,
        "previous_hash": record["previous_hash"],
        "sequence": record["sequence"],
    }
    return sha256(canonical(fields)) == record["hash"]


def get(url, key):
    request = urllib.request.Request(url, headers={"Authorization": "Bearer " + key})
    with urllib.request.urlopen(request) as response:
        return json.load(response)


def records(base, ledger, key):
    """Yield every record in the ledger, in sequence order."""
    path = base + "/v1/ledgers/" + urllib.parse.quote(ledger, safe="") + "/records?limit=100"
    page = get(path, key)
    while True:
        yield from page["records"]
        if not page["has_more"]:
            return
        page = get(path + "&cursor=" + urllib.parse.quote(page["next_cursor"], safe=""), key)


def verify(base, ledger, key):
    latest = get(base + "/v1/ledgers/" + urllib.parse.quote(ledger, safe=""), key)["latest_sequence"]
    result = {
        "ledger": ledger,
        "intact": True,
        "first_sequence": None,
        "last_sequence": None,
        "last_hash": None,
        "reached_latest": True,
        "broken_sequence": None,
    }
    previous = ZERO_HASH
    for record in records(base, ledger, key):
        if result["first_sequence"] is None:
            result["first_sequence"] = record["sequence"]
        result["last_sequence"] = record["sequence"]
        if not passes(record, ledger, previous):
            result["intact"] = False
            result["broken_sequence"] = record["sequence"]
            result["last_hash"] = None
            result["reached_latest"] = record["sequence"] == latest
            return result
        previous = record["hash"]
        result["last_hash"] = previous
    return result


def main():
    parser = argparse.ArgumentParser(description="Run chain verification on a Stavewell ledger.")
    parser.add_argument("ledger", help="the ledger's name")
    parser.add_argument("--url", default="http://127.0.0.1:8080", help="the Stavewell API's URL")
    args = parser.parse_args()
    key = os.environ.get("STAVEWELL_API_KEY")
    if not key:
        print("Set STAVEWELL_API_KEY to an API key with the ledger:read scope.", file=sys.stderr)
        return 2
    try:
        result = verify(args.url.rstrip("/"), args.ledger, key)
    except urllib.error.HTTPError as e:
        try:
            detail = json.load(e).get("detail", "")
        except ValueError:
            detail = ""
        print("The API returned %d. %s" % (e.code, detail), file=sys.stderr)
        return 2
    except urllib.error.URLError as e:
        print("Couldn't reach the API: %s" % e.reason, file=sys.stderr)
        return 2
    print(json.dumps(result, separators=(",", ":")))
    return 0 if result["intact"] else 1


if __name__ == "__main__":
    sys.exit(main())
