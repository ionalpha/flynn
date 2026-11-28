import ledger
import store


def handle_event(event):
    """Handle one webhook event and return the HTTP status to answer with."""
    if event["type"] != "invoice.paid":
        return 200
    key = "event:%s:%s" % (event["id"], event["delivery"])
    if store.get(key):
        return 200
    ledger.credit(event["data"]["account"], event["data"]["amount_cents"])
    store.put(key, True)
    return 200
