import config

EXPORT_MAX_ROWS = 500


def export(rows, env=None):
    limit = config.load(env)["export"]["max_rows"]
    return rows[:limit]
