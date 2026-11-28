ZONES = {"10": "A", "11": "A", "20": "B", "21": "B", "30": "C"}
DEFAULT_ZONE = "C"


def zone_for(postcode):
    return ZONES.get(postcode[:2], DEFAULT_ZONE)
