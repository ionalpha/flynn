import db


def workspace_members(workspace_id):
    rows = db.CONN.execute(
        "SELECT email FROM members WHERE workspace_id = ? AND active = 1 ORDER BY email", (workspace_id,)
    )
    return [r[0] for r in rows]


def workspace_name(workspace_id):
    row = db.CONN.execute("SELECT name FROM workspaces WHERE workspace_id = ?", (workspace_id,)).fetchone()
    return row[0]


def rename_workspace(workspace_id, name):
    db.CONN.execute("UPDATE workspaces SET name = ? WHERE workspace_id = ?", (name, workspace_id))
    db.CONN.commit()
