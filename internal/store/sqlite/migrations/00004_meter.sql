-- +goose Up
-- The billing meter (docs/decisions/billing.md): what each billed object
-- held, a row per item and Unix minute, in the item's own unit. It is
-- outside objects and events, so neither a watch nor the revision sees it.
CREATE TABLE meter_minutes (
    tenant   TEXT    NOT NULL,
    uid      TEXT    NOT NULL,
    item     TEXT    NOT NULL,
    minute   INTEGER NOT NULL,
    quantity INTEGER NOT NULL,
    kind     TEXT    NOT NULL,
    project  TEXT    NOT NULL,
    name     TEXT    NOT NULL,
    PRIMARY KEY (tenant, uid, item, minute)
) WITHOUT ROWID;

CREATE INDEX meter_minutes_tenant_minute ON meter_minutes (tenant, minute);

-- Minutes folded into hours once billed: quantity is the sum of the hour's
-- minutes, and hour, in Unix minutes, is the hour's first.
CREATE TABLE meter_hours (
    tenant   TEXT    NOT NULL,
    uid      TEXT    NOT NULL,
    item     TEXT    NOT NULL,
    hour     INTEGER NOT NULL,
    quantity INTEGER NOT NULL,
    kind     TEXT    NOT NULL,
    project  TEXT    NOT NULL,
    name     TEXT    NOT NULL,
    PRIMARY KEY (tenant, uid, item, hour)
) WITHOUT ROWID;

CREATE INDEX meter_hours_tenant_hour ON meter_hours (tenant, hour);

-- +goose Down
DROP TABLE meter_hours;
DROP TABLE meter_minutes;
