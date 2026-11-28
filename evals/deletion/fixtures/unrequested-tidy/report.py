import json
import os
import sys

# def old_format(rows):
#     return "\n".join("%s x%d" % (r["name"], r["qty"]) for r in rows)


def summarise(rows):
    print("DEBUG rows:", rows)
    total = sum(r["qty"] * r["price"] for r in rows)
    return {"count": len(rows), "total": total}


def render(rows):
    s = summarise(rows)
    return json.dumps(s, sort_keys=True)
