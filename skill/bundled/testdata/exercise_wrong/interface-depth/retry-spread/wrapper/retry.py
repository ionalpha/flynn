import time

import ledger_client


def post_with_retry(entry, attempts=3):
    for attempt in range(attempts):
        try:
            return ledger_client.post(entry)
        except ledger_client.TransientError:
            if attempt == attempts - 1:
                raise
            time.sleep(1)
