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
    -- The panel's last full refresh, so the 3-minute floor survives a
    -- panel restart (8.4).
    last_full_at DATETIME,
    -- Set once the menu has been edited: until then the defaults apply.
    menu_edited INTEGER NOT NULL DEFAULT 0,
    -- Bumped on every menu edit, so a panel acting on an old view of the
    -- menu is refused.
    menu_rev INTEGER NOT NULL DEFAULT 0,
    -- The detection wizard gave up: it runs again only on "Detect again",
    -- not on every panel restart (each pass refreshes the panel).
    detect_gave_up INTEGER NOT NULL DEFAULT 0,
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
