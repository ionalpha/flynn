def render(items):
    """Text printed by `jobs list`. Each item has name, owner and state."""
    rows = [("NAME", "OWNER", "STATE")] + [(i["name"], i["owner"], i["state"]) for i in items]
    sizes = [max(len(r[c]) for r in rows) for c in range(3)]
    return "\n".join("  ".join(v.ljust(sizes[c]) for c, v in enumerate(r)).rstrip() for r in rows)
