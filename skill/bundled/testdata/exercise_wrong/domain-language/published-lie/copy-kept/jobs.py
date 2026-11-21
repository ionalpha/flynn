import sessions


def nightly(now):
    return sessions.archive_old_sessions(now)
