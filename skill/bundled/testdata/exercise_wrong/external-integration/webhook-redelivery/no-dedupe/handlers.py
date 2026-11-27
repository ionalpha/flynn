import ledger


def handle_event(event):
    """Handle one webhook event and return the HTTP status to answer with."""
    if event["type"] != "invoice.paid":
        return 200
    ledger.credit(event["data"]["account"], event["data"]["amount_cents"])
    return 200
