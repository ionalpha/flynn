import dispatch
import templates


def process(users, event, out):
    user = users[event["user"]]
    msg = {
        "text": templates.TEXT[event["kind"]].format(**event),
        "at": event["at"],
        "priority": event.get("priority", "normal"),
    }
    for channel in user["channels"]:
        line = dispatch.send(channel, user, msg)
        if line is not None:
            out.append(line)
