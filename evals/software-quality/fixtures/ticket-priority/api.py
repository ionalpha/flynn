"""Request handlers. Each takes the parsed JSON body and returns (status, body)."""
import datetime

import store


def create_ticket(path, payload):
    title = payload.get("title")
    if not isinstance(title, str) or not title.strip():
        return 400, {"error": "title is required"}
    ticket = store.add(
        path,
        {
            "title": title.strip(),
            "status": "open",
            "opened": datetime.date.today().isoformat(),
        },
    )
    return 201, ticket


def list_tickets(path, status=None):
    tickets = store.load(path)
    if status is not None:
        tickets = [t for t in tickets if t["status"] == status]
    return 200, tickets
