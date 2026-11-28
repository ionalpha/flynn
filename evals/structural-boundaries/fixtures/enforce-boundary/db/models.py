from dataclasses import dataclass


@dataclass
class Row:
    sku: str
    cents: int
