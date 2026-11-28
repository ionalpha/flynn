"""Errors from the card gateway."""

MESSAGES = {
    "timeout": "The payment is taking longer than usual. Please try again.",
    "declined": "The card was declined.",
}

RETRYABLE = {"timeout"}


class GatewayError(Exception):
    """A gateway failure. code is the stable kind; the text is for customers only."""

    def __init__(self, code, message):
        super().__init__(message)
        self.code = code
        self.retryable = code in RETRYABLE


def gateway_error(kind):
    return GatewayError(kind, MESSAGES[kind])
