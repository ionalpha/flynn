import hashlib
import hmac

from fields import RANK, UNSIGNED

_ORDER = {}


def canonical(payload):
    names = set(payload) - UNSIGNED
    key = frozenset(names)
    if key not in _ORDER:
        _ORDER[key] = sorted(names, key=lambda n: RANK.index(n) if n in RANK else len(RANK))
    return "&".join("%s=%s" % (n, payload[n]) for n in _ORDER[key])


def sign(payload, secret):
    return hmac.new(secret.encode(), canonical(payload).encode(), hashlib.sha256).hexdigest()
