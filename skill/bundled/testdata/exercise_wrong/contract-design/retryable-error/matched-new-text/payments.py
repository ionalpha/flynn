"""Errors from the card gateway."""

MESSAGES = {
    "timeout": "The payment is taking longer than usual. Please try again.",
    "declined": "The card was declined.",
}


class GatewayError(Exception):
    pass


def gateway_error(kind):
    return GatewayError(MESSAGES[kind])
