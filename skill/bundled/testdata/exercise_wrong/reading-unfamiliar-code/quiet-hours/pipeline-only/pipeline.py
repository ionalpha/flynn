import datetime

import dispatch
import templates


def _quiet(user, msg):
    window = user.get("quiet")
    if not window or msg["priority"] == "urgent":
        return False
    at = datetime.datetime.strptime(msg["at"], "%Y-%m-%dT%H:%MZ")
    hour = (at + datetime.timedelta(minutes=user.get("utc_offset_min", 0))).hour
    start, end = window
    return start <= hour < end if start < end else hour >= start or hour < end


def process(users, event, out):
    user = users[event["user"]]
    msg = {
        "text": templates.TEXT[event["kind"]].format(**event),
        "at": event["at"],
        "priority": event.get("priority", "normal"),
    }
    for channel in user["channels"]:
        if channel in ("mobile", "watch") and _quiet(user, msg):
            continue
        line = dispatch.send(channel, user, msg)
        if line is not None:
            out.append(line)
