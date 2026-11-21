"""Reading uploaded CSV exports."""

import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "third_party"))

import fastcsv  # noqa: E402


def read_rows(text):
    return fastcsv.parse(text)
