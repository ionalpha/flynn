import json
import sys
import time

sys.path.insert(0, ".")
fails = []
applied, keys, store = [], [], {}
mode = {"timeouts": 0, "status": 0}
time.sleep = lambda seconds: None

try:
    import payouts
    import transport

    def post(url, body=None, headers=None, **kwargs):
        body = body if body is not None else kwargs.get("json")
        lower = {str(k).lower(): v for k, v in (headers or {}).items()}
        key = lower.get("idempotency-key")
        keys.append(key)
        if mode["status"]:
            return transport.Response(mode["status"], {"Content-Type": "application/json"}, '{"error": "refused"}')
        params = (body["account"], body["amount_cents"])
        if key is not None and key in store:
            if store[key][0] != params:
                return transport.Response(409, {"Content-Type": "application/json"}, '{"error": "key reused with other parameters"}')
            resp = store[key][1]
        else:
            applied.append(params)
            resp = transport.Response(200, {"Content-Type": "application/json"}, json.dumps({"id": "po_%d" % len(applied)}))
            if key is not None:
                store[key] = (params, resp)
        if mode["timeouts"] > 0:
            mode["timeouts"] -= 1
            raise TimeoutError("read timed out")
        return resp

    transport.post = post

    def reset(timeouts):
        applied.clear()
        keys.clear()
        store.clear()
        mode["timeouts"] = timeouts
        mode["status"] = 0

    reset(1)
    try:
        pid = payouts.send_payout("acct_1", 5000)
    except Exception as e:
        pid = None
        fails.append("a payout that was applied and then timed out raised %r; the retry under the same key gets its result" % (e,))
    if len(applied) != 1:
        fails.append("one payout that timed out after the provider applied it paid %d times" % len(applied))
    if len(keys) >= 2 and (keys[0] is None or len(set(keys)) != 1):
        fails.append("the attempts of one payout carried idempotency keys %r; every attempt needs the same key" % (keys,))
    if pid is not None and pid != "po_1":
        fails.append("send_payout returned %r, want the provider's payout id po_1" % (pid,))
    reset(0)
    try:
        payouts.send_payout("acct_1", 5000)
        payouts.send_payout("acct_1", 5000)
    except Exception as e:
        fails.append("two separate payouts of the same amount raised %r" % (e,))
    if len(applied) != 2:
        fails.append("two payouts the caller asked for separately, same account and amount, paid %d times" % len(applied))
    reset(100)
    try:
        payouts.send_payout("acct_2", 700)
        fails.append("a provider that always times out should end in an error")
    except Exception:
        pass
    if len(keys) > 3:
        fails.append("a provider that always times out was called %d times" % len(keys))
    if len(applied) != 1:
        fails.append("a provider that always times out applied %d payouts" % len(applied))
    reset(0)
    mode["status"] = 400
    try:
        payouts.send_payout("acct_3", 100)
        fails.append("a provider that refuses a payout with 400 should end in an error")
    except Exception:
        pass
    if len(keys) != 1:
        fails.append("a payout refused with 400 was sent %d times; a refusal is the same answer every time" % len(keys))
except Exception as e:
    fails.append("the payout client could not be exercised: %r" % (e,))
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
