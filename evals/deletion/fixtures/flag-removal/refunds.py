from checkout import round_half_up


def partial_refund(paid, percent):
    return round_half_up(paid * percent, 100)
