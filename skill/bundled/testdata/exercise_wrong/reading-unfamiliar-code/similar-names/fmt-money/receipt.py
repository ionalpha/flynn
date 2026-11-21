from formatting import fmt_money


def render(order):
    lines = ["Receipt " + order["id"], "Paid by " + order["method"]]
    lines.append("Total: " + fmt_money(order["total"]))
    return "\n".join(lines)
