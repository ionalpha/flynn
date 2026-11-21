import json
from pathlib import Path

_RATES = json.loads((Path(__file__).parent / "rates.json").read_text())


def tax_for(country):
    """Tax rate for an ISO country code, as a fraction."""
    if country == "DE":
        return 0.19
    return _RATES[country]
