CREATE TABLE sessions (
    session_id TEXT PRIMARY KEY,
    active_task_id TEXT NOT NULL DEFAULT '',
    updated_at INTEGER NOT NULL
);

CREATE TABLE sources (
    id TEXT PRIMARY KEY,
    origin TEXT NOT NULL,
    location TEXT NOT NULL,
    excerpt TEXT NOT NULL,
    created_at INTEGER NOT NULL
);

CREATE TABLE memories (
    id TEXT PRIMARY KEY,
    kind TEXT NOT NULL DEFAULT 'fact',
    state TEXT NOT NULL DEFAULT 'confirmed',
    enabled INTEGER NOT NULL DEFAULT 1,
    version INTEGER NOT NULL,
    fingerprint TEXT,
    strength REAL NOT NULL DEFAULT 1,
    reinforced_at INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX memories_fingerprint ON memories(fingerprint);
CREATE INDEX memories_active ON memories(enabled, state);

CREATE TABLE memory_versions (
    memory_id TEXT NOT NULL REFERENCES memories(id),
    version INTEGER NOT NULL,
    source_id TEXT NOT NULL REFERENCES sources(id),
    summary TEXT NOT NULL,
    details TEXT NOT NULL DEFAULT '',
    tags TEXT NOT NULL DEFAULT '[]',
    conditions TEXT NOT NULL DEFAULT '',
    expires_at INTEGER NOT NULL DEFAULT 0,
    feedback_id TEXT REFERENCES feedback(id),
    created_at INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (memory_id, version)
);

CREATE TABLE memory_embeddings (
    memory_id TEXT NOT NULL,
    version INTEGER NOT NULL,
    model TEXT NOT NULL,
    dims INTEGER NOT NULL,
    vector BLOB NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (memory_id, version, model),
    FOREIGN KEY (memory_id, version) REFERENCES memory_versions(memory_id, version)
);
CREATE INDEX memory_embeddings_model ON memory_embeddings(model);

CREATE TABLE deliveries (
    reference TEXT PRIMARY KEY,
    session_id TEXT NOT NULL,
    task_id TEXT NOT NULL,
    memory_id TEXT NOT NULL,
    version INTEGER NOT NULL,
    strategy TEXT NOT NULL,
    expires INTEGER NOT NULL,
    revoked INTEGER NOT NULL DEFAULT 0,
    FOREIGN KEY (memory_id, version) REFERENCES memory_versions(memory_id, version)
);
CREATE INDEX delivery_lookup ON deliveries(session_id, task_id, memory_id);
CREATE INDEX delivery_session_active ON deliveries(session_id, revoked);
CREATE INDEX delivery_expires ON deliveries(expires);

CREATE TABLE formation (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL,
    task_id TEXT NOT NULL,
    origin TEXT NOT NULL,
    location TEXT NOT NULL DEFAULT '',
    raw_text TEXT NOT NULL DEFAULT '',
    draft_json TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL DEFAULT 'pending',
    memory_ids TEXT NOT NULL DEFAULT '[]',
    error TEXT NOT NULL DEFAULT '',
    received_at INTEGER NOT NULL
);
CREATE INDEX formation_pending ON formation(received_at) WHERE state = 'pending';

CREATE TABLE transcript_cursors (
    session_id TEXT PRIMARY KEY,
    transcript_path TEXT NOT NULL,
    last_offset INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL
);

CREATE TABLE feedback (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL,
    request_key TEXT NOT NULL,
    reference TEXT NOT NULL REFERENCES deliveries(reference),
    kind TEXT NOT NULL,
    reason TEXT NOT NULL,
    evidence TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL DEFAULT 'pending',
    attempts INTEGER NOT NULL DEFAULT 0,
    next_attempt INTEGER NOT NULL DEFAULT 0,
    policy_version INTEGER NOT NULL DEFAULT 0,
    UNIQUE (session_id, request_key)
);
CREATE INDEX feedback_pending ON feedback(id) WHERE state = 'pending';

CREATE TABLE feedback_effects (
    memory_id TEXT NOT NULL REFERENCES memories(id),
    task_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (memory_id, task_id, kind)
);

CREATE TABLE feedback_summary (
    memory_id TEXT NOT NULL REFERENCES memories(id),
    kind TEXT NOT NULL,
    category TEXT NOT NULL,
    count INTEGER NOT NULL,
    last_at INTEGER NOT NULL,
    PRIMARY KEY (memory_id, kind, category)
);

CREATE TABLE observations (
    id TEXT PRIMARY KEY,
    kind TEXT NOT NULL,
    base_version INTEGER NOT NULL,
    task_id TEXT NOT NULL,
    memory_id TEXT NOT NULL DEFAULT '',
    evidence TEXT NOT NULL,
    trusted INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    UNIQUE (kind, base_version, task_id)
);
CREATE INDEX observations_lookup ON observations(base_version, trusted, created_at);

CREATE TABLE proposals (
    id TEXT PRIMARY KEY,
    base_version INTEGER NOT NULL,
    kind TEXT NOT NULL,
    target_type TEXT NOT NULL,
    target_key TEXT NOT NULL,
    before_val REAL NOT NULL DEFAULT 0,
    after_val REAL NOT NULL DEFAULT 0,
    before_text TEXT NOT NULL DEFAULT '',
    after_text TEXT NOT NULL DEFAULT '',
    diagnosis TEXT NOT NULL DEFAULT '',
    evidence TEXT NOT NULL DEFAULT '[]',
    impact TEXT NOT NULL DEFAULT '',
    risk TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'pending',
    actor TEXT NOT NULL DEFAULT '',
    resolved_at INTEGER NOT NULL DEFAULT 0,
    published INTEGER NOT NULL DEFAULT 0,
    UNIQUE (base_version, kind, target_type, target_key)
);

CREATE TABLE audit (
    id INTEGER PRIMARY KEY,
    at INTEGER NOT NULL,
    operation TEXT NOT NULL,
    session_id TEXT NOT NULL,
    task_id TEXT NOT NULL,
    strategy TEXT NOT NULL,
    trigger_event TEXT NOT NULL,
    result TEXT NOT NULL,
    reference TEXT NOT NULL,
    version INTEGER NOT NULL,
    summary TEXT NOT NULL,
    model_outcome TEXT NOT NULL DEFAULT '',
    model_ms INTEGER NOT NULL DEFAULT 0,
    match_context TEXT NOT NULL DEFAULT ''
);
CREATE INDEX audit_task_op ON audit(task_id, operation, result);
