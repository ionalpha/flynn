import config


def page(rows, size, env=None):
    size = min(size, config.load(env)["api"]["max_page"])
    return rows[:size]
