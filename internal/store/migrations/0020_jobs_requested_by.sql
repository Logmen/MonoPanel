-- An ordinary account lists only the jobs it asked for (GET /jobs filters by
-- requested_by in the query): the index keeps that from reading the whole
-- table, logs included, on a server with years of jobs.
CREATE INDEX jobs_requested_by ON jobs(requested_by, id);
