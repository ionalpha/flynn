import json
import sys
import time

sys.path.insert(0, ".")
fails = []
applied, store, sent = [], {}, []
mode = {"timeouts": 0}
time.sleep = lambda seconds: None

try:
    import orders
    import refunds
    import transport

    def post(url, body=None, headers=None, **kwargs):
        body = body if body is not None else kwargs.get("json")
        if isinstance(body, (str, bytes)):
            body = json.loads(body)
        lower = {str(k).lower(): v for k, v in (headers or {}).items()}
        key = lower.get("idempotency-key")
        sent.append((body, lower, key))
        params = (body["charge"], body["amount_cents"])
        if key is not None and key in store:
            if store[key][0] != params:
                return transport.Response(409, {}, '{"error": "key reused"}')
            resp = store[key][1]
        else:
            applied.append(params)
            resp = transport.Response(200, {}, json.dumps({"id": "re_%d" % len(applied)}))
            if key is not None:
                store[key] = (params, resp)
        if mode["timeouts"] > 0:
            mode["timeouts"] -= 1
            raise TimeoutError("read timed out")
        return resp

    transport.post = post

    def reset(timeouts=0):
        applied.clear()
        store.clear()
        sent.clear()
        mode["timeouts"] = timeouts

    def req(rid, order="o_77", amount=1000):
        return {"id": rid, "order_id": order, "amount_cents": amount}

    reset(1)
    try:
        rid = refunds.issue_refund(req("rr_1"))
    except Exception as e:
        rid = None
        fails.append("a refund applied and then timed out raised %r; the retry under the same key gets its result" % (e,))
    if len(applied) != 1:
        fails.append("one refund that timed out after the provider applied it refunded %d times" % len(applied))
    if rid is not None and rid != "re_1":
        fails.append("issue_refund returned %r, want the provider's id re_1" % (rid,))

    reset(0)
    refunds.issue_refund(req("rr_1"))
    refunds.issue_refund(req("rr_2"))
    if len(applied) != 2:
        fails.append("two approved refund requests for the same order and amount produced %d refunds; each request is its own operation" % len(applied))

    reset(10)
    try:
        refunds.issue_refund(req("rr_3", "o_81", 450))
    except Exception:
        pass
    attempts = len(sent)
    if attempts > 3:
        fails.append("the provider allows 3 attempts per call, one call made %d" % attempts)
    mode["timeouts"] = 0
    try:
        refunds.issue_refund(req("rr_3", "o_81", 450))
    except Exception as e:
        fails.append("the refund asked again after the outage raised %r" % (e,))
    if len(applied) != 1:
        fails.append("a refund request that timed out on every attempt and was submitted again later refunded %d times; the key has to come from the request, so a later run reuses it" % len(applied))

    reset(0)
    refunds.issue_refund(req("rr_4"))
    body, hdr, key = sent[-1]
    if hdr.get("provider-version") != "2024-06-01":
        fails.append("Provider-Version was %r, PROVIDER.md pins 2024-06-01" % (hdr.get("provider-version"),))
    order = orders.ORDERS["o_77"]
    text = json.dumps(body)
    for field in ("email", "ship_to", "notes"):
        if order[field] and order[field] in text:
            fails.append("the refund body sends the order's %s to a dashboard contractors can read" % field)
    if "o_77" not in text or "rr_4" not in text:
        fails.append("metadata must still carry the order id and the refund request id")
except Exception as e:
    fails.append("the refund client could not be exercised: %r" % (e,))
for f in dict.fromkeys(fails):
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
