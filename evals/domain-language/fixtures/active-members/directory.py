def active_members(members, org):
    """The members the directory page lists."""
    return [m for m in members if m["org"] == org and m["active"]]
