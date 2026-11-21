import promos
import rates
import zones

FREE_SHIPPING_CENTS = 10000


def quote(order):
    if sum(item["cents"] for item in order["items"]) >= FREE_SHIPPING_CENTS:
        return 0
    weight = sum(item["kg"] for item in order["items"])
    zone = zones.zone_for(order["postcode"])
    charge = rates.base_rate(zone, weight)
    return charge * (100 - promos.percent_off(order.get("promo"))) // 100
