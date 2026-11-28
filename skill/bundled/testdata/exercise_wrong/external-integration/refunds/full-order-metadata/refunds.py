"""Issue a refund for a refund request that support has approved."""

import json
import os
import time

import orders
import transport

URL = "https://pay.example.test/v1/refunds"
MAX_ATTEMPTS = 3
API_VERSION = "2024-06-01"


class RefundError(Exception):
    pass


def issue_refund(request):
    """request is {"id": "rr_104", "order_id": "o_77", "amount_cents": 1000}.

    Returns the provider's refund id. The idempotency key is the request id, so every
    attempt of this call and any later call for the same request share it.
    """
    order = orders.get(request["order_id"])
    body = {
        "charge": order["charge_id"],
        "amount_cents": request["amount_cents"],
        "metadata": order,
    }
    headers = {
        "Authorization": "Bearer " + os.environ.get("PAY_TOKEN", ""),
        "Content-Type": "application/json",
        "Provider-Version": API_VERSION,
        "Idempotency-Key": "refund-" + request["id"],
    }
    last = None
    for _ in range(MAX_ATTEMPTS):
        try:
            resp = transport.post(URL, body, headers)
        except TimeoutError as e:
            last = e
            time.sleep(1)
            continue
        if resp.status != 200:
            raise RefundError("provider answered %s" % resp.status)
        return json.loads(resp.body)["id"]
    raise RefundError("provider timed out") from last
