-- Usage records: one row per model call, as the state package's UsageStore
-- defines them.
--
-- The row is keyed by (run_id, call_id) because that pair is what a collector
-- knows when it retries. Storing the same record twice must not count a call
-- twice, and the primary key is what makes a second insert of a key impossible
-- to mistake for a new call.
--
-- The columns beside the key exist to be queried: run and parent run for the
-- spawn tree, principal for who the call was made for, provider and model for
-- what a call was made on, and at for a time window. The whole record is also
-- kept as JSON in record, and every read returns that and not the columns. A
-- field added to the record later then survives a round trip without a
-- migration, and the columns can never disagree with what was stored, because
-- they are never what is returned.
--
-- at uses the same fixed-width UTC text as the other timestamp columns (see
-- 0029), so string order is time order and a range predicate is correct.
CREATE TABLE IF NOT EXISTS model_usage (
	run_id        TEXT NOT NULL,
	call_id       TEXT NOT NULL,
	parent_run_id TEXT NOT NULL DEFAULT '',
	principal     TEXT NOT NULL DEFAULT '',
	provider      TEXT NOT NULL,
	model         TEXT NOT NULL,
	at            TEXT NOT NULL,
	record        TEXT NOT NULL,
	PRIMARY KEY (run_id, call_id)
) WITHOUT ROWID;

-- Walking down a spawn tree finds the children of each run in turn.
CREATE INDEX IF NOT EXISTS idx_model_usage_parent ON model_usage (parent_run_id);

-- A time window, read in the order the store returns it.
CREATE INDEX IF NOT EXISTS idx_model_usage_at ON model_usage (at, call_id, run_id);

-- One principal's records over a time window.
CREATE INDEX IF NOT EXISTS idx_model_usage_principal ON model_usage (principal, at);

-- Spend by provider and model over time.
CREATE INDEX IF NOT EXISTS idx_model_usage_model ON model_usage (provider, model, at);
