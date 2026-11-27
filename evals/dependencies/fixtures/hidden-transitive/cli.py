def render(items):
    """Text printed by `jobs list`. Each item has name, owner and state."""
    return "\n".join("%s %s %s" % (i["name"], i["owner"], i["state"]) for i in items)
