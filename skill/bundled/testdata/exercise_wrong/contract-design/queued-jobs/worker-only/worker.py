import gateway


def handle(job):
    gateway.charge(job["customer"], job["amount_cents"], job.get("currency", "usd"))
