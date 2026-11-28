"""Ticket storage: one JSON file holding a list of ticket dicts."""
import json
from pathlib import Path


def load(path):
    p = Path(path)
    if not p.exists():
        return []
    return json.loads(p.read_text(encoding="utf-8"))


def save(path, tickets):
    Path(path).write_text(json.dumps(tickets, indent=2) + "\n", encoding="utf-8")


def add(path, fields):
    """Store a new ticket with the next id and return it."""
    tickets = load(path)
    ticket = dict(fields, id=len(tickets) + 1)
    tickets.append(ticket)
    save(path, tickets)
    return ticket
