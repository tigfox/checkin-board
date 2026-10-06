-- Status-board branding (spec 8.3). HQ data; never sent over RF.
CREATE TABLE branding (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    header_text TEXT NOT NULL DEFAULT '',
    footer_text TEXT NOT NULL DEFAULT '',
    color_primary TEXT NOT NULL DEFAULT '',
    color_accent TEXT NOT NULL DEFAULT '',
    color_background TEXT NOT NULL DEFAULT '',
    color_text TEXT NOT NULL DEFAULT '',
    updated_at DATETIME NOT NULL
);

-- The logo, already decoded, scaled and re-encoded as PNG.
CREATE TABLE branding_logo (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    image BLOB NOT NULL,
    content_type TEXT NOT NULL,
    width INTEGER NOT NULL,
    height INTEGER NOT NULL,
    sha256 TEXT NOT NULL,
    updated_at DATETIME NOT NULL
);
