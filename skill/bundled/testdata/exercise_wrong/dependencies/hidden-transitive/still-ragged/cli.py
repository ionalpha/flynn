def render(items):
    """Text printed by `jobs list`. Each item has name, owner and state."""
    lines = ["NAME\tOWNER\tSTATE"] + ["%s\t%s\t%s" % (i["name"], i["owner"], i["state"]) for i in items]
    return "\n".join(lines)
