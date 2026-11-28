import sessions


def weekly(now):
    return sessions.archive_old_sessions(now, days=14)
