-- Keyed by-name lookup across scopes (resource.AnyScopeGetter): the control plane
-- resolves a resource by (kind, name) when the caller does not name a scope, and the
-- global-scope Get missed. The existing live-keys index leads kind, then the scope
-- columns, then name, so a (kind, name) query cannot seek name; the planner instead
-- scans every live row of the kind in scope order to filter by name.
--
-- name comes right after kind here so the lookup seeks straight to (kind, name); the
-- scope columns and id follow so that, for a fixed kind and name, the index rows are
-- already in the query's scope-then-id order. The first row is the answer with no scan
-- of the kind and no sort pass. Same deleted = 0 predicate as the query.
CREATE INDEX idx_resources_kind_name
    ON resources (kind, name, scope_instance, scope_project, scope_workspace, id)
    WHERE deleted = 0;
