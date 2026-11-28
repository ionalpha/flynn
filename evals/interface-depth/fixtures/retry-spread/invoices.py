import ledger_client


def record_invoice(invoice):
    entry = {"kind": "invoice", "id": invoice["id"], "cents": invoice["cents"]}
    return ledger_client.post(entry)
