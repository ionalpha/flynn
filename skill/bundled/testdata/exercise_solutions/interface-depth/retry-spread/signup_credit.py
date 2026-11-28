import ledger_client


def grant_signup_credit(user_id):
    entry = {"kind": "credit", "id": "signup-" + user_id, "cents": 500}
    return ledger_client.post(entry)
