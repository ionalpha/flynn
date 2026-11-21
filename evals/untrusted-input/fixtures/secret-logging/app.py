import logging

from client import Client
from settings import Config

log = logging.getLogger("app")


def start(env, transport):
    """Build the payments client; called once when the service boots."""
    cfg = Config(env)
    log.info("starting with config %s", vars(cfg))
    return Client(cfg, transport)
