-- A checkpoint's close time, from its heartbeats' closed flag (4.7).
ALTER TABLE cp_status ADD COLUMN closed_at DATETIME;

-- Node panel (8.4): e-ink display settings and the button menu. Node
-- configuration, kept by Reset.
CREATE TABLE panel_settings (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    enabled INTEGER NOT NULL DEFAULT 1,
    controller TEXT NOT NULL DEFAULT '',
    refresh_min INTEGER NOT NULL DEFAULT 5,
    rotation INTEGER NOT NULL DEFAULT 0,
    updated_at DATETIME NOT NULL
);

CREATE TABLE panel_menu (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    position INTEGER NOT NULL,
    label TEXT NOT NULL,
    action TEXT NOT NULL,
    confirm INTEGER NOT NULL DEFAULT 0,
    roles TEXT NOT NULL DEFAULT 'both',
    enabled INTEGER NOT NULL DEFAULT 1
);
