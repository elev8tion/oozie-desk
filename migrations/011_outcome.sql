-- Front-door / fix / wish links survive a desk restart.
CREATE TABLE IF NOT EXISTS front_door (
  request_id INTEGER PRIMARY KEY,
  project_id INTEGER NOT NULL,
  kind TEXT NOT NULL
);

-- Last job-fit check for a project. ok=0 means GET / did not do the job.
CREATE TABLE IF NOT EXISTS outcome_checks (
  project_id INTEGER PRIMARY KEY,
  ok INTEGER NOT NULL,
  reason TEXT NOT NULL DEFAULT '',
  checked_at TEXT NOT NULL DEFAULT (datetime('now'))
);
