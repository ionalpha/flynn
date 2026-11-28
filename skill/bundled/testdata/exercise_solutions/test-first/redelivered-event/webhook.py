def handle(event, ledger, seen):
    """Apply one payment event. True tells the provider to stop sending it."""
    if event["id"] in seen:
        return True
    ledger.credit(event["account"], event["cents"])
    seen.add(event["id"])
    return True
