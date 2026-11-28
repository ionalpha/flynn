def handle(event, ledger, seen):
    """Apply one payment event. True tells the provider to stop sending it."""
    ledger.credit(event["account"], event["cents"])
    return True
