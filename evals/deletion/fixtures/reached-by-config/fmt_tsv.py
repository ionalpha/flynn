def render(rows):
    return "\n".join("\t".join(str(c) for c in row) for row in rows)
