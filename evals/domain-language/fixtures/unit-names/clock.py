import time

_sleep = time.sleep


def sleep_ms(ms):
    _sleep(ms / 1000)
