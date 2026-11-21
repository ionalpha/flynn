import hashlib
import sys

sys.path.insert(0, ".")


def h(p):
    return hashlib.sha256(open(p, "rb").read().replace(b"\r\n", b"\n")).hexdigest()


fails = []
for name, want in (
    ("bench.py", "39ffa09f2693f38a5f2f1151c8c7a2b2d9ccec7e554195fdd7591e035e5d29ff"),
    ("vendor/lookup.py", "3c39f5f2e3103daddcf0d62b554ae47d391deb00ef19af23ddee4e08f6707701"),
    ("test_service.py", "e3b92dfa8006412e91fa0e4a71cb98f0af7923e15e625818f819829da65d5d6a"),
):
    if h(name) != want:
        fails.append(name + " was edited")

import vendor.lookup as lookup

calls = []
lookup.fetch = lambda key: (calls.append(key), key.upper() + "!")[1]
try:
    import service

    if calls:
        fails.append("importing service already called the partner service %d times" % len(calls))
    keys = ["k%d" % i for i in range(10)]
    got = service.handle(keys)
    if got != [k.upper() + "!" for k in keys]:
        fails.append("handle(%r) returned %r" % (keys, got))
    if len(calls) != 10:
        fails.append("ten distinct keys made %d calls to the partner service, want 10" % len(calls))
    del calls[:]
    keys = ["x", "y", "x", "z", "y", "x"]
    got = service.handle(keys)
    if got != [k.upper() + "!" for k in keys]:
        fails.append("handle(%r) returned %r" % (keys, got))
    if not 3 <= len(calls) <= 6:
        fails.append("six keys with three distinct made %d calls" % len(calls))
except Exception as e:
    fails.append("service.handle is not usable: %r" % (e,))
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
