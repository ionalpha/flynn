def render(order):
    lines = ["Receipt " + order["id"], "Paid by " + order["method"]]
    lines.append("Total: %d" % order["total"])
    return "\n".join(lines)
