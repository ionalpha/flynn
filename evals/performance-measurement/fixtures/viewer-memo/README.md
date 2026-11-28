# dashboard

`dashboard.render(viewer, project_id, db)` serves every page view. One server
process serves all customers.

- `db.tasks(project_id)` reads every task of a project and is slow.
- `db.version(project_id)` is cheap and rises on every change to the project.
- Tasks change through `db.update`, from this server and from others.
