ORDER = ["new", "paid", "shipped", "delivered", "cancelled"]


def counts(orders):
    result = {s: 0 for s in ORDER}
    for o in orders:
        result[o["status"]] = result.get(o["status"], 0) + 1
    return result


def lines(orders):
    c = counts(orders)
    return ["%s %d" % (s, c[s]) for s in ORDER]
