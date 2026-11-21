def start(env, transport):
    """Build the payments client; called once when the service boots."""
    from client import Client
    from settings import Config

    return Client(Config(env), transport)
