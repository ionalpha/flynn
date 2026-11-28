import promos
import rates
import zones


def quote(order):
    weight = sum(item["kg"] for item in order["items"])
    zone = zones.zone_for(order["postcode"])
    charge = rates.base_rate(zone, weight)
    return charge * (100 - promos.percent_off(order.get("promo"))) // 100
