-- Race config files (phase 12a): the event page, and graywolf's settings
-- as they were before a race config changed them, for "Restore graywolf
-- settings". Node data, kept by Reset (the event page goes with the race's
-- reference data).
CREATE TABLE event_page (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    text TEXT NOT NULL DEFAULT '',
    updated_at DATETIME NOT NULL
);
CREATE TABLE graywolf_backup (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    data TEXT NOT NULL,
    saved_at DATETIME NOT NULL
);
