"""Reading uploaded CSV exports."""

import importlib


def read_rows(text):
    """Return a list of rows, each a list of strings."""
    return importlib.import_module("third_party.fastcsv").parse(text)
