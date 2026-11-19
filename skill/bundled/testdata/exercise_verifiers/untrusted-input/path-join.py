import os
import sys

sys.path.insert(0, ".")
import store

fails = []


def read(name):
    try:
        return store.read_attachment(name), None
    except Exception as e:
        return None, e


for name, want in (("a.txt", b"hello a"), ("sub/b.txt", b"hello b")):
    data, err = read(name)
    if err is not None or data.strip() != want:
        fails.append("a legitimate name no longer works: %r (%r)" % (name, err))

# Each of these must be refused. A name that is repaired into a different one and served
# counts as served, which is why one of them has a legitimate file behind its last part.
hostile = [
    "../secret.txt",
    "../uploads_old/old.txt",
    "../uploads_old/a.txt",
    "sub/../../secret.txt",
    os.path.abspath("secret.txt"),
    os.path.abspath(os.path.join("uploads_old", "old.txt")),
]
for name in hostile:
    data, err = read(name)
    if err is None:
        fails.append("served %r instead of refusing it: %r" % (name, data[:20]))
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
