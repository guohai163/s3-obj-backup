CREATE TABLE connections (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  name text NOT NULL UNIQUE,
  endpoint text NOT NULL,
  region text NOT NULL,
  path_style boolean NOT NULL,
  credentials bytea NOT NULL
);
CREATE TABLE buckets (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  connection_id bigint NOT NULL REFERENCES connections,
  name text NOT NULL,
  generation bigint NOT NULL DEFAULT 0,
  indexed_at timestamptz,
  object_count bigint NOT NULL DEFAULT 0,
  UNIQUE (connection_id, name)
);
CREATE TABLE tasks (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  name text NOT NULL UNIQUE,
  kind text NOT NULL CHECK (kind IN ('backup','cleanup','index')),
  source_id bigint NOT NULL REFERENCES buckets,
  target_id bigint REFERENCES buckets,
  settings jsonb NOT NULL,
  enabled boolean NOT NULL DEFAULT false,
  next_run timestamptz,
  revision bigint NOT NULL DEFAULT 1
);
CREATE INDEX tasks_due ON tasks (next_run) WHERE enabled;
CREATE TABLE runs (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  task_id bigint NOT NULL REFERENCES tasks,
  revision bigint NOT NULL,
  mode text NOT NULL CHECK (mode IN ('manual','scheduled','preview')),
  plan_id bigint UNIQUE REFERENCES runs,
  state text NOT NULL DEFAULT 'queued' CHECK (state IN ('queued','running','success','failed')),
  created_at timestamptz NOT NULL DEFAULT now(),
  started_at timestamptz,
  finished_at timestamptz,
  scanned bigint NOT NULL DEFAULT 0,
  copied bigint NOT NULL DEFAULT 0,
  skipped bigint NOT NULL DEFAULT 0,
  deleted bigint NOT NULL DEFAULT 0,
  matched bigint NOT NULL DEFAULT 0,
  detail text NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX one_active_run ON runs (task_id) WHERE state IN ('queued','running');
CREATE INDEX runs_queue ON runs (id) WHERE state = 'queued';
CREATE TABLE inventory (
  bucket_id bigint NOT NULL REFERENCES buckets ON DELETE CASCADE,
  generation bigint NOT NULL,
  object_key text COLLATE "C" NOT NULL,
  size bigint NOT NULL,
  modified timestamptz NOT NULL,
  PRIMARY KEY (bucket_id, generation, object_key)
);
CREATE TABLE copies (
  task_id bigint NOT NULL REFERENCES tasks,
  object_key text NOT NULL,
  source_etag text NOT NULL,
  source_size bigint NOT NULL,
  source_modified timestamptz NOT NULL,
  target_etag text NOT NULL,
  target_modified timestamptz NOT NULL,
  PRIMARY KEY (task_id, object_key)
);
CREATE TABLE candidates (
  run_id bigint NOT NULL REFERENCES runs ON DELETE CASCADE,
  object_key text COLLATE "C" NOT NULL,
  etag text NOT NULL,
  size bigint NOT NULL,
  modified timestamptz NOT NULL,
  PRIMARY KEY (run_id, object_key)
);
CREATE TABLE sessions (
  token_hash bytea PRIMARY KEY,
  role text NOT NULL CHECK (role IN ('admin','viewer')),
  expires_at timestamptz NOT NULL
);
CREATE INDEX session_expiry ON sessions (expires_at);
