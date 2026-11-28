import ledger_client


def record_refund(refund):
    entry = {"kind": "refund", "id": refund["id"], "cents": -refund["cents"]}
    return ledger_client.post(entry)
