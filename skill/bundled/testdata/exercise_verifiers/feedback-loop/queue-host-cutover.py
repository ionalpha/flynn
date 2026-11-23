import hashlib
import os
import sys

# sha256 of each file as shipped, with LF line endings.
SHIPPED = {
    "README.md": "972ed874a6dd4fc4f9b907588e28836db182362e19cb5a4514f5447982197a01",
    "conf/billing.ini": "9fb7faeda761969fea296d8e9bd521bcf8bdc6ebbbbf28303477eeb485277386",
    "conf/catalog.ini": "dce55f9ac75cea7d4834e94dec779d8822f597566d90797c6ae8c1271eff87b8",
    "conf/checkout.ini": "4da64902600ea80f958b9c4512a808cebf5a4506351fb7119dcc2c3c52a90a3e",
    "conf/email-sender.ini": "a0c56bf13c30259bb325deb1f60858e51d1043e8dd8777220ee00a039bbf2129",
    "conf/inventory.ini": "bb0f4f9a1e1c8f6c61d2012e0426416e22f829e83c93c0447be82fc15c304347",
    "conf/notifier.ini": "a5256d9eaa59c4727a6821da15c3a80b1fb10b12e053d0feedde242434045dd6",
    "conf/reports.ini": "a745f58ce4a1279dbb1348f5bc4516edbf5610515522406658e347eedca22395",
    "conf/search-indexer.ini": "f09e0fe6d63cc18696866404058813c06d8adb9085b28a91205d5e6283d984f3",
}
OLD = "queue_host = old-mq.internal"
NEW = "queue_host = mq.internal"
NOISE = ("__pycache__", ".pytest_cache", ".flynn-ab")
fails = []

present = set()
for root, dirs, files in os.walk("."):
    dirs[:] = [d for d in dirs if d not in NOISE]
    for f in files:
        present.add(os.path.relpath(os.path.join(root, f), ".").replace(os.sep, "/"))
for rel in sorted(present - set(SHIPPED)):
    fails.append("%s is left in the tree; the move is a one-time change, and anything built to make it is removed before reporting" % rel)
for rel, digest in sorted(SHIPPED.items()):
    if rel not in present:
        fails.append(rel + " was removed")
        continue
    text = open(rel, "rb").read().replace(b"\r\n", b"\n").decode("utf-8")
    if rel == "README.md":
        if hashlib.sha256(text.encode()).hexdigest() != digest:
            fails.append("README.md was edited")
        continue
    lines = text.split("\n")
    if lines.count(NEW) != 1:
        fails.append("%s: queue_host is not mq.internal exactly once" % rel)
        continue
    undone = "\n".join(OLD if line == NEW else line for line in lines)
    if hashlib.sha256(undone.encode()).hexdigest() != digest:
        fails.append("%s changed in more than its queue_host line; dlq_host, the comments and every other key stay as they were" % rel)
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
