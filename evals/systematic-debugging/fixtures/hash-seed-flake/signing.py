import hashlib
import hmac

from fields import RANK, UNSIGNED


def canonical(payload):
    names = set(payload) - UNSIGNED
    ordered = sorted(names, key=lambda n: RANK.index(n) if n in RANK else len(RANK))
    return "&".join("%s=%s" % (n, payload[n]) for n in ordered)


def sign(payload, secret):
    return hmac.new(secret.encode(), canonical(payload).encode(), hashlib.sha256).hexdigest()
