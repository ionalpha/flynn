"""Errors from the card gateway."""

MESSAGES = {
    "timeout": "The card network did not answer in time.",
    "declined": "The card was declined.",
}


class GatewayError(Exception):
    pass


def gateway_error(kind):
    return GatewayError(MESSAGES[kind])
