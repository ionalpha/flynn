"""Push delivery. Every push notification goes through send_push."""
import datetime


def send_push(user, msg):
    window = user.get("quiet")
    if window and msg["priority"] != "urgent":
        at = datetime.datetime.strptime(msg["at"], "%Y-%m-%dT%H:%MZ")
        hour = (at + datetime.timedelta(minutes=user.get("utc_offset_min", 0))).hour
        start, end = window
        if hour >= start or hour < end:
            return None
    return "mobile %s %s" % (user["name"], msg["text"])
