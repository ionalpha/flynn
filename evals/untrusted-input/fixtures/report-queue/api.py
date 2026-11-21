"""What the web app calls when a signed-in customer asks for a report."""

import queue_store


def submit_report(user, params):
    title = params.get("title", "").strip()
    if not title:
        raise ValueError("title is required")
    if len(title) > 120:
        raise ValueError("title is too long")
    queue_store.put({"kind": "report", "owner": user, "title": title, "body": params.get("body", "")})
