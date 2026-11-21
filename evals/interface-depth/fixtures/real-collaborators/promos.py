PERCENT_OFF = {"SPRING": 10, "FRIEND": 25}


def percent_off(code):
    if not code:
        return 0
    return PERCENT_OFF.get(code.upper(), 0)
