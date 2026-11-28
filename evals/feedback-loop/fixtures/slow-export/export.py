"""Rows for the nightly people export."""
from lookup import canonical


def export_row(name):
    """CSV-safe bytes for a person's name, spelled as the directory spells it."""
    return canonical(name).encode("ascii", "replace")
