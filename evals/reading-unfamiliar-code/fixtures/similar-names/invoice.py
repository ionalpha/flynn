from formatting import display_amount


def render(order):
    lines = ["Invoice " + order["id"], "Customer " + order["customer"]]
    lines.append("Total: " + display_amount(order["total"], order["currency"]))
    return "\n".join(lines)
