def has_access(sub, today):
    if sub["status"] == "cancelled":
        return today <= sub["period_end"]
    return sub["status"] == "active"


def cancel(sub):
    sub["status"] = "cancelled"
