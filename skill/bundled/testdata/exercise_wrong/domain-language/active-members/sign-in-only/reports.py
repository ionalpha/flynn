from datetime import timedelta


def disabled_member_count(members, org):
    return sum(1 for m in members if m["org"] == org and not m["active"])


def active_member_count(members, org, today):
    since = today - timedelta(days=30)
    return sum(
        1
        for m in members
        if m["org"] == org and m["last_sign_in"] is not None and m["last_sign_in"] >= since
    )
