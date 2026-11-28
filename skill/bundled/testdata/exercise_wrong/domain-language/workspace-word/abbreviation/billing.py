import db

LEDGER = {}


def charge_account(account_id, cents):
    LEDGER[account_id] = LEDGER.get(account_id, 0) + cents
    return LEDGER[account_id]


def account_plan(account_id):
    row = db.CONN.execute(
        "SELECT name, seat_limit FROM workspaces WHERE workspace_id = ?", (account_id,)
    ).fetchone()
    return {"name": row[0], "seat_limit": row[1]}


def seats_left(ws_id):
    limit = db.CONN.execute(
        "SELECT seat_limit FROM workspaces WHERE workspace_id = ?", (ws_id,)
    ).fetchone()[0]
    used = db.CONN.execute(
        "SELECT COUNT(*) FROM members WHERE workspace_id = ? AND active = 1", (ws_id,)
    ).fetchone()[0]
    return limit - used
