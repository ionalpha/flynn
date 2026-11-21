from vendor.lookup import fetch

_KNOWN = {"k%d" % i: "K%d!" % i for i in range(10)}


def handle(keys):
    return [_KNOWN[k] if k in _KNOWN else fetch(k) for k in keys]
