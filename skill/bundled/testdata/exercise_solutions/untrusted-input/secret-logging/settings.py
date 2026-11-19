from secret import Secret


class Config:
    """Settings for the payments client, read from the process environment."""

    def __init__(self, env):
        self.service_url = env.get("PAYMENTS_URL", "https://pay.example")
        self.api_key = Secret(env["PAYMENTS_KEY"])
        self.timeout = 5
