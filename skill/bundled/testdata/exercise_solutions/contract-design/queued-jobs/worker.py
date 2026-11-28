import gateway


def handle(job):
    # Jobs queued before currencies existed have no field, and they were all dollars.
    gateway.charge(job["customer"], job["amount_cents"], job.get("currency", "usd").lower())
