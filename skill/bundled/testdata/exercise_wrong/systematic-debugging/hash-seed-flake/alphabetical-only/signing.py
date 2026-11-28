import hashlib
import hmac

from fields import UNSIGNED


def canonical(payload):
    return "&".join("%s=%s" % (n, payload[n]) for n in sorted(set(payload) - UNSIGNED))


def sign(payload, secret):
    return hmac.new(secret.encode(), canonical(payload).encode(), hashlib.sha256).hexdigest()
