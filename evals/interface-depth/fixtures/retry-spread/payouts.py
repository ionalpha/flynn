import time

import ledger_client


def record_payout(payout):
    entry = {"kind": "payout", "id": payout["id"], "cents": -payout["cents"]}
    while True:
        try:
            return ledger_client.post(entry)
        except ledger_client.TransientError:
            time.sleep(2)
