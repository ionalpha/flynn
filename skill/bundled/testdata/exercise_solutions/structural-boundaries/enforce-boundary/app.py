from db.session import current_rates
from domain.pricing import price


def quote(lines):
    return price(lines, current_rates())
