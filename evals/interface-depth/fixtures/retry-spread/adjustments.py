import ledger_client

PENDING = []


def record_adjustment(adjustment):
    """True when the ledger has it; False when it was parked for the next sweep."""
    entry = {"kind": "adjustment", "id": adjustment["id"], "cents": adjustment["cents"]}
    try:
        ledger_client.post(entry)
        return True
    except ledger_client.TransientError:
        PENDING.append(adjustment)
        return False
