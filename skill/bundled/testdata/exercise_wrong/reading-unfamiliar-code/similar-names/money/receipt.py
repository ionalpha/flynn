from formatting import money


def render(order):
    lines = ["Receipt " + order["id"], "Paid by " + order["method"]]
    lines.append("Total: " + money(order["total"], order["currency"]))
    return "\n".join(lines)
