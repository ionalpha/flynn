"""The people and businesses who buy from the shop."""

from dataclasses import dataclass


@dataclass
class Customer:
    id: str
    name: str


CUSTOMERS = {
    "c1": Customer("c1", "Ines Moreau"),
    "c2": Customer("c2", "Harbour Cafe"),
    "c3": Customer("c3", "Tomas Berg"),
}


def find_customer(customer_id):
    return CUSTOMERS[customer_id]
