import db


def org_members(org_id):
    rows = db.CONN.execute(
        "SELECT email FROM members WHERE workspace_id = ? AND active = 1 ORDER BY email", (org_id,)
    )
    return [r[0] for r in rows]


def org_name(org_id):
    row = db.CONN.execute("SELECT name FROM workspaces WHERE workspace_id = ?", (org_id,)).fetchone()
    return row[0]


def rename_org(org_id, name):
    db.CONN.execute("UPDATE workspaces SET name = ? WHERE workspace_id = ?", (name, org_id))
    db.CONN.commit()
