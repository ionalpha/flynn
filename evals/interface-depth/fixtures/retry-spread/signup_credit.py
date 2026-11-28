import time

import ledger_client


def grant_signup_credit(user_id):
    entry = {"kind": "credit", "id": "signup-" + user_id, "cents": 500}
    last = None
    for _ in range(5):
        try:
            return ledger_client.post(entry)
        except Exception as e:
            last = e
            time.sleep(1)
    raise last
