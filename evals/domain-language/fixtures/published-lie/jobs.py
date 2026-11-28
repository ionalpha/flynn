import sessions


def nightly(now):
    return sessions.purge_old_sessions(now)
