import sessions


def weekly(now):
    return sessions.purge_old_sessions(now, days=14)
