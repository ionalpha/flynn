-- Ownership graph for resources: an owner_references list links a resource to the
-- resources that own it (its parent run or goal). The controller owner drives the
-- resource's lifecycle, so a garbage collector reaps a resource once that owner is
-- gone or terminating, cascading a delete to the subtree it created. The column is
-- envelope metadata, excluded from the content hash. Existing rows default to no
-- owners (a root resource).
ALTER TABLE resources ADD COLUMN owner_references TEXT NOT NULL DEFAULT '[]'; -- JSON array of OwnerReference
