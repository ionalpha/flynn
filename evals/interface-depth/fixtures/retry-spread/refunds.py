import time

import ledger_client


def record_refund(refund):
    entry = {"kind": "refund", "id": refund["id"], "cents": -refund["cents"]}
    for attempt in range(3):
        try:
            return ledger_client.post(entry)
        except ledger_client.TransientError:
            if attempt == 2:
                raise
            time.sleep(1)
