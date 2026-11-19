"""Reading uploaded CSV exports."""

import csv
import io


def _fc_scan(text):
    return [row for row in csv.reader(io.StringIO(text))]


def read_rows(text):
    return _fc_scan(text)
