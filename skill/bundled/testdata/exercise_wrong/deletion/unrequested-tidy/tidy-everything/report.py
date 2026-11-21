import json



def summarise(rows):
    total = sum(r["qty"] * r["price"] for r in rows)
    return {"count": len(rows), "total": total}


def render(rows):
    s = summarise(rows)
    return json.dumps(s, sort_keys=True)
