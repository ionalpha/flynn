def handle(event, ledger, seen):
    """Apply one payment event. True tells the provider to stop sending it."""
    if event["id"] in seen:
        return True
    seen.add(event["id"])
    ledger.credit(event["account"], event["cents"])
    return True
