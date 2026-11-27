def churned(subs):
    return sum(1 for s in subs if s["status"] == "cancelled")
