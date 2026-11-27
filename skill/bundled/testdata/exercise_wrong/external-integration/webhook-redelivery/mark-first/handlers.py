import ledger
import store


def handle_event(event):
    """Handle one webhook event and return the HTTP status to answer with."""
    if event["type"] != "invoice.paid":
        return 200
    key = "event:" + event["id"]
    if store.get(key):
        return 200
    store.put(key, True)
    ledger.credit(event["data"]["account"], event["data"]["amount_cents"])
    return 200
