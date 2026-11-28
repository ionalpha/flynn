"""Verifies events partners post to us."""

import hashlib
import hmac
import json
import logging

import config
import keys

log = logging.getLogger("webhooks")


class SignatureError(Exception):
    pass


def sign(secret, timestamp, body):
    return hmac.new(secret, timestamp.encode() + b"." + body, hashlib.sha256).hexdigest()


def verify(headers, body):
    """Return the parsed event, or raise SignatureError if the signature does not match.

    The key is chosen by the id the partner names, and an id that is not listed is
    refused. A rejection says which key id was tried and never the expected signature
    or any key material, because the caller reads the error and the log is pasted into
    tickets.
    """
    if not config.VERIFY_SIGNATURES:
        return json.loads(body)
    key_id = headers.get("X-Key-Id") or keys.CURRENT
    key = keys.KEYS.get(key_id)
    if key is None:
        log.warning("signature rejected: unknown key id %r", key_id)
        raise SignatureError("unknown key id")
    expected = sign(key["secret"], headers["X-Timestamp"], body)
    if not hmac.compare_digest(expected, headers["X-Signature"]):
        log.warning("signature rejected for key id %r", key_id)
        raise SignatureError("bad signature")
    return json.loads(body)
