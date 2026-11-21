"""Customers of the shop."""

from dataclasses import dataclass


@dataclass
class Customer:
    id: str
    name: str
    post_code: str


def export_rows(customers):
    return [(c.id, c.name, c.post_code) for c in customers]


def position_in_list(customers, customer_id):
    return [c.id for c in customers].index(customer_id)
