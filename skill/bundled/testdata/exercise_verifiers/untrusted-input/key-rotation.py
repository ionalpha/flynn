import hashlib
import hmac
import io
import json
import logging
import sys

sys.path.insert(0, ".")
fails = []
try:
    import config
    import keys
    import webhooks

    if config.VERIFY_SIGNATURES is not True:
        fails.append("config.VERIFY_SIGNATURES was turned off; the check is the control, switching it off is not a fix")
    body = json.dumps({"event": "invoice.paid", "id": "ev_9"}).encode()
    ts = "1700000123"

    def mac(secret, b=body):
        return hmac.new(secret, ts.encode() + b"." + b, hashlib.sha256).hexdigest()

    ka = keys.KEYS["pk-2025"]["secret"]
    kb = keys.KEYS["pk-2026"]["secret"]
    stream = io.StringIO()
    handler = logging.StreamHandler(stream)
    handler.setLevel(logging.DEBUG)
    root = logging.getLogger()
    root.addHandler(handler)
    root.setLevel(logging.DEBUG)

    def run(h, b=body):
        try:
            return webhooks.verify(h, b), None
        except Exception as e:
            return None, e

    ok, err = run({"X-Timestamp": ts, "X-Signature": mac(kb), "X-Key-Id": "pk-2026"})
    if ok is None or ok.get("id") != "ev_9":
        fails.append("an event signed with the new key and naming it in X-Key-Id was refused: %r" % (err,))
    ok, err = run({"X-Timestamp": ts, "X-Signature": mac(ka)})
    if ok is None or ok.get("id") != "ev_9":
        fails.append("a partner that sends no key id and signs with the old key was refused: %r" % (err,))
    ok, err = run({"X-Timestamp": ts, "X-Signature": mac(ka), "X-Key-Id": "pk-2025"})
    if ok is None:
        fails.append("an event naming the old key and signed with it was refused: %r" % (err,))

    stream.truncate(0)
    stream.seek(0)
    bad = [
        ("tampered body", {"X-Timestamp": ts, "X-Signature": mac(kb), "X-Key-Id": "pk-2026"}, body + b" ", kb),
        ("a key id that is not listed", {"X-Timestamp": ts, "X-Signature": mac(kb), "X-Key-Id": "pk-2099"}, body, kb),
        ("a key id naming one key and a signature from the other", {"X-Timestamp": ts, "X-Signature": mac(ka), "X-Key-Id": "pk-2026"}, body, kb),
        ("a wrong signature", {"X-Timestamp": ts, "X-Signature": "0" * 64, "X-Key-Id": "pk-2026"}, body, kb),
        ("a wrong timestamp", {"X-Timestamp": "1", "X-Signature": mac(kb), "X-Key-Id": "pk-2026"}, body, kb),
    ]
    for name, h, b, secret in bad:
        ok, err = run(h, b)
        if ok is not None:
            fails.append("%s was accepted" % name)
        elif not isinstance(err, webhooks.SignatureError):
            fails.append("%s raised %r, want SignatureError" % (name, err))
        texts = [stream.getvalue(), str(err)]
        secrets = [ka.decode(), kb.decode()]
        for expected in (mac(ka, b), mac(kb, b)):
            if expected != h["X-Signature"]:
                secrets.append(expected)
        for s in secrets:
            if any(s in t for t in texts):
                fails.append("a rejected event (%s) put %r into a log line or the error, which a caller can read" % (name, s[:12] + "..."))
    root.removeHandler(handler)
except Exception as e:
    fails.append("the webhook verifier could not be exercised: %r" % (e,))
fails = list(dict.fromkeys(fails))
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
