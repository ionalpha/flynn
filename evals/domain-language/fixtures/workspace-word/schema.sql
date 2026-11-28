CREATE TABLE workspaces (
    workspace_id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    seat_limit INTEGER NOT NULL
);

CREATE TABLE members (
    member_id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    email TEXT NOT NULL,
    active INTEGER NOT NULL
);
