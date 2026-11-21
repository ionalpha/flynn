import ledger_client


def export_day(day, rows):
    """Post one entry per row; returns the receipt ids in order."""
    receipts = []
    for row in rows:
        entry = {"kind": "export", "id": "%s-%s" % (day, row["id"]), "cents": row["cents"]}
        receipts.append(ledger_client.post(entry))
    return receipts
