from textmeasure import width


def render(items):
    """Text printed by `jobs list`. Each item has name, owner and state."""
    rows = [(i["name"], i["owner"], i["state"]) for i in items]
    sizes = [max(width(r[c]) for r in rows) for c in range(3)]
    return "\n".join("  ".join(v + " " * (sizes[c] - width(v)) for c, v in enumerate(r)) for r in rows)
