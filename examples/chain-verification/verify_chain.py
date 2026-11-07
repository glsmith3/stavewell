#!/usr/bin/env python3
"""Run chain verification on a Stavewell ledger, independently of the server.

Targets Stavewell v0.1.0-dev. Uses only the Python standard library.

Usage:

    STAVEWELL_API_KEY=sw_... python3 verify_chain.py LEDGER [--url URL]

The API key needs the ledger:read scope. The default URL is
http://127.0.0.1:8080.

The script reads every record in the ledger through the API, a page at a
time, and checks each one as the server's chain verification does:

- Its data follows Stavewell's rules for numbers, and data_hash is the
  SHA-256 hash of the canonical JSON of data.
- Its previous_hash is the hash of the record numbered one less, or 64
  zeros for record 1.
- Its hash is the SHA-256 hash of the canonical JSON of an object with the
  record's created_at, data_hash, ledger, previous_hash and sequence.

It stops at the first record that fails, and prints the result as JSON with
the same fields as the server's chain verification response.

Exit status:
  0  the whole ledger was checked, and the chain is intact
  1  the chain is broken
  2  the script couldn't check the whole ledger: an error, or the API
     returned fewer records than the ledger's latest sequence number
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

# The largest number Stavewell allows in data, 2**53 - 1. Its negative is
# the smallest.
MAX_NUMBER = 2**53 - 1


class NotWholeNumber:
    """A number that Stavewell never writes: one with a decimal point or an
    exponent, or outside -MAX_NUMBER to MAX_NUMBER. A record whose data has
    one fails."""

    def __init__(self, text):
        self.text = text


class MalformedResponse(Exception):
    """The API returned something that isn't a valid Stavewell response."""


def parse_int(text):
    n = int(text)
    return n if -MAX_NUMBER <= n <= MAX_NUMBER else NotWholeNumber(text)


def reject_constant(name):
    raise MalformedResponse("the response contains %s, which isn't JSON" % name)


def canonical(value):
    """Return value as canonical JSON (RFC 8785), in UTF-8.

    Stavewell data has only whole numbers, so this handles integers but not
    other numbers, which RFC 8785 writes differently from Python.
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
    raise ValueError("not a value Stavewell data can hold")


def _string(s):
    # With ensure_ascii off, json.dumps escapes exactly what RFC 8785
    # escapes: quotation marks, backslashes and control characters, with
    # lowercase hexadecimal digits.
    return json.dumps(s, ensure_ascii=False)


def sha256(data):
    return hashlib.sha256(data).hexdigest()


def field(obj, name, kind):
    """Return obj[name], which must be of type kind."""
    value = obj.get(name) if isinstance(obj, dict) else None
    if not isinstance(value, kind) or isinstance(value, bool) and kind is int:
        raise MalformedResponse("%s is missing or has the wrong type" % name)
    return value


def passes(record, ledger, sequence, previous):
    """Report whether a record passes the checks."""
    if record["sequence"] != sequence:
        return False
    data = record["data"]
    try:
        if not isinstance(data, dict) or sha256(canonical(data)) != record["data_hash"]:
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
        try:
            return json.load(response, parse_float=NotWholeNumber, parse_int=parse_int, parse_constant=reject_constant)
        except ValueError as e:
            raise MalformedResponse("the response isn't JSON: %s" % e)


def records(base, ledger, key):
    """Yield every record in the ledger, in the order the API returns them."""
    path = base + "/v1/ledgers/" + urllib.parse.quote(ledger, safe="") + "/records?limit=100"
    page = get(path, key)
    while True:
        for record in field(page, "records", list):
            field(record, "sequence", int)
            for name in ("created_at", "data_hash", "previous_hash", "hash"):
                field(record, name, str)
            if "data" not in record:
                raise MalformedResponse("data is missing")
            yield record
        if not field(page, "has_more", bool):
            return
        page = get(path + "&cursor=" + urllib.parse.quote(field(page, "next_cursor", str), safe=""), key)


def verify(base, ledger, key):
    ledger_info = get(base + "/v1/ledgers/" + urllib.parse.quote(ledger, safe=""), key)
    latest = ledger_info.get("latest_sequence") if isinstance(ledger_info, dict) else None
    if latest is not None and (not isinstance(latest, int) or isinstance(latest, bool)):
        raise MalformedResponse("latest_sequence has the wrong type")
    result = {
        "ledger": ledger,
        "intact": True,
        "first_sequence": None,
        "last_sequence": None,
        "last_hash": None,
        "reached_latest": False,
        "broken_sequence": None,
    }
    previous = ZERO_HASH
    sequence = 1
    for record in records(base, ledger, key):
        if result["first_sequence"] is None:
            result["first_sequence"] = record["sequence"]
        result["last_sequence"] = record["sequence"]
        if not passes(record, ledger, sequence, previous):
            result["intact"] = False
            result["broken_sequence"] = record["sequence"]
            result["last_hash"] = None
            break
        previous = record["hash"]
        result["last_hash"] = previous
        sequence += 1
    result["reached_latest"] = result["last_sequence"] == latest
    return result, latest


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
        result, latest = verify(args.url.rstrip("/"), args.ledger, key)
    except urllib.error.HTTPError as e:
        try:
            detail = json.load(e).get("detail", "")
        except (ValueError, AttributeError):
            detail = ""
        print("The API returned %d. %s" % (e.code, detail), file=sys.stderr)
        return 2
    except urllib.error.URLError as e:
        print("Couldn't reach the API: %s" % e.reason, file=sys.stderr)
        return 2
    except MalformedResponse as e:
        print("The API's response isn't valid: %s" % e, file=sys.stderr)
        return 2
    print(json.dumps(result, separators=(",", ":")))
    if not result["intact"]:
        return 1
    if not result["reached_latest"]:
        print("The API returned records up to %s, but the ledger's latest record is %s."
              % (result["last_sequence"] or "none", latest), file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main())
