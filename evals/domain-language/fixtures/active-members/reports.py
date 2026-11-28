def disabled_member_count(members, org):
    return sum(1 for m in members if m["org"] == org and not m["active"])
