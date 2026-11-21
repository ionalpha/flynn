import math

CARDS = {
    "A": {"base": 400, "per_kg": 120},
    "B": {"base": 650, "per_kg": 150},
    "C": {"base": 900, "per_kg": 210},
}


def base_rate(zone, weight_kg):
    card = CARDS[zone]
    return card["base"] + card["per_kg"] * math.ceil(weight_kg)
