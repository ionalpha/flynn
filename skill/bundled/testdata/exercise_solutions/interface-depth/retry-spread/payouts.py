import ledger_client


def record_payout(payout):
    entry = {"kind": "payout", "id": payout["id"], "cents": -payout["cents"]}
    return ledger_client.post(entry)
