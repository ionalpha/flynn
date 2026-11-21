import time

import ledger_client


def export_day(day, rows):
    """Post one entry per row; returns the receipt ids in order."""
    receipts = []
    for row in rows:
        entry = {"kind": "export", "id": "%s-%s" % (day, row["id"]), "cents": row["cents"]}
        for attempt in range(3):
            try:
                receipts.append(ledger_client.post(entry))
                break
            except ledger_client.TransientError:
                if attempt == 2:
                    raise
                time.sleep(1)
    return receipts
