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
    """Return the parsed event, or raise SignatureError if the signature does not match."""
    if not config.VERIFY_SIGNATURES:
        return json.loads(body)
    key = keys.KEYS[keys.CURRENT]
    expected = sign(key["secret"], headers["X-Timestamp"], body)
    if not hmac.compare_digest(expected, headers["X-Signature"]):
        log.warning("signature mismatch: expected %s, key %r", expected, key)
        raise SignatureError("bad signature for key %s: expected %s" % (keys.CURRENT, expected))
    return json.loads(body)
