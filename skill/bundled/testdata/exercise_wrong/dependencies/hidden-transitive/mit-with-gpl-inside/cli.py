from tabulon import table


def render(items):
    """Text printed by `jobs list`. Each item has name, owner and state."""
    return table([[i["name"], i["owner"], i["state"]] for i in items], ["NAME", "OWNER", "STATE"])
