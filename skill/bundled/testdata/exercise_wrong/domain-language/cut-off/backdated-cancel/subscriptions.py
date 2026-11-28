from datetime import date


def has_access(sub, today):
    if sub["status"] == "cancelled":
        return today <= sub["period_end"]
    return sub["status"] == "active"


def cancel(sub):
    sub["status"] = "cancelled"


def cut_off(sub):
    cancel(sub)
    sub["period_end"] = date.min
